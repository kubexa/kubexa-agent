// Command schemagen reflects over kubexa-agent's own managed-block structs
// to produce schema/agent-config-blocks.json: the JSON Schema the backend
// validates a platform-pushed config block against (Task 11) and the app
// builds a per-block form from (Task 17).
//
// It carries no list of its own of either the managed blocks, their
// editable keys, or their shapes -- all three come from pkg/config, through
// config.ManagedBlockKeys, config.ManagedBlockAllowedKeys and
// config.ManagedBlockPrototype, so the schema cannot drift from what
// pkg/config.ValidateBlock actually accepts.
//
// Run it with `go run ./tools/schemagen -o schema/agent-config-blocks.json`.
// Use -o rather than shell redirection: `>` truncates its target the moment
// the shell parses the command line, before this program has run at all, so
// a hard error here -- the phantom-allowlist-key case Ruling B demands, say
// -- would leave the committed file at 0 bytes instead of unchanged. -o
// only opens the destination after the schema has built successfully.
//
// Output is deterministic (object keys marshal in sorted order): the build
// workflow regenerates into a temp file and diffs it against the committed
// one, so drift between the structs and the committed schema fails CI.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/kubexa/kubexa-agent/pkg/config"
)

var durationType = reflect.TypeOf(time.Duration(0))

func main() {
	out := flag.String("o", "", "write the schema here instead of stdout")
	flag.Parse()

	schema, err := buildSchema()
	if err != nil {
		fmt.Fprintln(os.Stderr, "schemagen:", err)
		os.Exit(1)
	}
	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "schemagen:", err)
		os.Exit(1)
	}
	data = append(data, '\n')

	if *out == "" {
		if _, err := os.Stdout.Write(data); err != nil {
			fmt.Fprintln(os.Stderr, "schemagen:", err)
			os.Exit(1)
		}
		return
	}
	// Only reached once the schema built and marshalled without error, so
	// the destination is never truncated ahead of a failure.
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "schemagen:", err)
		os.Exit(1)
	}
}

// buildSchema reflects over every managed block and returns the full JSON
// Schema document as a plain JSON-marshalable value.
//
// It carries NO version field (Ruling A): git describe changes on every
// commit, so baking one in would make the CI diff step fail on every merge.
// Version skew is the agent's own job, rejecting what it does not
// understand and reporting it in config_error -- the app already knows the
// agent's version from the handshake and heartbeat, so a version here would
// be a third, staler copy of the same fact.
func buildSchema() (map[string]any, error) {
	properties := map[string]any{}
	for _, key := range config.ManagedBlockKeys() {
		blockSchema, err := schemaForManagedBlock(key)
		if err != nil {
			return nil, fmt.Errorf("block %q: %w", key, err)
		}
		properties[key] = blockSchema
	}
	return map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type":    "object",
		// The document root is exactly as closed a set as a block body:
		// IsManagedBlockKey refuses "rbac", "mutate.policy" and the like
		// with "unknown config block", and this must refuse them too, or a
		// key ValidateBlock rejects would still validate against the
		// schema.
		"additionalProperties": false,
		"properties":           properties,
	}, nil
}

// schemaForManagedBlock builds the JSON Schema for one managed block: an
// object whose properties are the intersection of its allowlist and its
// struct's yaml tags (Ruling B). config.ManagedBlockAllowedKeys decides
// WHICH keys exist; config.ManagedBlockPrototype's struct supplies each
// one's type and nested shape.
//
// The block's own top level is closed (additionalProperties: false): that
// mirrors checkAllowedKeys in pkg/config/blocks.go, which hard-refuses any
// top-level key the allowlist does not name.
func schemaForManagedBlock(key string) (map[string]any, error) {
	proto, ok := config.ManagedBlockPrototype(key)
	if !ok {
		return nil, fmt.Errorf("no prototype registered in config.ManagedBlockPrototype")
	}
	allowed, ok := config.ManagedBlockAllowedKeys(key)
	if !ok {
		return nil, fmt.Errorf("no allowlist registered in config.ManagedBlockAllowedKeys")
	}
	schema, err := schemaForAllowedKeys(allowed, reflect.TypeOf(proto))
	if err != nil {
		return nil, err
	}
	schema["additionalProperties"] = false
	return schema, nil
}

// schemaForAllowedKeys is the pure core of Ruling B: given a set of allowed
// top-level keys and the struct type that must supply each one's shape, it
// returns an object schema naming exactly those keys.
//
// An allowed key with no matching yaml tag on t is a hard error, not a
// skip: it means the allowlist names something that cannot exist, which is
// a bug in the allowlist itself, not a property to silently drop.
func schemaForAllowedKeys(allowed []string, t reflect.Type) (map[string]any, error) {
	fields := yamlFields(t)
	properties := map[string]any{}
	for _, name := range allowed {
		field, ok := fields[name]
		if !ok {
			return nil, fmt.Errorf("allowlist names %q, which has no matching yaml tag on %s", name, t.Name())
		}
		propSchema, err := schemaForType(field.Type)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", name, err)
		}
		properties[name] = propSchema
	}
	return map[string]any{
		"type":       "object",
		"properties": properties,
	}, nil
}

// yamlFields maps each of t's yaml tag names (the ",omitempty" and similar
// suffixes stripped) to its field. A field tagged yaml:"-", with no yaml
// tag at all, or unexported, is left out -- there is nothing here that
// pkg/config's own yaml (de)serialization would carry either.
func yamlFields(t reflect.Type) map[string]reflect.StructField {
	out := make(map[string]reflect.StructField, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		tag, ok := f.Tag.Lookup("yaml")
		if !ok {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out[name] = f
	}
	return out
}

// schemaForType maps a Go type reachable from a managed block to a JSON
// Schema fragment. It recurses into pointers, slices, maps and structs so a
// nested shape -- a rules entry, say -- gets real properties instead of a
// bare "array": that is what lets a form be built from the result.
//
// It encodes only SHAPE. Nothing here invents a validation rule that does
// not already live in pkg/config: field-level constraints (a positive
// batch_size, a resource string the policy compiler accepts, and so on)
// stay the agent's job in ValidateBlock, not the schema's.
func schemaForType(t reflect.Type) (map[string]any, error) {
	if t.Kind() == reflect.Pointer {
		// A pointer here (QueryConfig.Enabled, LogNamespaceRule.Follow, ...)
		// means "unset inherits/defaults", which is exactly what an absent
		// property in a partial block body already means. The shape a form
		// needs is the pointed-to type.
		return schemaForType(t.Elem())
	}
	switch t.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if t == durationType {
			// yaml.v3's isDuration flag (decode.go) does the opposite of
			// what a plain int64 field gets: for every OTHER integer type
			// it lets a bare number through, but for time.Duration it
			// refuses one and requires a duration string ("5m").
			// config.ValidateBlock confirms it against the real producer:
			// "resync_period: 5m" parses, "resync_period: 300000000000"
			// fails with "cannot unmarshal !!int into time.Duration". A
			// ["string","integer"] union here would advertise an
			// acceptance path the agent does not have.
			return map[string]any{"type": "string"}, nil
		}
		return map[string]any{"type": "integer"}, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil
	case reflect.Slice, reflect.Array:
		items, err := schemaForType(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items}, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("unsupported map key type %s", t.Key())
		}
		values, err := schemaForType(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": values}, nil
	case reflect.Struct:
		return schemaForStruct(t)
	default:
		return nil, fmt.Errorf("unsupported kind %s for type %s", t.Kind(), t)
	}
}

// schemaForStruct expands every yaml-tagged field of t. Unlike a managed
// block's own top level, a nested struct (a rules entry, a custom metric
// endpoint) is not filtered by any allowlist: checkAllowedKeys
// (pkg/config/blocks.go) only ever looks at a block body's OWN top-level
// keys, and a key nested inside a rules entry is governed by UnknownKeys
// instead. So every field here is included, and the object stays open
// (no additionalProperties: false) to match that.
func schemaForStruct(t reflect.Type) (map[string]any, error) {
	properties := map[string]any{}
	for name, field := range yamlFields(t) {
		propSchema, err := schemaForType(field.Type)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", name, err)
		}
		properties[name] = propSchema
	}
	return map[string]any{
		"type":       "object",
		"properties": properties,
	}, nil
}
