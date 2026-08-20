package config_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// jsonTypes is the set of JSON types a value may take without the agent's
// decoder rejecting it. These are yaml.v3's actual rules, measured rather than
// assumed, because the two interesting cases are opposites:
//
//	dur: 30       -> cannot unmarshal !!int `30` into time.Duration
//	str: 7        -> binds, Str == "7"
//	labels: {v:1} -> binds, Labels == map[v:1]
//	bool: "true"  -> cannot unmarshal !!str `true` into bool
//
// So a duration must be declared string-only -- an integer there renders a
// config the agent refuses at startup, which is a CrashLoopBackOff, not a
// dropped key -- while every string field must accept any scalar, or the
// schema refuses values that install and work today (`labels: {version: 1}`).
func jsonTypes(field reflect.Type) []string {
	for field.Kind() == reflect.Pointer {
		field = field.Elem()
	}
	if field == durationType {
		return []string{"string"}
	}
	//nolint:exhaustive // every kind the rule structs use is listed; the rest fall through.
	switch field.Kind() {
	case reflect.String:
		return []string{"boolean", "integer", "number", "string"}
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

// checkType compares one schema node against the field it has to admit, and
// descends: a list's `items` and a map's `additionalProperties` carry the
// element type, and that is where the string widening above actually matters
// (pod_names, labels). A node that enumerates its allowed values is left
// narrow -- no integer equals "list", so widening it would say nothing.
//
// nullable says whether a blank value is allowed here, and it is inherited by
// everything nested. Helm coalesces map values and drops nulls, but list items
// are replaced wholesale, so a blank inside a rule reaches the validator -- and
// yaml.v3 reads every spelling of it as "not set":
//
//	pod_names:        -> nil slice
//	pod_names: [null] -> the entry is SKIPPED, len 0
//	labels: {team: }  -> binds ""
//
// It is false only under a key the agent refuses to start without, where a
// blank is the empty value it already rejects.
func checkType(t *testing.T, where string, field reflect.Type, node map[string]any, nullable bool) {
	t.Helper()
	for field.Kind() == reflect.Pointer {
		field = field.Elem()
	}
	want := jsonTypes(field)
	if _, enumerated := node["enum"]; enumerated && field.Kind() == reflect.String {
		want = []string{"string"}
	}
	if nullable {
		want = append(want, "null")
		sort.Strings(want)
	}
	if got := declaredTypes(node); !reflect.DeepEqual(want, got) {
		t.Errorf("%s: type %v in the schema, want %v (%s)", where, got, want, field)
		return
	}
	if field == durationType {
		// "30" is as fatal as 30: yaml.v3 refuses !!str into time.Duration
		// just as it refuses !!int, and the agent exits on the rendered file.
		// Only a pattern separates a duration from any other string.
		if pattern, ok := node["pattern"].(string); !ok || pattern == "" {
			t.Errorf("%s: a duration with no `pattern`, so a quoted number passes helm and "+
				"the agent exits on the config it renders", where)
		}
	}
	switch field.Kind() {
	case reflect.Slice, reflect.Array:
		items, ok := node["items"].(map[string]any)
		if !ok {
			t.Errorf("%s: an array with no `items`, so its elements are unchecked", where)
			return
		}
		checkType(t, where+"[]", field.Elem(), items, nullable)
	case reflect.Map:
		values, ok := node["additionalProperties"].(map[string]any)
		if !ok {
			t.Errorf("%s: a map with no `additionalProperties` schema, so its values are unchecked", where)
			return
		}
		checkType(t, where+"{}", field.Elem(), values, nullable)
	default:
	}
}

// requiredByPath is what the agent's own Validate rejects a config for, and so
// the only thing the schema may mark required: anything more refuses a config
// the agent runs, and anything less lets helm install one it will not.
//
// Only query.rules qualifies. validateQuery walks the rules whatever
// query.enabled says, while StateCollectConfig.validate and
// MetricsCollectConfig.validate return before looking at a rule when their
// section is disabled -- so a resource-less rule left behind under
// `enabled: false` starts fine, and helm must not refuse it.
// collect.metrics.customEndpoints validates neither name nor url at all.
func requiredByPath() map[string][]string {
	return map[string][]string{
		"query.rules": {"resources"}, // query.go: rejected however query.enabled reads
	}
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

		mandatory := map[string]bool{}
		for _, key := range requiredByPath()[path] {
			mandatory[key] = true
		}

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
			// A key the agent refuses to start without is not nullable: a null
			// there is the empty value it already rejects.
			checkType(t, fmt.Sprintf("%s items: %q", path, key), field,
				props[key].(map[string]any), !mandatory[key])
		}

		var required []string
		declared, _ := items["required"].([]any)
		for _, one := range declared {
			name, _ := one.(string)
			required = append(required, name)
		}
		sort.Strings(required)
		want := requiredByPath()[path]
		sort.Strings(want)
		if !reflect.DeepEqual(required, want) {
			t.Errorf("%s items: required = %v, want %v -- the schema may demand exactly what "+
				"the agent's Validate demands, no more (helm would refuse a config that runs) "+
				"and no less (helm would pass one that does not)", path, required, want)
		}
	}
}

// hasKeyRef catches `hasKey .Values.query "redactSecrets"`, which reads a value
// without ever writing its dotted path.
var (
	valuesRef = regexp.MustCompile(`\.Values\.((?:collect\.[A-Za-z0-9]+)|query)\.([A-Za-z0-9]+)`)
	hasKeyRef = regexp.MustCompile(`hasKey\s+\.Values\.((?:collect\.[A-Za-z0-9]+)|query)\s+"([A-Za-z0-9]+)"`)
)

// templateKeys returns, per block, the chart keys the templates actually read.
// The file list is globbed rather than written out: a hand-written list goes
// stale the moment a template starts reading a new key, and the schema would
// then refuse a key the chart itself uses, with no test failing.
func templateKeys(t *testing.T) map[string]map[string]bool {
	t.Helper()
	templates, err := filepath.Glob(filepath.Join("..", "..", "helm", "kubexa-agent", "templates", "*"))
	if err != nil || len(templates) == 0 {
		t.Fatalf("glob chart templates: %v (found %d)", err, len(templates))
	}
	keys := map[string]map[string]bool{}
	for _, path := range templates {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := string(raw)
		for _, re := range []*regexp.Regexp{valuesRef, hasKeyRef} {
			for _, m := range re.FindAllStringSubmatch(body, -1) {
				if keys[m[1]] == nil {
					keys[m[1]] = map[string]bool{}
				}
				keys[m[1]][m[2]] = true
				// `collect` is a block too: .Values.collect.logs.enabled says
				// `logs` is one of its keys, and `collect.log.enabled` -- the
				// singular typo -- is the same silent mistake one level up.
				if parent, child, found := strings.Cut(m[1], "."); found {
					if keys[parent] == nil {
						keys[parent] = map[string]bool{}
					}
					keys[parent][child] = true
				}
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

	if len(fromTemplate) == 0 {
		t.Fatal("no .Values references found in the templates; this test is pinning nothing")
	}
	for path, want := range fromTemplate {
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
		// `additionalProperties: false` decodes to the bool false, which is also
		// the zero value a missing key yields: the second result is what tells
		// the two apart, and dropping it made this walk skip every closed node.
		extra, present := node["additionalProperties"].(bool)
		closed := present && !extra
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

// The walk above judges keys only. Everything else the schema says -- types,
// required, enums, minItems -- is enforced by helm's validator, not by any Go
// code here, and a schema that refuses the chart's own defaults would fail
// every install of an untouched chart. So the check is helm itself.
func TestHelmAcceptsTheShippedChart(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		// `make helm-lint` validates the DEFAULT values only, so it exercises
		// none of the refusals below. Nothing else covers them.
		t.Skip("helm not installed: the schema's types, patterns and required fields go unchecked")
	}

	cases := []struct {
		name string
		args []string
		want string // empty: helm must accept
	}{
		{name: "defaults"},
		{
			name: "a rule written with the agent's keys",
			args: []string{
				"--set", "collect.logs.rules[0].namespace=prod",
				"--set-json", `collect.logs.rules[0].pod_names=["api-*"]`,
				"--set-json", `collect.logs.rules[0].labels={"version":1}`,
			},
		},
		{
			name: "a rule written with the chart's camelCase",
			args: []string{"--set-json", `collect.logs.rules[0]={"namespace":"prod","podNames":["api-*"]}`},
			want: "podNames",
		},
		{
			name: "a chart key written in the agent's snake_case",
			args: []string{"--set", "collect.logs.tail_lines=50"},
			want: "tail_lines",
		},
		{
			// A number here -- quoted or not -- renders a config the agent
			// refuses at startup, so helm has to be the one to say no.
			name: "a duration given as a number",
			args: []string{
				"--set-json", `collect.metrics.rules[0]={"resources":["pods"],"pod_interval":30}`,
			},
			want: "pod_interval",
		},
		{
			name: "a duration given as a quoted number",
			args: []string{
				"--set-json", `collect.metrics.rules[0]={"resources":["pods"],"pod_interval":"30"}`,
			},
			want: "pod_interval",
		},
		{
			name: "a block key misspelled one level up",
			args: []string{"--set", "collect.logz.enabled=false"},
			want: "logz",
		},
		{
			// Helm replaces list items wholesale instead of coalescing them,
			// so unlike a chart-level null this one reaches the validator --
			// and the agent binds it as "filter not set".
			name: "a rule field left blank",
			args: []string{"--values", filepath.Join("testdata", "blank-rule-fields.yaml")},
		},
		{
			// state and metrics validate nothing while disabled, so helm must
			// not refuse what the agent would start with.
			name: "a resource-less rule under a disabled section",
			args: []string{"--values", filepath.Join("testdata", "disabled-section-rule.yaml")},
		},
		{
			// A negative tail count is refused by the agent only while logs are
			// enabled, so the schema cannot carry a `minimum` for it.
			name: "values the agent does not check while the section is disabled",
			args: []string{"--values", filepath.Join("testdata", "unvalidated-while-disabled.yaml")},
		},
		{
			// Everything time.ParseDuration takes, including the bare "0" that
			// turns a resync off -- refusing these would fail an upgrade.
			name: "the duration forms the agent parses",
			args: []string{"--values", filepath.Join("testdata", "duration-forms.yaml")},
		},
		{
			// validatePattern allows "*" only at the end, however query.enabled
			// reads, so an infix one is a startup failure helm can prevent.
			name: "an infix wildcard in a query namespace",
			args: []string{
				"--set-json", `query.rules[0]={"resources":["pods"],"namespace":"pr*d"}`,
			},
			want: "namespace",
		},
		{
			name: "a partial resource wildcard",
			args: []string{"--set-json", `query.rules[0]={"resources":["apps/*"]}`},
			want: "resources",
		},
		{
			name: "a trailing wildcard, which the agent supports",
			args: []string{
				"--set-json", `query.rules[0]={"resources":["*"],"namespace":"prod*","names":["api-*"]}`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{
				"template", "t", filepath.Join("..", "..", "helm", "kubexa-agent"),
				"--set", "secret.tenantToken=x",
			}, tc.args...)
			out, err := exec.Command(helm, args...).CombinedOutput()

			if tc.want == "" {
				if err != nil {
					t.Errorf("helm refused a valid chart: %v\n%s", err, out)
				}
				return
			}
			if err == nil {
				t.Fatalf("helm accepted %s; the schema is the only gate that runs before "+
					"the config reaches the cluster", tc.want)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("helm refused it without naming %q, so the operator is not told what "+
					"to fix:\n%s", tc.want, out)
			}
		})
	}
}
