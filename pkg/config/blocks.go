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
