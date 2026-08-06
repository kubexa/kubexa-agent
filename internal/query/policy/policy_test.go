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

func TestDecideCarriesTheAuthorizingRuleNamePatterns(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - namespace: stage
      resources: [pods]
      names: ["be-*", "worker-*"]
`)
	d := p.Decide(podsRef, VerbList, "stage", "")
	if !d.Allowed {
		t.Fatal("the list must be allowed")
	}
	if strings.Join(d.NamePatterns, ",") != "be-*,worker-*" {
		t.Errorf("NamePatterns = %v, want [be-* worker-*]", d.NamePatterns)
	}
}

func TestDecideCarriesNoPatternsWhenTheRuleHasNone(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - namespace: stage
      resources: [pods]
`)
	if got := p.Decide(podsRef, VerbList, "stage", "").NamePatterns; len(got) != 0 {
		t.Errorf("NamePatterns = %v, want empty (rule restricts no names)", got)
	}
}

// MatchesName is a pure function over the patterns Decide handed back. It has
// no access to the rule list, which is what makes it impossible for row
// filtering to land on a different rule than the one that authorized the query.
func TestMatchesNameIsPureOverPatterns(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		object   string
		want     bool
	}{
		{"prefix hit", []string{"be-*"}, "be-1", true},
		{"prefix miss", []string{"be-*"}, "fe-1", false},
		{"exact hit", []string{"be-1"}, "be-1", true},
		{"exact miss", []string{"be-1"}, "be-2", false},
		{"second pattern hits", []string{"be-*", "worker-*"}, "worker-9", true},
		{"no patterns matches everything", nil, "anything-at-all", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchesName(tc.object, tc.patterns); got != tc.want {
				t.Errorf("MatchesName(%q, %v) = %v, want %v", tc.object, tc.patterns, got, tc.want)
			}
		})
	}
}

// A regression test for the divergence a second rule-walking filter would
// reintroduce. An all-namespaces LIST can only be authorized by the
// namespaceless rule, so every returned row must be filtered by THAT rule's
// "safe-*" restriction -- including rows from the prod namespace, which the
// first rule would have admitted unrestricted had filtering re-selected a rule
// per row.
func TestAllNamespacesListFiltersByTheAuthorizingRuleOnly(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - namespace: prod
      resources: [secrets]
      verbs: [list]
    - resources: [secrets]
      names: ["safe-*"]
      verbs: [list]
`)
	d := p.Decide(secretsRef, VerbList, "", "")
	if !d.Allowed {
		t.Fatal("the namespaceless rule must authorize an all-namespaces list")
	}
	if MatchesName("db-password", d.NamePatterns) {
		t.Error("prod/db-password must be filtered out: the authorizing rule restricts names to safe-*, " +
			"and a direct Decide(get) for the same object is denied")
	}
	if !MatchesName("safe-token", d.NamePatterns) {
		t.Error("safe-token matches the authorizing rule's pattern and must survive filtering")
	}
	if p.Decide(secretsRef, VerbGet, "prod", "db-password").Allowed {
		t.Error("a direct get of prod/db-password must be denied, which is why the list must not leak it")
	}
}

func TestDecideIsDeniedWhenQueryIsDisabled(t *testing.T) {
	p := compile(t, `
query:
  enabled: false
  rules:
    - resources: [pods]
`)
	d := p.Decide(podsRef, VerbList, "stage", "")
	if d.Allowed {
		t.Error("a disabled policy must deny, leaving nothing to filter")
	}
	if len(d.NamePatterns) != 0 {
		t.Error("a denied decision must carry no name patterns")
	}
}

func TestDecideAcceptsUppercaseVerbsFromConfig(t *testing.T) {
	// Verbs are validated upstream but stored unnormalized, so a config may
	// legitimately contain "LIST".
	p := compile(t, `
query:
  rules:
    - namespace: stage
      resources: [pods]
      verbs: [LIST]
`)
	if !p.Decide(podsRef, VerbList, "stage", "").Allowed {
		t.Error("an uppercase verb in config must still grant list")
	}
	if p.Decide(podsRef, VerbGet, "stage", "x").Allowed {
		t.Error("verbs: [LIST] must not grant get")
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

var crdRef = Ref{Group: "monitoring.coreos.com", Version: "v1", Resource: "prometheusrules"}

func TestWildcardAllowsUnknownCRD(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - id: everything
      resources: ["*"]
`)
	for _, verb := range []Verb{VerbList, VerbGet} {
		name := ""
		if verb == VerbGet {
			name = "some-rule"
		}
		if d := p.Decide(crdRef, verb, "stage", name); !d.Allowed {
			t.Errorf("Decide(%s) = %+v, want allowed", verb, d)
		}
	}
}

func TestWildcardStillHonoursNamespace(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - id: stage-all
      namespace: stage
      resources: ["*"]
`)
	if p.Decide(crdRef, VerbList, "prod", "").Allowed {
		t.Error("a namespace-scoped wildcard must not answer another namespace")
	}
	// A cluster-scoped read carries no namespace and is granted only by a rule
	// that names none -- unchanged by the wildcard.
	if p.Decide(nodesRef, VerbList, "", "").Allowed {
		t.Error("a namespace-scoped wildcard must not answer a cluster-scoped read")
	}
}

func TestWildcardStillHonoursVerbs(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - id: everything
      resources: ["*"]
      verbs: [list]
`)
	if !p.Decide(crdRef, VerbList, "stage", "").Allowed {
		t.Error("list must be allowed")
	}
	if p.Decide(crdRef, VerbGet, "stage", "some-rule").Allowed {
		t.Error("get must be denied when only list is granted")
	}
}

// Name patterns must reach the Decision so the executor filters LIST rows
// against the same rule that permitted the query.
func TestWildcardCarriesNamePatterns(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - id: everything
      resources: ["*"]
      names: ["be-*"]
`)
	d := p.Decide(podsRef, VerbList, "stage", "")
	if !d.Allowed {
		t.Fatalf("Decide = %+v, want allowed", d)
	}
	if !MatchesName("be-api", d.NamePatterns) {
		t.Error("be-api must match")
	}
	if MatchesName("fe-web", d.NamePatterns) {
		t.Error("fe-web must not match")
	}
}

func TestWildcardDeniedWhenQueryDisabled(t *testing.T) {
	p := compile(t, `
query:
  enabled: false
  rules:
    - id: everything
      resources: ["*"]
`)
	if p.Decide(crdRef, VerbList, "stage", "").Allowed {
		t.Error("query.enabled=false must still refuse everything")
	}
}

func TestWildcardMakesCapabilityReportAllowAnyGVR(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - id: everything
      resources: ["*"]
`)
	if !p.AllowsAnyList("example.io", "v1alpha1", "widgets") {
		t.Error("AllowsAnyList must report true for an unknown GVR under a wildcard")
	}
	if !p.AllowsAnyGet("example.io", "v1alpha1", "widgets") {
		t.Error("AllowsAnyGet must report true for an unknown GVR under a wildcard")
	}
}

func TestWildcardRuleIDs(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - id: named-pods
      resources: [pods]
    - id: everything
      resources: ["*"]
`)
	ids := p.WildcardRuleIDs()
	if len(ids) != 1 || ids[0] != "everything" {
		t.Fatalf("WildcardRuleIDs = %v, want [everything]", ids)
	}

	off := compile(t, `
query:
  enabled: false
  rules:
    - id: everything
      resources: ["*"]
`)
	if len(off.WildcardRuleIDs()) != 0 {
		t.Error("a disabled policy permits nothing and must report no wildcard rules")
	}
}

// A rule with no id still has to be nameable in the startup warning.
func TestWildcardRuleIDsFallBackToIndex(t *testing.T) {
	p := compile(t, `
query:
  rules:
    - resources: ["*"]
`)
	ids := p.WildcardRuleIDs()
	if len(ids) != 1 || ids[0] != "#0" {
		t.Fatalf("WildcardRuleIDs = %v, want [#0]", ids)
	}
}
