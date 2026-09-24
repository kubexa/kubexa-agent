package config

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// managedBlockKeys is the closed set of blocks the Kubexa platform may own
// for a cluster. It is deliberately NOT derived from the Config struct: the
// point of the set is that mutate.*, rbac and the two redact_secrets flags
// stay outside it no matter what the struct grows.
var managedBlockKeys = []string{
	"collect.logs",
	"collect.state",
	"collect.metrics",
	"query",
	"buffer",
}

// managedBlockAllowedKeys is the explicit allowlist of top-level keys each
// managed block may set. It is narrower than the struct on purpose: a block
// may only ever carry what the platform exposes to a cluster owner, and
// everything left off this list -- redact_secrets on collect.state and
// query, custom_endpoints/cadvisor/kube_state_metrics/kube_metrics on
// collect.metrics, spill_dir/max_disk_bytes/max_memory_bytes/flush_interval
// on buffer -- must not become settable just because the struct has a field
// for it.
//
// redact_secrets matters most: it is the cluster owner's guarantee that
// Secret values never leave their cluster, and that guarantee has to hold
// IN THE AGENT, not only in the platform's own route. The whole point of a
// second gate here is that the platform cannot open it from its own side.
//
// TestManagedBlockAllowedKeysCoversEveryManagedKey (blocks_internal_test.go)
// pins that every key in managedBlockKeys has an entry here, so a sixth
// block landing without one fails a test instead of silently reaching
// ValidateBlock's allowlist check as a permissive default.
var managedBlockAllowedKeys = map[string]map[string]bool{
	"collect.logs": {
		"enabled":            true,
		"follow":             true,
		"tail_lines":         true,
		"exclude_namespaces": true,
		"rules":              true,
	},
	"collect.state": {
		"enabled":       true,
		"resync_period": true,
		"rules":         true,
	},
	"collect.metrics": {
		"enabled":       true,
		"pod_interval":  true,
		"node_interval": true,
		"rules":         true,
	},
	"query": {
		"enabled": true,
		"rules":   true,
	},
	"buffer": {
		"batch_size": true,
	},
}

// managedBlockAppliesLive classifies every managed block by WHEN a running
// agent can put it into effect. It is a hand-written set, not a derivation,
// and that is the point: "live" is a claim about every consumer of a block
// re-reading EffectiveConfig() on each use, which no amount of reflection
// over the struct can check. A derivation that drifted would hand the
// platform a confident wrong answer, which is worse than the honest list.
//
//   - true (live): the agent applies the block without a restart. query is
//     swapped into the compiled policy holder on every applied set; buffer's
//     only settable key, batch_size, is re-read by the drain loop on every
//     batch.
//   - false (restart-only): the block is baked into a component at
//     CONSTRUCTION. The collectors are built once, from the config resolved
//     at boot, so collect.logs/state/metrics -- their rules, intervals,
//     resync_period and enabled flags alike -- only change when the process
//     restarts.
//
// TestEveryManagedBlockIsClassifiedByWhenItTakesEffect (blocks_test.go)
// pins this map against a literal, and
// TestHeartbeatRestartRequiredFollowsTheBlockClassification
// (internal/stream) pins that the heartbeat's restart_required follows it.
// Moving a block between the categories fails both until the reporting
// moves with it.
var managedBlockAppliesLive = map[string]bool{
	"collect.logs":    false,
	"collect.state":   false,
	"collect.metrics": false,
	"query":           true,
	"buffer":          true,
}

// ManagedBlockAppliesLive reports whether a running agent puts key's block
// into effect without a restart. ok is false for any key outside the managed
// set.
func ManagedBlockAppliesLive(key string) (live bool, ok bool) {
	live, ok = managedBlockAppliesLive[key]
	return live, ok
}

// RestartRequiredBlocks names every restart-only managed block whose value
// in effective differs from the one in built -- that is, every block the
// agent has ACCEPTED but cannot actually be running until it restarts. The
// result is in managedBlockKeys order and empty when the two agree.
//
// built is the config the long-lived components were constructed from, not
// the file config: once boot resolves a managed set before building the
// collectors, the two are different, and the file config would report a
// restart for a block the collectors already run.
//
// It compares VALUES, never the set of block keys present. The platform
// re-sends the whole managed set on every revision, so a key-based answer
// would demand a restart for every edit to any other block, and an operator
// who restarts and sees the flag come back has been told a falsehood twice.
//
// Both sides are normalized and compared through their YAML rendering
// rather than reflect.DeepEqual, and neither is a nicety: an ApplyBlocks
// result has been normalized and round-tripped, so against a base that has
// not been it differs by injected default rule IDs and by nil-versus-empty
// slices -- differences that mean nothing and would report a restart for
// every revision, forever. Normalizing here rather than demanding it of the
// caller keeps that failure out of reach of anyone holding a config this
// package did not hand them.
//
// A nil on either side answers nothing: before boot reports what it built,
// no component exists to be stale.
func RestartRequiredBlocks(built, effective *Config) []string {
	if built == nil || effective == nil {
		return nil
	}
	built, effective = normalizedCopy(built), normalizedCopy(effective)
	var out []string
	for _, key := range managedBlockKeys {
		if live := managedBlockAppliesLive[key]; live {
			continue
		}
		if !bytes.Equal(managedBlockYAML(built, key), managedBlockYAML(effective, key)) {
			out = append(out, key)
		}
	}
	return out
}

// normalizedCopy returns a normalized deep copy of cfg, leaving cfg itself
// untouched -- Normalize writes rule IDs in place, and the file config this
// is called on is the one value the agent keeps immutable so a released
// block has something to restore.
//
// On the (marshalling) error cloneConfig can return, it falls back to cfg
// itself rather than to nil: a comparison of two un-normalized configs can
// be noisy, but answering "nothing to restart" because a copy failed would
// be the silent wrong answer this whole field exists to stop.
func normalizedCopy(cfg *Config) *Config {
	clone, err := cloneConfig(cfg)
	if err != nil {
		return cfg
	}
	clone.Normalize()
	return clone
}

// managedBlockYAML renders key's block out of cfg for comparison. A key
// outside the managed set, and a value yaml cannot marshal, both render as
// nil -- two of which compare equal, so an unknown key reports no change
// rather than a spurious restart.
func managedBlockYAML(cfg *Config, key string) []byte {
	var v any
	switch key {
	case "collect.logs":
		v = cfg.Collect.Logs
	case "collect.state":
		v = cfg.Collect.State
	case "collect.metrics":
		v = cfg.Collect.Metrics
	case "query":
		v = cfg.Query
	case "buffer":
		v = cfg.Buffer
	default:
		return nil
	}
	out, err := yaml.Marshal(v)
	if err != nil {
		return nil
	}
	return out
}

// ManagedBlockKeys returns the closed set, in display order.
func ManagedBlockKeys() []string {
	out := make([]string, len(managedBlockKeys))
	copy(out, managedBlockKeys)
	return out
}

// IsManagedBlockKey reports whether key names a block the platform may own.
func IsManagedBlockKey(key string) bool {
	for _, k := range managedBlockKeys {
		if k == key {
			return true
		}
	}
	return false
}

// ManagedBlockPrototype returns a zero value of the struct behind key, so a
// tool can reflect over the block's shape without carrying its own copy of
// the key set. ok is false for any key outside the managed set.
func ManagedBlockPrototype(key string) (value any, ok bool) {
	switch key {
	case "collect.logs":
		return LogsCollectConfig{}, true
	case "collect.state":
		return StateCollectConfig{}, true
	case "collect.metrics":
		return MetricsCollectConfig{}, true
	case "query":
		return QueryConfig{}, true
	case "buffer":
		return BufferConfig{}, true
	}
	return nil, false
}

// ManagedBlockAllowedKeys returns key's allowlist as a sorted slice, so a
// tool can build against the actual gate instead of reconstructing it: a
// tool that reflected over ManagedBlockPrototype's struct alone would
// include every field the struct happens to have -- redact_secrets chief
// among them -- not only the ones the platform may set. ok is false for any
// key outside the managed set.
func ManagedBlockAllowedKeys(key string) (keys []string, ok bool) {
	allowed, ok := managedBlockAllowedKeys[key]
	if !ok {
		return nil, false
	}
	keys = make([]string, 0, len(allowed))
	for k := range allowed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, true
}

// checkAllowedKeys refuses any top-level key in body that key's allowlist
// does not name. It looks only at body's own top-level keys -- a key nested
// inside a rules entry belongs to the rule, not the block, and stays
// governed by UnknownKeys instead.
//
// A body that does not parse as a mapping is let through here: the later
// splice/unmarshal pass produces a clearer error naming the actual YAML
// problem.
func checkAllowedKeys(key string, body []byte) error {
	allowed := managedBlockAllowedKeys[key]
	var raw map[string]yaml.Node
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return nil
	}
	var refused []string
	for k := range raw {
		if !allowed[k] {
			refused = append(refused, k)
		}
	}
	if len(refused) == 0 {
		return nil
	}
	sort.Strings(refused)
	violations := make([]string, len(refused))
	for i, k := range refused {
		violations[i] = fmt.Sprintf("%q is not an editable key", k)
	}
	return fmt.Errorf("%s: %s", key, strings.Join(violations, "; "))
}

// checkEmptyRules refuses a `rules` key that is present but carries nothing.
// An empty list is ambiguous -- "no rules" and "use the defaults" are both
// readable -- and every consumer currently picks the widest reading:
// QueryRules inherits collect.state's rules at len()==0 (query.go:304), and
// normalize() refills an empty list with a permissive default for logs
// (collect.go:76-79) and state (collect.go:119-126). Omitting the key already
// means "unchanged" under the patch rule and `enabled: false` already means
// "collect nothing", so refusing this spelling loses nothing expressible.
//
// Two YAML spellings resolve to the same nil slice and the same widening:
// `rules: []` parses as a SequenceNode with no Content, and a bare `rules:`
// with no value parses as a null ScalarNode. Refusing only the sequence form
// would leave the quieter, easier-to-write-by-accident spelling open.
//
// A body that does not parse as a mapping is let through here, same as
// checkAllowedKeys and for the same reason: the later splice/unmarshal pass
// already produces a clearer error naming the actual YAML problem, and
// duplicating that failure here would only double the error, not the
// coverage.
func checkEmptyRules(key string, body []byte) error {
	var raw map[string]yaml.Node
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return nil
	}
	node, present := raw["rules"]
	if !present {
		return nil
	}
	empty := (node.Kind == yaml.SequenceNode && len(node.Content) == 0) ||
		(node.Kind == yaml.ScalarNode && node.Tag == "!!null")
	if !empty {
		return nil
	}
	return fmt.Errorf(
		"%s: rules: [] is ambiguous -- omit the key to keep the current rules, or set enabled: false to disable this block",
		key,
	)
}

// spliceBlock nests body under key's path, producing a document that Config
// can bind. "collect.logs" becomes `collect:\n  logs:\n    <body>`.
func spliceBlock(key string, body []byte) ([]byte, error) {
	if !IsManagedBlockKey(key) {
		return nil, fmt.Errorf("unknown config block %q", key)
	}
	var buf bytes.Buffer
	segments := strings.Split(key, ".")
	for i, seg := range segments {
		buf.WriteString(strings.Repeat("  ", i))
		buf.WriteString(seg)
		buf.WriteString(":\n")
	}
	indent := strings.Repeat("  ", len(segments))
	for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if line == "" {
			buf.WriteString("\n")
			continue
		}
		buf.WriteString(indent)
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	return buf.Bytes(), nil
}

// ValidateBlock parses body as the named block and reports the first reason
// it cannot be used.
//
// Unknown keys are an ERROR here, unlike Load's own warning: this is the
// interactive path, there is a human to correct it, and a silently dropped
// key is how a rule loses its filter and collects a whole namespace.
//
// A key the struct has but the platform does not expose (redact_secrets,
// custom_endpoints, spill_dir, ...) is refused the same way, by
// checkAllowedKeys, before the body is ever parsed into a Config.
//
// It runs the BLOCK's validators, never Config.Validate: a one-block
// document carries no tenant token and no gateway address, so the whole-
// config validator would refuse every block for reasons outside it.
func ValidateBlock(key string, body []byte) error {
	if !IsManagedBlockKey(key) {
		return fmt.Errorf("unknown config block %q", key)
	}
	if err := checkAllowedKeys(key, body); err != nil {
		return err
	}
	if err := checkEmptyRules(key, body); err != nil {
		return err
	}
	doc, err := spliceBlock(key, body)
	if err != nil {
		return err
	}
	if warnings := UnknownKeys(doc); len(warnings) > 0 {
		return fmt.Errorf("%s: %s", key, strings.Join(warnings, "; "))
	}
	cfg := Default()
	if err := yaml.Unmarshal(doc, cfg); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	cfg.Normalize()

	var violations []string
	switch key {
	case "collect.logs":
		violations = cfg.Collect.Logs.validate()
	case "collect.state":
		violations = cfg.Collect.State.validate()
	case "collect.metrics":
		violations = cfg.Collect.Metrics.validate()
	case "query":
		violations = cfg.validateQuery()
	case "buffer":
		// The only check Config.Validate makes that this block's one
		// editable field (batch_size) can violate. max_memory_bytes is out
		// of scope here and Default() always makes it positive, so there is
		// nothing else in Config.Validate's buffer checks left to mirror.
		if cfg.Buffer.BatchSize <= 0 {
			violations = append(violations, "buffer.batch_size must be greater than 0")
		}
	}
	if len(violations) > 0 {
		return fmt.Errorf("%s: %s", key, strings.Join(violations, "; "))
	}
	return nil
}

// cloneConfig returns a deep copy of cfg. A shallow `*cfg` struct copy keeps
// the ORIGINAL pointers, slices and maps: yaml.v3 decodes a scalar into an
// existing non-nil pointer by writing THROUGH it rather than allocating a
// new one, so unmarshalling a block into a shallow copy could still mutate
// cfg's own QueryConfig.Enabled, QueryConfig.RedactSecrets or
// MetricsCollectConfig.MaxSamplesPerScrape -- all pointers, all reachable
// from a managed block. A mutated base would then make releasing a block
// restore the platform's last value instead of the operator's file value.
//
// Marshalling cfg to YAML and unmarshalling into a fresh, zero-valued Config
// gives new backing memory for everything the format carries, closing that
// off for every pointer, slice and map field, present or future -- not only
// the three named above.
//
// Fields tagged yaml:"-" are not carried by that round trip and must be
// restored explicitly. Today that is exactly one: Agent.ClusterID, set at
// runtime from the kube-system namespace UID and never written to the
// config file.
func cloneConfig(cfg *Config) (*Config, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("clone config: %w", err)
	}
	clone := &Config{}
	if err := yaml.Unmarshal(data, clone); err != nil {
		return nil, fmt.Errorf("clone config: %w", err)
	}
	clone.Agent.ClusterID = cfg.Agent.ClusterID
	return clone, nil
}

// ApplyBlocks overlays the managed blocks on a deep copy of base and returns
// the effective config. base is never modified, not even through a shared
// pointer field: the agent keeps the config it loaded from disk immutable so
// releasing a block can restore that value rather than a default, or a
// stale platform value, this code invents.
//
// The overlay is a MERGE, not a replacement of the block's node. A block
// body carries only the keys E3 edits, so replacing the node would reset
// every other key in that block to a default nobody chose -- saving
// buffer.batch_size would silently reset the operator's max_disk_bytes, a
// field deliberately left out of scope because mis-setting it means an OOM
// and a deadlocked write-ahead log. yaml.v3 unmarshalling into a populated
// struct gives exactly this: absent keys keep their value, a present key
// (including a list) replaces.
//
// It is all-or-nothing. A caller that applied the blocks it could parse
// would leave the agent in a state no operator ever wrote.
func ApplyBlocks(base *Config, blocks map[string][]byte) (*Config, error) {
	if base == nil {
		return nil, fmt.Errorf("base config is nil")
	}
	for _, key := range managedBlockKeys { // deterministic order
		body, ok := blocks[key]
		if !ok {
			continue
		}
		if err := ValidateBlock(key, body); err != nil {
			return nil, err
		}
	}
	for key := range blocks {
		if !IsManagedBlockKey(key) {
			return nil, fmt.Errorf("unknown config block %q", key)
		}
	}

	cfg, err := cloneConfig(base)
	if err != nil {
		return nil, err
	}
	for _, key := range managedBlockKeys {
		body, ok := blocks[key]
		if !ok {
			continue
		}
		doc, err := spliceBlock(key, body)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(doc, cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
	}
	cfg.Normalize()
	// The whole-config validator IS right here: unlike a single block, the
	// effective config is complete -- it started from the operator's file.
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("effective config: %w", err)
	}
	return cfg, nil
}
