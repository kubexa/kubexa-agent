package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kubexa/kubexa-agent/pkg/config"
)

// The chart renders every `rules` list (and custom_endpoints) with toYaml,
// which copies the operator's keys through untouched. So those keys are the
// AGENT's own config keys -- snake_case -- while every chart value around
// them is camelCase. Nothing enforces the difference: an unknown key is
// dropped, and a rule that lost its filters matches more than it was meant
// to, never less.
//
// The chart shipped `podNames` / `labelSelector` / `extraLabels` in its
// examples for exactly that reason, and a rule copied from them collected
// every pod in its namespace. These tests read the chart and the README the
// way an operator does -- by copying what is written there -- and check the
// keys against the structs that have to accept them.
//
// rulePaths maps a values.yaml path whose contents pass through verbatim to
// the struct that receives them.
func rulePaths() map[string]reflect.Type {
	return map[string]reflect.Type{
		"collect.logs.rules":              reflect.TypeOf(config.LogNamespaceRule{}),
		"collect.state.rules":             reflect.TypeOf(config.StateNamespaceRule{}),
		"collect.metrics.rules":           reflect.TypeOf(config.MetricsNamespaceRule{}),
		"collect.metrics.customEndpoints": reflect.TypeOf(config.MetricEndpointConfig{}),
		"query.rules":                     reflect.TypeOf(config.QueryRule{}),
	}
}

// acceptedKeys returns the YAML keys a rule struct binds.
func acceptedKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		name, _, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" {
			keys[name] = true
		}
	}
	return keys
}

// chartFile reads a file from the chart, which lives outside this package's
// directory. `go test` can serve a cached PASS after a chart-only edit, so a
// mutation that should be red reads as green -- pass -count=1 when checking
// one by hand. `make test` and the CI step already do.
func chartFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", ".."}, parts...)...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// checkItems reports every key in items that the struct does not bind.
func checkItems(t *testing.T, where string, ruleType reflect.Type, items []any) {
	t.Helper()
	accepted := acceptedKeys(ruleType)
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		bad := []string{}
		for key := range fields {
			if !accepted[key] {
				bad = append(bad, key)
			}
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			t.Errorf("%s: %v -- %s does not bind these, so an operator who copies this "+
				"gets a rule without them and never hears why", where, bad, ruleType.Name())
		}
	}
}

// itemsAt walks a parsed values tree to a dotted path and returns its list items.
func itemsAt(tree map[string]any, path string) []any {
	var cur any = tree
	for _, segment := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[segment]
		if !ok {
			return nil
		}
	}
	list, _ := cur.([]any)
	return list
}

// The defaults the chart actually ships -- not comments, these render into
// every install that does not override them.
func TestChartDefaultRuleKeysAreAgentKeys(t *testing.T) {
	var values map[string]any
	if err := yaml.Unmarshal([]byte(chartFile(t, "helm", "kubexa-agent", "values.yaml")), &values); err != nil {
		t.Fatalf("parse values.yaml: %v", err)
	}

	checked := 0
	for path, ruleType := range rulePaths() {
		items := itemsAt(values, path)
		if len(items) == 0 {
			continue
		}
		checked++
		checkItems(t, "values.yaml "+path, ruleType, items)
	}
	if checked == 0 {
		t.Fatal("no default rules found in values.yaml; this test is pinning nothing")
	}
}

// scalarPassthroughs are values the template passes through verbatim that have
// no keys to get wrong -- plain lists of strings. They are named here so the
// coverage check below can tell "no keys to check" from "nobody checked".
var scalarPassthroughs = map[string]bool{
	"collect.logs.excludeNamespaces": true,
}

var toYamlValue = regexp.MustCompile(`\.Values\.([A-Za-z0-9_.]+)`)

// rulePaths is a hand-written registry, and a registry that nothing pins goes
// stale the moment a passthrough is added: the new list's examples would go
// unchecked, which is this bug happening again. The template is the authority
// on what passes through, so it is read directly.
func TestEveryPassthroughListIsChecked(t *testing.T) {
	template := chartFile(t, "helm", "kubexa-agent", "templates", "configmap.yaml")

	rendered := map[string]bool{}
	context := ""
	for _, line := range strings.Split(template, "\n") {
		if m := toYamlValue.FindStringSubmatch(line); m != nil {
			context = m[1]
		}
		if !strings.Contains(line, "toYaml") {
			continue
		}
		if context == "" {
			t.Fatalf("toYaml with no preceding .Values path: %q", strings.TrimSpace(line))
		}
		rendered[context] = true
	}
	if len(rendered) == 0 {
		t.Fatal("no toYaml passthroughs found in configmap.yaml; this test is pinning nothing")
	}

	known := rulePaths()
	for path := range rendered {
		if _, ok := known[path]; ok || scalarPassthroughs[path] {
			continue
		}
		t.Errorf("configmap.yaml renders %s with toYaml, so its keys reach the agent "+
			"verbatim, but no test checks them: add it to rulePaths (or to "+
			"scalarPassthroughs if it carries no keys)", path)
	}
	for path := range known {
		if !rendered[path] {
			t.Errorf("rulePaths lists %s, which the template no longer passes through; "+
				"the check it stands for is now aimed at nothing", path)
		}
	}
}

// commentBlock is a run of comment lines in values.yaml, with the path of the
// last real key above it -- which is what says which struct it illustrates.
type commentBlock struct {
	path string
	body string
}

var keyLine = regexp.MustCompile(`^(\s*)([A-Za-z][A-Za-z0-9_]*):`)

// commentBlocks extracts every comment run from values.yaml, tagging each with
// the dotted path of the nearest preceding uncommented key. Comment bodies are
// stripped of their `#` and dedented so they parse as YAML on their own; prose
// blocks simply fail to parse later and are skipped.
func commentBlocks(source string) []commentBlock {
	blocks := []commentBlock{}
	pathAt := map[int]string{}
	current := ""
	var body []string

	flush := func() {
		if len(body) > 0 && current != "" {
			blocks = append(blocks, commentBlock{path: current, body: dedent(body)})
		}
		body = nil
	}

	for _, line := range strings.Split(source, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if strings.HasPrefix(trimmed, "#") {
			indent := len(line) - len(trimmed)
			text := strings.TrimPrefix(trimmed, "#")
			body = append(body, strings.Repeat(" ", indent)+text)
			continue
		}
		flush()
		m := keyLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		indent, key := len(m[1]), m[2]
		prefix := ""
		if indent > 0 {
			prefix = pathAt[indent-2]
		}
		if prefix != "" {
			current = prefix + "." + key
		} else {
			current = key
		}
		pathAt[indent] = current
	}
	flush()
	return blocks
}

// dedent removes the common leading whitespace of a comment body.
func dedent(lines []string) string {
	common := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if common < 0 || indent < common {
			common = indent
		}
	}
	if common <= 0 {
		return strings.Join(lines, "\n")
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if len(line) >= common {
			line = line[common:]
		}
		out = append(out, strings.TrimRight(line, " "))
	}
	return strings.Join(out, "\n")
}

// The commented-out examples are what an operator copies, so a wrong key here
// is shipped advice, not a typo in a comment.
func TestChartExampleRuleKeysAreAgentKeys(t *testing.T) {
	paths := rulePaths()
	found := map[string]bool{}

	for _, block := range commentBlocks(chartFile(t, "helm", "kubexa-agent", "values.yaml")) {
		ruleType, ok := paths[block.path]
		if !ok {
			continue
		}

		// An example is written either as a bare list of rules, or as the
		// enclosing key repeated with its list underneath.
		var asList []any
		if err := yaml.Unmarshal([]byte(block.body), &asList); err == nil && len(asList) > 0 {
			found[block.path] = true
			checkItems(t, "values.yaml example under "+block.path, ruleType, asList)
			continue
		}
		var asMap map[string]any
		if err := yaml.Unmarshal([]byte(block.body), &asMap); err != nil {
			continue // prose, not an example
		}
		leaf := block.path[strings.LastIndex(block.path, ".")+1:]
		list, _ := asMap[leaf].([]any)
		if len(list) == 0 {
			continue
		}
		found[block.path] = true
		checkItems(t, "values.yaml example under "+block.path, ruleType, list)
	}

	// Named rather than counted: a count stays satisfied when one example
	// moves out of the extractor's reach and an unrelated one appears.
	for _, path := range []string{
		"collect.logs.rules",
		"collect.metrics.customEndpoints",
		"query.rules",
	} {
		if !found[path] {
			t.Errorf("no %s example found in values.yaml; either the example was removed "+
				"or the extraction no longer sees it, and in both cases nothing checks "+
				"the keys an operator copies from there", path)
		}
	}
}

// --set paths reach the same passthrough lists, so the README's install
// commands carry the same keys and the same failure.
var readmeSetKey = regexp.MustCompile(`(collect\.[a-zA-Z]+\.(?:rules|customEndpoints)|query\.rules)\[\d+\]\.([A-Za-z0-9_]+)`)

func TestReadmeSetPathsUseAgentKeys(t *testing.T) {
	paths := rulePaths()
	matches := readmeSetKey.FindAllStringSubmatch(chartFile(t, "README.md"), -1)
	if len(matches) == 0 {
		t.Fatal("no --set rule paths found in README.md; this test is pinning nothing")
	}

	for _, m := range matches {
		ruleType, ok := paths[m[1]]
		if !ok {
			t.Errorf("README --set targets %q, which is not a passthrough list this test knows", m[1])
			continue
		}
		if !acceptedKeys(ruleType)[m[2]] {
			t.Errorf("README: --set %s[N].%s -- %s does not bind %q, so the rule installs "+
				"without it", m[1], m[2], ruleType.Name(), m[2])
		}
	}
}
