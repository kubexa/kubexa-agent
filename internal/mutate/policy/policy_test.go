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

// An empty name must not short-circuit the names: allowlist. A rule
// restricting deletes to "nginx-*" was matching an empty-name delete on
// anything in the namespace -- what actually refused it was client-go's
// own "name is required", not this policy. A rule with no names: at all
// must keep allowing an empty name, since it never restricted names in
// the first place.
func TestEmptyNameConsultsNamesAllowlist(t *testing.T) {
	restricted := compile(t, true, config.MutateRule{
		Resources: []string{"pods"}, Names: []string{"nginx-*"}, Verbs: []string{"delete"},
	})
	if d := restricted.Decide(podsRef, policy.VerbDelete, "dev", ""); d.Allowed {
		t.Fatal("an empty name must not bypass a rule's names: allowlist")
	}

	unrestricted := compile(t, true, config.MutateRule{
		Resources: []string{"pods"}, Verbs: []string{"delete"},
	})
	if d := unrestricted.Decide(podsRef, policy.VerbDelete, "dev", ""); !d.Allowed {
		t.Fatalf("a rule with no names: must still allow an empty name, got %q", d.Reason)
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
// Compile, using a Go-built MutateRule -- not just via config-load-time
// validation (pkg/config's own tests already cover that).
//
// A bare "*" case was deliberately removed from this test (it was here
// originally, as a second subtest alongside this one). It does not belong:
// verified by temporarily deleting the `trimmed == ResourceWildcard` branch
// in pkg/config's ValidateMutateRules and re-running pkg/config's own
// TestWildcardResourceIsRefused, which stayed GREEN -- because the very next
// check, `strings.Contains(trimmed, ResourceWildcard)`, also matches a bare
// "*" (a string always contains itself) and its message contains "wildcard"
// too, so the two checks are redundant for the bare form specifically. A
// Compile-level subtest built the same way this partial-wildcard case is
// built would therefore pass whether or not any wildcard-specific check
// exists at all: k8sresource.Parse("*") independently returns "unsupported
// resource" for a bare "*" (no "/", not a registered alias), so Compile
// would still error out on it via that unrelated path. Such a subtest cannot
// fail for the reason its name claims, so it was removed rather than kept as
// a passing test that pins nothing. The bare form is exercised at the layer
// where it can actually be isolated: pkg/config/mutate_test.go's
// TestWildcardResourceIsRefused, which is a config-load-time-shaped test
// (via ValidateMutateForTest), not this package's Compile-shaped one.
func TestCompileRejectsWildcardResource(t *testing.T) {
	enabled := true
	cfg := &config.Config{Mutate: config.MutateConfig{
		Enabled: &enabled,
		Rules:   []config.MutateRule{{Resources: []string{"apps/*"}, Verbs: []string{"delete"}}},
	}}
	if _, err := policy.Compile(cfg); err == nil {
		t.Fatal("want an error compiling resources [apps/*], got nil")
	}
}
