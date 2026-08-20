package config_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// values.schema.json is the only check that runs before the config reaches the
// cluster: helm validates values against it on install, upgrade, template and
// lint. Everything after it is too late -- the agent's own startup warning
// (see strict.go) is read, if ever, after the rule has already been running
// with the wrong keys.
//
// The schema described `query.rules` fully and `collect.logs/state/metrics`
// as bare objects, so `query.rules[0].labelSelector` was refused by name while
// `collect.logs.rules[0].podNames` installed clean and collected the whole
// namespace. These tests hold the schema to the two things it claims to
// describe: the agent's structs for the passed-through rule lists, and the
// template's own .Values references for the chart-level keys around them.

func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	var schema map[string]any
	raw := chartFile(t, "helm", "kubexa-agent", "values.schema.json")
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatalf("parse values.schema.json: %v", err)
	}
	return schema
}

// schemaAt walks a dotted values path through the schema's `properties` maps.
// It returns nil when the path is not described, which is the state this test
// exists to catch, so callers report it rather than panicking.
func schemaAt(schema map[string]any, path string) map[string]any {
	node := schema
	for _, segment := range strings.Split(path, ".") {
		props, ok := node["properties"].(map[string]any)
		if !ok {
			return nil
		}
		next, ok := props[segment].(map[string]any)
		if !ok {
			return nil
		}
		node = next
	}
	return node
}

// declaredTypes normalizes a schema node's `type`, which JSON Schema allows to
// be a single name or a list of them.
func declaredTypes(node map[string]any) []string {
	switch typ := node["type"].(type) {
	case string:
		return []string{typ}
	case []any:
		out := make([]string, 0, len(typ))
		for _, one := range typ {
			if name, ok := one.(string); ok {
				out = append(out, name)
			}
		}
		sort.Strings(out)
		return out
	}
	return nil
}

var durationType = reflect.TypeOf(time.Duration(0))

// jsonTypes is the set of JSON types a value must be allowed to take for the
// agent's YAML decoder to bind it to this field.
func jsonTypes(field reflect.Type) []string {
	for field.Kind() == reflect.Pointer {
		field = field.Elem()
	}
	if field == durationType {
		// yaml.v3 takes "30s" or a bare nanosecond count; the chart writes the
		// first, and refusing the second would reject a config the agent loads.
		return []string{"integer", "string"}
	}
	switch field.Kind() {
	case reflect.String:
		return []string{"string"}
	case reflect.Bool:
		return []string{"boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return []string{"integer"}
	case reflect.Float32, reflect.Float64:
		return []string{"number"}
	case reflect.Slice, reflect.Array:
		return []string{"array"}
	case reflect.Map, reflect.Struct:
		return []string{"object"}
	}
	return nil
}

// fieldsByYAMLKey indexes a rule struct by the key it binds.
func fieldsByYAMLKey(t reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if name != "" && name != "-" {
			fields[name] = t.Field(i).Type
		}
	}
	return fields
}

// Every list the template passes through with toYaml carries the agent's own
// keys, so the schema for its items is the struct. Both directions matter and
// they fail differently: a key the schema omits is REFUSED though the agent
// binds it, and a key the schema invents is accepted though the agent drops it.
func TestSchemaPassthroughItemsMatchAgentStructs(t *testing.T) {
	schema := loadSchema(t)

	for path, ruleType := range rulePaths() {
		node := schemaAt(schema, path)
		if node == nil {
			t.Errorf("%s: not described in values.schema.json, so helm accepts any key under "+
				"it and the agent silently drops the ones it does not know", path)
			continue
		}
		items, ok := node["items"].(map[string]any)
		if !ok {
			t.Errorf("%s: described without `items`, so the rules inside it are unchecked", path)
			continue
		}
		if extra, ok := items["additionalProperties"].(bool); !ok || extra {
			t.Errorf("%s items: additionalProperties is not false, so a misspelled key is "+
				"accepted by helm and dropped by the agent", path)
		}

		props, _ := items["properties"].(map[string]any)
		fields := fieldsByYAMLKey(ruleType)

		for key := range fields {
			if _, ok := props[key]; !ok {
				t.Errorf("%s items: schema omits %q, which %s binds -- with "+
					"additionalProperties false, helm now refuses a valid config",
					path, key, ruleType.Name())
			}
		}
		for key := range props {
			field, ok := fields[key]
			if !ok {
				t.Errorf("%s items: schema allows %q, which %s does not bind, so the schema "+
					"blesses a key the agent drops", path, key, ruleType.Name())
				continue
			}
			want := jsonTypes(field)
			got := declaredTypes(props[key].(map[string]any))
			if !reflect.DeepEqual(want, got) {
				t.Errorf("%s items: %q is type %v in the schema, want %v (%s)",
					path, key, got, want, field)
			}
		}
	}
}

// hasKeyRef catches `hasKey .Values.query "redactSecrets"`, which reads a value
// without ever writing its dotted path.
var (
	valuesRef = regexp.MustCompile(`\.Values\.((?:collect\.(?:logs|state|metrics))|query)\.([A-Za-z0-9]+)`)
	hasKeyRef = regexp.MustCompile(`hasKey\s+\.Values\.((?:collect\.(?:logs|state|metrics))|query)\s+"([A-Za-z0-9]+)"`)
)

// templateKeys returns, per block, the chart keys the templates actually read.
func templateKeys(t *testing.T) map[string]map[string]bool {
	t.Helper()
	keys := map[string]map[string]bool{}
	for _, name := range []string{"configmap.yaml", "clusterrole.yaml", "NOTES.txt"} {
		body := chartFile(t, "helm", "kubexa-agent", "templates", name)
		for _, re := range []*regexp.Regexp{valuesRef, hasKeyRef} {
			for _, m := range re.FindAllStringSubmatch(body, -1) {
				if keys[m[1]] == nil {
					keys[m[1]] = map[string]bool{}
				}
				keys[m[1]][m[2]] = true
			}
		}
	}
	return keys
}

// Around the passthrough lists the keys are the CHART's, camelCase, and the
// mirror mistake is just as quiet: `tail_lines` at chart level reads as unset,
// and the rendered config carries the chart's default instead. The template is
// the authority on which keys are real, so the schema is checked against it
// rather than against a second hand-written list.
func TestSchemaBlocksRejectUnknownChartKeys(t *testing.T) {
	schema := loadSchema(t)
	fromTemplate := templateKeys(t)

	for _, path := range []string{"collect.logs", "collect.state", "collect.metrics", "query"} {
		want := fromTemplate[path]
		if len(want) == 0 {
			t.Fatalf("%s: no .Values references found in the templates; this test is pinning nothing", path)
		}
		node := schemaAt(schema, path)
		if node == nil {
			t.Errorf("%s: not described in values.schema.json", path)
			continue
		}
		if extra, ok := node["additionalProperties"].(bool); !ok || extra {
			t.Errorf("%s: additionalProperties is not false, so a camelCase key the template "+
				"never reads installs without complaint", path)
		}
		props, _ := node["properties"].(map[string]any)
		for key := range want {
			if _, ok := props[key]; !ok {
				t.Errorf("%s: the template reads .Values.%s.%s but the schema does not allow it, "+
					"so helm refuses a key the chart itself uses", path, path, key)
			}
		}
		for key := range props {
			if !want[key] {
				t.Errorf("%s: schema allows %q, which no template reads -- it renders nothing "+
					"and reads as a setting that took effect", path, key)
			}
		}
	}
}

// The chart's own defaults are the first config anyone installs. If the schema
// refuses them, `helm install` fails on an untouched values file.
func TestShippedValuesSatisfyTheSchema(t *testing.T) {
	schema := loadSchema(t)
	var values map[string]any
	if err := yaml.Unmarshal([]byte(chartFile(t, "helm", "kubexa-agent", "values.yaml")), &values); err != nil {
		t.Fatalf("parse values.yaml: %v", err)
	}

	var walk func(node map[string]any, value any, where string)
	walk = func(node map[string]any, value any, where string) {
		if node == nil {
			return
		}
		if items, ok := node["items"].(map[string]any); ok {
			list, _ := value.([]any)
			for i, item := range list {
				walk(items, item, fmt.Sprintf("%s[%d]", where, i))
			}
			return
		}
		fields, ok := value.(map[string]any)
		if !ok {
			return
		}
		props, _ := node["properties"].(map[string]any)
		closed, _ := node["additionalProperties"].(bool)
		for key, sub := range fields {
			child, described := props[key].(map[string]any)
			if !described {
				if !closed {
					continue
				}
				t.Errorf("values.yaml %s.%s is not allowed by the schema, so an untouched "+
					"chart fails its own validation", where, key)
				continue
			}
			walk(child, sub, where+"."+key)
		}
	}
	walk(schema, values, "")
}
