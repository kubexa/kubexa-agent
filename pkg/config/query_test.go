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
	// +1 for the implicit metrics usage rule that QueryRules always appends.
	if len(rules) != 2 {
		t.Fatalf("got %d inherited rules, want 2 (the inherited rule plus the implicit metrics rule)", len(rules))
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
	// +1 for the implicit metrics usage rule that QueryRules always appends.
	if len(rules) != 2 || rules[0].Namespace != "prod" {
		t.Fatalf("got %+v, want the query rule to replace the state rules", rules)
	}
	if strings.Join(rules[0].Verbs, ",") != "list" {
		t.Errorf("verbs = %v, want [list]", rules[0].Verbs)
	}
	if !strings.HasPrefix(rules[1].ID, metricsUsageRuleID) {
		t.Errorf("rules[1].ID = %q, want prefix %q (the mirrored metrics rule)", rules[1].ID, metricsUsageRuleID)
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
	// +1 for the implicit metrics usage rule that QueryRules always appends.
	if got := cfg.QueryRules(); len(got) != 2 || got[0].Namespace != "stage" {
		t.Errorf("rules = %+v, want the inherited state rule plus the implicit metrics rule", got)
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

// The cpu/memory columns of a live listing read metrics.k8s.io through this
// same query path. The grant is implicit rather than written into the
// chart's query.rules -- a non-empty query.rules cancels the collect.state
// inheritance below, which would leave a cluster able to read metrics and
// nothing else -- but it must never be BROADER than what the owner already
// granted the object listing itself. The tests below mirror the finding: the
// old implicit rule was one unscoped grant for every namespace and every
// node; it is now one mirror per owner rule, carrying that rule's own scope.

func TestQueryRulesMirrorScopedOwnerRuleIntoOneMetricsRule(t *testing.T) {
	cfg := &Config{}
	cfg.Query.Rules = []QueryRule{{
		ID:            "be",
		Namespace:     "stage",
		Resources:     []string{"pods"},
		Names:         []string{"be-*"},
		LabelSelector: "app=be",
	}}

	rules := cfg.QueryRules()

	// The owner's own rule must still be first; the mirror only adds.
	if len(rules) != 2 || rules[0].ID != "be" {
		t.Fatalf("got %+v, want the owner's rule first plus exactly one mirror", rules)
	}
	mirror := rules[1]
	if !strings.HasPrefix(mirror.ID, metricsUsageRuleID) {
		t.Errorf("mirror ID = %q, want prefix %q", mirror.ID, metricsUsageRuleID)
	}
	if strings.Join(mirror.Resources, ",") != "metrics.k8s.io/v1beta1/pods" {
		t.Errorf("resources = %v, want only the pods metrics resource (no nodes rule)", mirror.Resources)
	}
	if mirror.Namespace != "stage" {
		t.Errorf("namespace = %q, want %q", mirror.Namespace, "stage")
	}
	if strings.Join(mirror.Names, ",") != "be-*" {
		t.Errorf("names = %v, want [be-*]", mirror.Names)
	}
	if strings.Join(mirror.Verbs, ",") != "list" {
		t.Errorf("verbs = %v, want [list]", mirror.Verbs)
	}
	// LabelSelector must be carried over too -- metrics-server's PodMetrics
	// endpoint genuinely honours it (verified against a live cluster), so
	// dropping it here would silently widen what the metrics query returns
	// relative to the object rule that authorized it.
	if mirror.LabelSelector != "app=be" {
		t.Errorf("label_selector = %q, want %q", mirror.LabelSelector, "app=be")
	}
}

func TestQueryRulesMirrorUnrestrictedRuleIntoPodsAndNodesMetricsRules(t *testing.T) {
	cfg := &Config{}
	cfg.Query.Rules = []QueryRule{{ID: "own", Resources: []string{"pods", "nodes"}}}

	rules := cfg.QueryRules()

	if len(rules) != 3 || rules[0].ID != "own" {
		t.Fatalf("got %+v, want the owner's rule first plus two mirrors", rules)
	}
	var sawPods, sawNodes bool
	for _, r := range rules[1:] {
		if !strings.HasPrefix(r.ID, metricsUsageRuleID) {
			t.Errorf("mirror ID = %q, want prefix %q", r.ID, metricsUsageRuleID)
		}
		if r.Namespace != "" || len(r.Names) != 0 {
			t.Errorf("rule %+v should stay unrestricted, mirroring the owner's unrestricted rule", r)
		}
		switch strings.Join(r.Resources, ",") {
		case "metrics.k8s.io/v1beta1/pods":
			sawPods = true
		case "metrics.k8s.io/v1beta1/nodes":
			sawNodes = true
		}
	}
	if !sawPods || !sawNodes {
		t.Fatalf("rules = %+v, want both a pods and a nodes metrics mirror", rules)
	}
}

func TestQueryRulesMirrorResourceAliasForms(t *testing.T) {
	// "pods", "pod" and "v1/pods" all resolve to the same GVR via
	// k8sresource.Parse; the mirror must recognise all of them, not just the
	// literal string "pods".
	for _, alias := range []string{"pod", "v1/pods"} {
		t.Run(alias, func(t *testing.T) {
			cfg := &Config{}
			cfg.Query.Rules = []QueryRule{{Resources: []string{alias}}}
			rules := cfg.QueryRules()
			if len(rules) != 2 || strings.Join(rules[1].Resources, ",") != "metrics.k8s.io/v1beta1/pods" {
				t.Fatalf("got %+v, want the alias recognised and mirrored to metrics.k8s.io/v1beta1/pods", rules)
			}
		})
	}
}

func TestQueryRulesGetOnlyRuleProducesNoMetricsMirror(t *testing.T) {
	cfg := &Config{}
	cfg.Query.Rules = []QueryRule{{Resources: []string{"pods"}, Verbs: []string{"get"}}}

	rules := cfg.QueryRules()
	if len(rules) != 1 {
		t.Fatalf("got %+v, want no mirror: a get-only rule permits no listing for a metrics column to attach to", rules)
	}
}

func TestQueryRulesFieldSelectorRuleProducesNoMetricsMirror(t *testing.T) {
	cfg := &Config{}
	cfg.Query.Rules = []QueryRule{{Resources: []string{"pods"}, FieldSelector: "spec.nodeName=x"}}

	rules := cfg.QueryRules()
	if len(rules) != 1 {
		t.Fatalf("got %+v, want no mirror: metrics-server's PodMetrics fieldSelector accepts only "+
			"metadata.name/metadata.namespace and hard-400s on anything else (verified against a live "+
			"cluster), and dropping just the selector would silently widen the owner's scope", rules)
	}
}

// A field-selector rule that permits listing a resource captures that
// resource's object LIST too, because Decide's rule selection does not
// consider field selectors -- it is the executor's row filter, not the rule
// match, that applies the selector. So a later, broader rule for the same
// resource is unreachable for the object listing, and mirroring ITS metrics
// grant would answer with more than the effective object policy allows.
// Verified against the compiled policy: with these two rules in force, a
// live "list pods" query is allowed with fieldSel="status.phase=Running",
// so rule b never decides a pods query and must not seed a metrics mirror
// either.
func TestQueryRulesFieldSelectorRuleBlocksLaterBroaderRuleFromMirroring(t *testing.T) {
	cfg := &Config{}
	cfg.Query.Rules = []QueryRule{
		{ID: "a", Resources: []string{"pods"}, FieldSelector: "status.phase=Running"},
		{ID: "b", Resources: []string{"pods"}},
	}

	rules := cfg.QueryRules()

	for _, r := range rules {
		if strings.HasPrefix(r.ID, metricsUsageRuleID) {
			t.Fatalf("got %+v, want no pods metrics mirror at all: rule b is unreachable for pods "+
				"because rule a's field selector already captures every pods query", rules)
		}
	}
	// The two owner rules themselves must be untouched.
	if len(rules) != 2 || rules[0].ID != "a" || rules[1].ID != "b" {
		t.Fatalf("got %+v, want exactly the two owner rules and nothing else", rules)
	}
}

func TestQueryRulesUnparseableResourceEntryIsSkippedNotFatal(t *testing.T) {
	cfg := &Config{}
	cfg.Query.Rules = []QueryRule{{Resources: []string{"nonsense", "pods"}}}

	rules := cfg.QueryRules()

	var sawMirror bool
	for _, r := range rules {
		if strings.HasPrefix(r.ID, metricsUsageRuleID) {
			sawMirror = true
			if strings.Join(r.Resources, ",") != "metrics.k8s.io/v1beta1/pods" {
				t.Errorf("resources = %v, want only the pods metrics resource", r.Resources)
			}
		}
	}
	if !sawMirror {
		t.Fatalf("got %+v, want the unparseable entry skipped and the pods mirror still produced", rules)
	}
}

func TestQueryRulesUnrelatedResourceProducesNoMetricsMirror(t *testing.T) {
	cfg := &Config{}
	cfg.Query.Rules = []QueryRule{{Resources: []string{"services"}}}

	rules := cfg.QueryRules()
	if len(rules) != 1 {
		t.Fatalf("got %+v, want no mirror for a rule that grants neither pods nor nodes", rules)
	}
}

func TestQueryRulesMirrorInheritedStateRulesToo(t *testing.T) {
	cfg := &Config{}
	cfg.Collect.State.Rules = []StateNamespaceRule{{ID: "pods", Namespace: "stage", Resources: []string{"pods"}}}

	rules := cfg.QueryRules()

	// The inherited rule must still be there: the mirror only adds.
	if len(rules) != 2 || rules[0].ID != "pods" {
		t.Fatalf("got %+v, want the inherited rule first plus one mirror", rules)
	}
	mirror := rules[1]
	if !strings.HasPrefix(mirror.ID, metricsUsageRuleID) {
		t.Errorf("mirror ID = %q, want prefix %q", mirror.ID, metricsUsageRuleID)
	}
	if mirror.Namespace != "stage" {
		t.Errorf("namespace = %q, want %q", mirror.Namespace, "stage")
	}
	if strings.Join(mirror.Resources, ",") != "metrics.k8s.io/v1beta1/pods" {
		t.Errorf("resources = %v, want only pods (no nodes rule)", mirror.Resources)
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
