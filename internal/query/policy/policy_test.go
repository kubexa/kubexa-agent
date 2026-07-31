package policy

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
)

func compile(t *testing.T, src string) *Policy {
	t.Helper()
	var cfg pkgconfig.Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	p, err := Compile(&cfg)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return p
}

var podsRef = Ref{Group: "", Version: "v1", Resource: "pods"}
var secretsRef = Ref{Group: "", Version: "v1", Resource: "secrets"}
var nodesRef = Ref{Group: "", Version: "v1", Resource: "nodes"}

func TestDecideAllowsMatchingRule(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - namespace: stage
      resources: [pods]
      verbs: [list, get]
`)
	d := p.Decide(podsRef, VerbList, "stage", "")
	if !d.Allowed {
		t.Fatalf("Decide = %+v, want allowed", d)
	}
}

func TestDecideDeniesUnlistedResource(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - namespace: stage
      resources: [pods]
`)
	d := p.Decide(secretsRef, VerbList, "stage", "")
	if d.Allowed {
		t.Fatal("secrets must be denied when only pods is listed")
	}
	if d.Reason == "" {
		t.Error("a denial must carry a human-readable reason")
	}
}

func TestDecideDeniesUnlistedNamespace(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - namespace: stage
      resources: [pods]
`)
	if p.Decide(podsRef, VerbList, "prod", "").Allowed {
		t.Fatal("namespace prod must be denied")
	}
}

func TestDecideEmptyRuleNamespaceMatchesEverything(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - resources: [pods]
`)
	for _, ns := range []string{"stage", "prod", ""} {
		if !p.Decide(podsRef, VerbList, ns, "").Allowed {
			t.Errorf("namespace %q must be allowed by a rule with no namespace", ns)
		}
	}
}

func TestDecideNamespacePrefixPattern(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - namespace: "team-*"
      resources: [pods]
`)
	if !p.Decide(podsRef, VerbList, "team-alpha", "").Allowed {
		t.Error("team-alpha must match team-*")
	}
	if p.Decide(podsRef, VerbList, "other", "").Allowed {
		t.Error("other must not match team-*")
	}
}

func TestDecideNamePrefixPattern(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - namespace: stage
      resources: [pods]
      names: ["be-*"]
`)
	if !p.Decide(podsRef, VerbGet, "stage", "be-api-1").Allowed {
		t.Error("be-api-1 must match be-*")
	}
	if p.Decide(podsRef, VerbGet, "stage", "fe-web-1").Allowed {
		t.Error("fe-web-1 must not match be-*")
	}
	// A LIST carries no name; the name constraint cannot deny it here. It is
	// applied to the returned rows by the executor instead.
	if !p.Decide(podsRef, VerbList, "stage", "").Allowed {
		t.Error("a LIST must not be denied by a name pattern")
	}
}

func TestDecideVerbRestriction(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - namespace: stage
      resources: [secrets]
      verbs: [list]
`)
	if !p.Decide(secretsRef, VerbList, "stage", "").Allowed {
		t.Error("list must be allowed")
	}
	if p.Decide(secretsRef, VerbGet, "stage", "db").Allowed {
		t.Error("get must be denied when verbs is [list]")
	}
}

func TestDecideEmptyVerbsMeansBoth(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - namespace: stage
      resources: [pods]
`)
	if !p.Decide(podsRef, VerbList, "stage", "").Allowed {
		t.Error("list must be allowed when verbs is unset")
	}
	if !p.Decide(podsRef, VerbGet, "stage", "x").Allowed {
		t.Error("get must be allowed when verbs is unset")
	}
}

func TestDecideClusterScopedNeedsANamespacelessRule(t *testing.T) {
	// A namespaced rule must never grant a cluster-scoped read. The owner who
	// wrote "namespace: stage" never agreed to let node data leave.
	p := compile(t, `
query:
  rules:
    - namespace: stage
      resources: [pods, nodes]
`)
	if p.Decide(nodesRef, VerbList, "", "").Allowed {
		t.Fatal("cluster-scoped read must be denied by a namespaced rule")
	}

	q := compile(t, `
query:
  rules:
    - resources: [nodes]
`)
	if !q.Decide(nodesRef, VerbList, "", "").Allowed {
		t.Fatal("a rule with no namespace must grant the cluster-scoped read")
	}
}

func TestDecideDisabledRefusesEverything(t *testing.T) {
	p := compile(t, `
query:
  enabled: false
  rules:
    - resources: [pods]
`)
	d := p.Decide(podsRef, VerbList, "stage", "")
	if d.Allowed {
		t.Fatal("enabled:false must refuse every query")
	}
	if !strings.Contains(strings.ToLower(d.Reason), "disabled") {
		t.Errorf("reason = %q, want it to mention that query is disabled", d.Reason)
	}
}

func TestDecideEmptyPolicyDeniesEverything(t *testing.T) {
	p := compile(t, "{}\n")
	if p.Decide(podsRef, VerbList, "stage", "").Allowed {
		t.Fatal("a config with no rules anywhere must deny")
	}
}

func TestDecideInheritsStateRules(t *testing.T) {
	p := compile(t, `
collect:
  state:
    redact_secrets: true
    rules:
      - namespace: stage
        resources: [pods]
`)
	d := p.Decide(podsRef, VerbGet, "stage", "any")
	if !d.Allowed {
		t.Fatal("an inherited state rule must grant both verbs")
	}
	if !d.RedactSecrets {
		t.Error("RedactSecrets must inherit collect.state.redact_secrets")
	}
}

func TestDecideCarriesFirstMatchingRuleSelectors(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - namespace: stage
      resources: [pods]
      label_selector: app=be
    - namespace: stage
      resources: [pods]
      label_selector: app=fe
`)
	d := p.Decide(podsRef, VerbList, "stage", "")
	if d.LabelSelector != "app=be" {
		t.Errorf("LabelSelector = %q, want the first matching rule's %q", d.LabelSelector, "app=be")
	}
}

func TestDecideResolvesCRDGVR(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - resources: ["monitoring.coreos.com/v1/prometheusrules"]
`)
	ref := Ref{Group: "monitoring.coreos.com", Version: "v1", Resource: "prometheusrules"}
	if !p.Decide(ref, VerbList, "", "").Allowed {
		t.Fatal("a CRD named by full GVR must be allowed")
	}
	if p.Decide(podsRef, VerbList, "", "").Allowed {
		t.Fatal("pods must not be allowed by a CRD-only rule")
	}
}

func TestAllowsAnyMirrorsDecide(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - namespace: stage
      resources: [secrets]
      verbs: [list]
`)
	if !p.AllowsAnyList("", "v1", "secrets") {
		t.Error("AllowsAnyList must report true for a granted list verb")
	}
	if p.AllowsAnyGet("", "v1", "secrets") {
		t.Error("AllowsAnyGet must report false when only list is granted")
	}
	if p.AllowsAnyList("", "v1", "pods") {
		t.Error("AllowsAnyList must report false for an ungranted resource")
	}
}

// MatchesName is what the executor uses to filter LIST rows, because Decide
// cannot: a LIST carries no name. These two must agree, or a row the policy
// forbids leaves the cluster.
func TestMatchesNameFiltersListRows(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - namespace: stage
      resources: [pods]
      names: ["be-*"]
`)
	if !p.MatchesName(podsRef, "stage", "be-1") {
		t.Error("be-1 must match")
	}
	if p.MatchesName(podsRef, "stage", "fe-1") {
		t.Error("fe-1 must not match")
	}
	if p.MatchesName(podsRef, "prod", "be-1") {
		t.Error("a row outside the rule's namespace must not match")
	}
	if p.MatchesName(secretsRef, "stage", "be-1") {
		t.Error("a row of an ungranted resource must not match")
	}
}

func TestMatchesNameAllowsEverythingWithNoPatterns(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - namespace: stage
      resources: [pods]
`)
	if !p.MatchesName(podsRef, "stage", "anything-at-all") {
		t.Error("a rule with no name patterns must match every name")
	}
}

func TestMatchesNameIsFalseWhenQueryIsDisabled(t *testing.T) {
	p := compile(t, `
query:
  enabled: false
  rules:
    - resources: [pods]
`)
	if p.MatchesName(podsRef, "stage", "be-1") {
		t.Error("a disabled policy must filter out every row")
	}
}

func TestCompileRejectsInvalidConfig(t *testing.T) {
	var cfg pkgconfig.Config
	src := "query:\n  rules:\n    - resources: [pods]\n      names: [\"a*b\"]\n"
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, err := Compile(&cfg); err == nil {
		t.Fatal("Compile must reject a pattern with a non-trailing star")
	}
}
