package policy_test

import (
	"testing"

	"github.com/kubexa/kubexa-agent/internal/mutate/policy"
	"github.com/kubexa/kubexa-agent/pkg/config"
)

func compile(t *testing.T, enabled bool, rules ...config.MutateRule) *policy.Policy {
	t.Helper()
	cfg := &config.Config{Mutate: config.MutateConfig{Enabled: &enabled, Rules: rules}}
	p, err := policy.Compile(cfg)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return p
}

var podsRef = policy.Ref{Version: "v1", Resource: "pods"}

func TestDisabledRefusesEverything(t *testing.T) {
	p := compile(t, false, config.MutateRule{Resources: []string{"pods"}, Verbs: []string{"delete"}})
	if d := p.Decide(podsRef, policy.VerbDelete, "dev", "web-0"); d.Allowed {
		t.Fatal("a disabled policy must refuse")
	}
}

func TestVerbNotGrantedIsRefused(t *testing.T) {
	p := compile(t, true, config.MutateRule{Resources: []string{"pods"}, Verbs: []string{"patch"}})
	if d := p.Decide(podsRef, policy.VerbDelete, "dev", "web-0"); d.Allowed {
		t.Fatal("delete must not be granted by a patch rule")
	}
}

func TestNamespaceAndNamePatternsBind(t *testing.T) {
	p := compile(t, true, config.MutateRule{
		Namespace: "prod", Resources: []string{"deployments"},
		Names: []string{"api-*"}, Verbs: []string{"restart"},
	})
	ref := policy.Ref{Group: "apps", Version: "v1", Resource: "deployments"}
	if d := p.Decide(ref, policy.VerbRestart, "prod", "api-gateway"); !d.Allowed {
		t.Fatalf("want allowed, got %q", d.Reason)
	}
	if d := p.Decide(ref, policy.VerbRestart, "prod", "worker"); d.Allowed {
		t.Fatal("a name outside the pattern must be refused")
	}
	if d := p.Decide(ref, policy.VerbRestart, "staging", "api-gateway"); d.Allowed {
		t.Fatal("a namespace outside the rule must be refused")
	}
}

// The same choke point the query policy grew after the wildcard incident:
// a ref whose segments are not DNS-1123 never reaches the dynamic client,
// which builds its URL with path.Join and validates nothing.
func TestNonDNS1123RefIsRefused(t *testing.T) {
	p := compile(t, true, config.MutateRule{Resources: []string{"secrets"}, Verbs: []string{"delete"}})
	bad := policy.Ref{Version: "v1", Resource: "../secrets"}
	if d := p.Decide(bad, policy.VerbDelete, "dev", "x"); d.Allowed {
		t.Fatal("a non-DNS-1123 resource must be refused at the gate")
	}
}

// Review finding 3: guard the "empty verbs" case directly, instead of
// relying on TestVerbNotGrantedIsRefused's map-miss to also happen to catch
// it -- a regression reintroducing the query policy's "empty means all"
// default for verbs must fail this test even if every other test in the
// package still passes.
//
// This does NOT compile successfully and then check Decide, unlike the
// other tests here. Compile now calls config.ValidateMutateRules
// unconditionally (see the fix for review findings 1+2, in policy.go's
// Compile), and that validator refuses a rule with no verbs -- the same
// check pkg/config/mutate.go already enforced at config-load time. So an
// empty-Verbs rule can never produce a *Policy to call Decide on; Compile
// itself is the gate that must refuse it, one step earlier than Decide.
// That is a strictly stronger guarantee than "compiles, but denies", so
// this test asserts the gate that actually exists.
func TestCompileRejectsEmptyVerbs(t *testing.T) {
	enabled := true
	cfg := &config.Config{Mutate: config.MutateConfig{
		Enabled: &enabled,
		Rules:   []config.MutateRule{{Resources: []string{"pods"}}},
	}}
	if _, err := policy.Compile(cfg); err == nil {
		t.Fatal("want an error compiling a rule with no verbs, got nil")
	}
}

// Review finding 4: prove the wildcard rejection actually fires from
// Compile, for both the bare and the partial form, using a Go-built
// MutateRule -- not just via config-load-time validation (pkg/config's own
// tests already cover that).
func TestCompileRejectsWildcardResource(t *testing.T) {
	cases := []struct {
		name      string
		resources []string
	}{
		{"bare wildcard", []string{"*"}},
		{"partial wildcard", []string{"apps/*"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enabled := true
			cfg := &config.Config{Mutate: config.MutateConfig{
				Enabled: &enabled,
				Rules:   []config.MutateRule{{Resources: tc.resources, Verbs: []string{"delete"}}},
			}}
			if _, err := policy.Compile(cfg); err == nil {
				t.Fatalf("want an error compiling resources %v, got nil", tc.resources)
			}
		})
	}
}
