package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestQueryRulesInheritStateRulesWhenUnset(t *testing.T) {
	var cfg Config
	src := `
collect:
  state:
    redact_secrets: true
    rules:
      - namespace: stage
        resources: [pods, services]
        label_selector: app=be
`
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	rules := cfg.QueryRules()
	if len(rules) != 1 {
		t.Fatalf("got %d inherited rules, want 1", len(rules))
	}
	if rules[0].Namespace != "stage" {
		t.Errorf("namespace = %q, want %q", rules[0].Namespace, "stage")
	}
	if strings.Join(rules[0].Resources, ",") != "pods,services" {
		t.Errorf("resources = %v, want [pods services]", rules[0].Resources)
	}
	if rules[0].LabelSelector != "app=be" {
		t.Errorf("label_selector = %q, want %q", rules[0].LabelSelector, "app=be")
	}
	// Inherited rules carry no verbs; both are implied.
	if len(rules[0].Verbs) != 0 {
		t.Errorf("verbs = %v, want empty (both implied)", rules[0].Verbs)
	}
	if !cfg.QueryRedactSecrets() {
		t.Error("redact_secrets should inherit collect.state.redact_secrets=true")
	}
	if !cfg.QueryEnabled() {
		t.Error("query should default to enabled")
	}
}

func TestQueryRulesReplaceStateRulesWhenSet(t *testing.T) {
	var cfg Config
	src := `
collect:
  state:
    rules:
      - namespace: stage
        resources: [pods, services, secrets]
query:
  rules:
    - namespace: prod
      resources: [pods]
      verbs: [list]
`
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	rules := cfg.QueryRules()
	if len(rules) != 1 || rules[0].Namespace != "prod" {
		t.Fatalf("got %+v, want the query rule to replace the state rules", rules)
	}
	if strings.Join(rules[0].Verbs, ",") != "list" {
		t.Errorf("verbs = %v, want [list]", rules[0].Verbs)
	}
}

func TestQueryInheritanceIsPerFieldNotPerSection(t *testing.T) {
	// A query section that sets only redact_secrets still inherits its rules.
	var cfg Config
	src := `
collect:
  state:
    redact_secrets: true
    rules:
      - namespace: stage
        resources: [pods]
query:
  redact_secrets: false
`
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := cfg.QueryRules(); len(got) != 1 || got[0].Namespace != "stage" {
		t.Errorf("rules = %+v, want the inherited state rule", got)
	}
	if cfg.QueryRedactSecrets() {
		t.Error("query.redact_secrets=false must override collect.state.redact_secrets=true")
	}
}

func TestQueryEnabledFalseIsDistinctFromUnset(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("query:\n  enabled: false\n"), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.QueryEnabled() {
		t.Error("explicit enabled:false must disable query")
	}

	var unset Config
	if err := yaml.Unmarshal([]byte("query: {}\n"), &unset); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !unset.QueryEnabled() {
		t.Error("unset enabled must default to true")
	}
}

func TestValidateQueryRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "star in the middle of a name pattern",
			src:  "query:\n  rules:\n    - resources: [pods]\n      names: [\"be-*-x\"]\n",
			want: "only as a trailing",
		},
		{
			name: "star in the middle of a namespace pattern",
			src:  "query:\n  rules:\n    - namespace: \"a*b\"\n      resources: [pods]\n",
			want: "only as a trailing",
		},
		{
			name: "unknown verb",
			src:  "query:\n  rules:\n    - resources: [pods]\n      verbs: [delete]\n",
			want: "unsupported verb",
		},
		{
			name: "unparseable resource",
			src:  "query:\n  rules:\n    - resources: [\"not a resource\"]\n",
			want: "unsupported resource",
		},
		{
			name: "rule with no resources",
			src:  "query:\n  rules:\n    - namespace: stage\n",
			want: "at least one resource",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte(tc.src), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			err := cfg.ValidateQuery()
			if err == nil {
				t.Fatalf("ValidateQuery() = nil, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ValidateQuery() = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateQueryAcceptsGoodInput(t *testing.T) {
	var cfg Config
	src := `
query:
  rules:
    - namespace: "team-*"
      resources: [pods, "monitoring.coreos.com/v1/prometheusrules"]
      names: ["be-*", "exact-name"]
      verbs: [list, get]
`
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := cfg.ValidateQuery(); err != nil {
		t.Fatalf("ValidateQuery() = %v, want nil", err)
	}
}

func TestValidateAggregatesQueryViolationsWithOtherSections(t *testing.T) {
	// Verify that query violations aggregate with other config sections
	// in a single *ValidationError, not short-circuiting on query errors.
	var cfg Config
	src := `
gateway:
  address: ""
query:
  rules:
    - resources: [pods]
      verbs: [delete]
`
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("Validate() = nil, want error")
	}
	errMsg := err.Error()
	// Both violations must be in the error message
	if !strings.Contains(errMsg, "gateway.address") {
		t.Errorf("Validate() error should contain gateway.address violation: %v", err)
	}
	if !strings.Contains(errMsg, "unsupported verb") {
		t.Errorf("Validate() error should contain query verb violation: %v", err)
	}
}
