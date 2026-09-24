package config

import (
	"bytes"
	"fmt"
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
// It runs the BLOCK's validators, never Config.Validate: a one-block
// document carries no tenant token and no gateway address, so the whole-
// config validator would refuse every block for reasons outside it.
func ValidateBlock(key string, body []byte) error {
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
		// The two checks Config.Validate makes inline for this block. Only
		// batch_size and flush_interval are editable; max_memory_bytes is
		// out of scope and comes from the operator's file.
		if cfg.Buffer.BatchSize <= 0 {
			violations = append(violations, "buffer.batch_size must be greater than 0")
		}
		if cfg.Buffer.FlushInterval < 0 {
			violations = append(violations, "buffer.flush_interval must not be negative")
		}
	}
	if len(violations) > 0 {
		return fmt.Errorf("%s: %s", key, strings.Join(violations, "; "))
	}
	return nil
}

// ApplyBlocks overlays the managed blocks on a copy of base and returns the
// effective config. base is never modified: the agent keeps the config it
// loaded from disk immutable so releasing a block can restore that value
// rather than a default this code invents.
//
// The overlay is a MERGE, not a replacement of the block's node. A block
// body carries only the keys E3 edits, so replacing the node would reset
// every other key in that block to a default nobody chose -- saving
// buffer.batch_size would silently reset the operator's max_disk_bytes, a
// field deliberately left out of scope because mis-setting it means an OOM
// and a deadlocked write-ahead log. yaml.v3 unmarshalling into a populated
// struct gives exactly this: absent keys keep their value, a present key
// (including a list) replaces. Nothing is mutated in place, so the shallow
// copy below never writes through to base.
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

	cfg := *base
	for _, key := range managedBlockKeys {
		body, ok := blocks[key]
		if !ok {
			continue
		}
		doc, err := spliceBlock(key, body)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(doc, &cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
	}
	cfg.Normalize()
	// The whole-config validator IS right here: unlike a single block, the
	// effective config is complete -- it started from the operator's file.
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("effective config: %w", err)
	}
	return &cfg, nil
}
