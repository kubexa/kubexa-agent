package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubexa/kubexa-agent/pkg/config"
)

// A rule key the agent does not know is dropped in silence, and a log rule
// that lost its filters collects everything instead of nothing -- the failure
// looks like an over-broad rule, never like a typo. The chart makes this easy
// to hit: the values around `rules` are camelCase, the keys inside it are the
// agent's own snake_case config keys, because the whole block is passed
// through with toYaml.
func TestUnknownKeysNamesACamelCaseRuleKey(t *testing.T) {
	src := `
collect:
  logs:
    enabled: true
    rules:
      - id: prod-api
        namespace: production
        podNames:
          - api-*
`
	warnings := config.UnknownKeys([]byte(src))

	if len(warnings) != 1 {
		t.Fatalf("UnknownKeys returned %d warnings, want 1: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "podNames") {
		t.Errorf("warning does not name the offending key: %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "LogNamespaceRule") {
		t.Errorf("warning does not name the type that rejected the key, so the operator "+
			"cannot tell which block it came from: %q", warnings[0])
	}
}

func TestUnknownKeysReportsEveryOffender(t *testing.T) {
	src := `
collect:
  logs:
    rules:
      - labelSelector: app=api
  metrics:
    # custom_endpoints, not customEndpoints: this is the RENDERED agent config,
    # where the template has already translated the chart's own camelCase key.
    # Only the passed-through contents of the list can still be wrong.
    custom_endpoints:
      - name: my-app
        extraLabels:
          service: my-app
query:
  rules:
    - resources: [pods]
      labelSelector: app=api
`
	warnings := config.UnknownKeys([]byte(src))

	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"labelSelector", "extraLabels"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning names %q; got:\n%s", want, joined)
		}
	}
	// Two rule blocks use labelSelector, and each is its own mistake to fix.
	if got := strings.Count(joined, "labelSelector"); got != 2 {
		t.Errorf("labelSelector reported %d times, want 2 (one per offending block):\n%s", got, joined)
	}
}

func TestUnknownKeysSilentOnAValidConfig(t *testing.T) {
	src := `
agent:
  tenant_token: t
gateway:
  address: gateway.kubexa.dev:443
collect:
  logs:
    enabled: true
    tail_lines: 100
    exclude_namespaces: [kube-system]
    rules:
      - id: prod-api
        namespace: production
        pod_names: [api-*]
        label_selector: app=api
        containers: [api]
  metrics:
    custom_endpoints:
      - name: my-app
        url: http://my-app:8080/metrics
        interval: 30s
        extra_labels:
          service: my-app
query:
  rules:
    - namespace: stage
      resources: [pods]
      names: ["be-*"]
      verbs: [list, get]
`
	if warnings := config.UnknownKeys([]byte(src)); len(warnings) != 0 {
		t.Errorf("UnknownKeys flagged a valid config: %v", warnings)
	}
}

// A malformed value is the normal parse pass's error to report, and it reports
// it by refusing to start. Repeating it here would attach "unknown key" to a
// key that is known, sending the operator to look for a typo that is not there.
func TestUnknownKeysIgnoresTypeErrors(t *testing.T) {
	src := "collect:\n  logs:\n    tail_lines: not-a-number\n"

	if warnings := config.UnknownKeys([]byte(src)); len(warnings) != 0 {
		t.Errorf("UnknownKeys reported a type error as an unknown key: %v", warnings)
	}
}

// A file too broken to parse has no keys to judge. Load reports it.
func TestUnknownKeysOnUnparseableYAML(t *testing.T) {
	if warnings := config.UnknownKeys([]byte("collect: [unclosed\n")); len(warnings) != 0 {
		t.Errorf("UnknownKeys on unparseable YAML returned %v, want none", warnings)
	}
}

func TestLoadWithWarningsStartsDespiteAnUnknownKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	src := `
agent:
  tenant_token: t
gateway:
  address: gateway.kubexa.dev:443
collect:
  logs:
    rules:
      - id: prod-api
        podNames: [api-*]
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, warnings, err := config.LoadWithWarnings(path)
	if err != nil {
		t.Fatalf("LoadWithWarnings: %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadWithWarnings returned no config")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "podNames") {
		t.Errorf("warnings = %v, want one naming podNames", warnings)
	}
	// The unknown key really was dropped: this is what the warning is for.
	if got := cfg.Collect.Logs.Rules[0].PodNames; len(got) != 0 {
		t.Errorf("pod_names = %v, want empty -- the camelCase key was not supposed to bind", got)
	}
}

func TestLoadWithWarningsQuietOnAValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	src := "agent:\n  tenant_token: t\ngateway:\n  address: gateway.kubexa.dev:443\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, warnings, err := config.LoadWithWarnings(path)
	if err != nil {
		t.Fatalf("LoadWithWarnings: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}
