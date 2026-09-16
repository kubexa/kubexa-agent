package config_test

import (
	"strings"
	"testing"

	"github.com/kubexa/kubexa-agent/pkg/config"
)

func TestMutateDefaultsToDisabled(t *testing.T) {
	var c config.Config
	if c.MutateEnabled() {
		t.Fatal("mutate must default to disabled")
	}
}

// The inverse of query.rules: an empty verb list grants nothing AND is
// refused at load, so a rule copied from a query rule cannot silently become
// a delete grant.
func TestEmptyVerbsIsAConfigError(t *testing.T) {
	errs := config.ValidateMutateForTest(config.MutateConfig{
		Enabled: boolPtr(true),
		Rules:   []config.MutateRule{{Resources: []string{"deployments"}}},
	})
	if !containsSubstring(errs, "verbs") {
		t.Fatalf("want a verbs error, got %v", errs)
	}
}

func TestWildcardResourceIsRefused(t *testing.T) {
	errs := config.ValidateMutateForTest(config.MutateConfig{
		Enabled: boolPtr(true),
		Rules:   []config.MutateRule{{Resources: []string{"*"}, Verbs: []string{"delete"}}},
	})
	if !containsSubstring(errs, "wildcard") {
		t.Fatalf("want a wildcard error, got %v", errs)
	}
}

// k8sresource.Parse tolerates a partial wildcard like "apps/*" -- it reads as
// an ordinary GVR whose Resource field happens to be "*", nil error. A rule
// like this must be refused too, not just the bare "*".
func TestPartialWildcardResourceIsRefused(t *testing.T) {
	errs := config.ValidateMutateForTest(config.MutateConfig{
		Enabled: boolPtr(true),
		Rules:   []config.MutateRule{{Resources: []string{"apps/*"}, Verbs: []string{"delete"}}},
	})
	if !containsSubstring(errs, "apps/*") {
		t.Fatalf("want a wildcard error naming the entry, got %v", errs)
	}
}

func TestUnknownVerbIsRefused(t *testing.T) {
	errs := config.ValidateMutateForTest(config.MutateConfig{
		Enabled: boolPtr(true),
		Rules:   []config.MutateRule{{Resources: []string{"pods"}, Verbs: []string{"exec"}}},
	})
	if !containsSubstring(errs, "exec") {
		t.Fatalf("want an unknown-verb error, got %v", errs)
	}
}

// containsSubstring already exists in this package at
// pkg/config/collect_test.go:430 -- reuse it, do not redeclare it, or the
// package will not compile. boolPtr does not exist yet.
func boolPtr(b bool) *bool { return &b }

// minimalValidConfig returns a *config.Config that passes Validate() with
// nothing else set -- config.Default() alone does not, because
// agent.tenant_token and gateway.address are required and Default() leaves
// both empty.
func minimalValidConfig(t *testing.T) *config.Config {
	t.Helper()
	c := config.Default()
	c.Agent.TenantToken = "test-token"
	c.Gateway.Address = "gateway.example.com:443"
	if err := c.Validate(); err != nil {
		t.Fatalf("minimalValidConfig: Validate() = %v, want nil", err)
	}
	return c
}

func TestMutateNodeDefaultsOff(t *testing.T) {
	var c config.Config
	if c.MutateNodeEnabled() {
		t.Fatal("mutate.node enabled with nothing written")
	}
	s := c.MutateNodeSettings()
	if s.MaxTimeoutSec != 1800 {
		t.Fatalf("MaxTimeoutSec default = %d, want 1800", s.MaxTimeoutSec)
	}
	if len(s.Verbs) != 0 || len(s.Nodes) != 0 {
		t.Fatalf("settings carry grants nobody wrote: %+v", s)
	}
}

func TestMutateNodeDoesNotInheritMutateEnabled(t *testing.T) {
	c := config.Config{Mutate: config.MutateConfig{Enabled: boolPtr(true)}}
	if c.MutateNodeEnabled() {
		t.Fatal("mutate.enabled true leaked into mutate.node")
	}
	c = config.Config{Mutate: config.MutateConfig{Node: config.NodeMutateConfig{Enabled: boolPtr(true), Nodes: []string{"*"}, Verbs: []string{"cordon"}}}}
	if !c.MutateNodeEnabled() || c.MutateEnabled() {
		t.Fatal("mutate.node.enabled must be read on its own and must not switch mutate.enabled on")
	}
}

func TestValidateMutateNode(t *testing.T) {
	cases := []struct {
		name string
		node config.NodeMutateConfig
		want string // substring of one violation; "" means valid
	}{
		{"disabled section is not validated", config.NodeMutateConfig{Verbs: []string{"bogus"}}, ""},
		{"empty verbs grants nothing", config.NodeMutateConfig{Enabled: boolPtr(true), Nodes: []string{"*"}}, "mutate.node.verbs must name at least one of cordon, uncordon, drain"},
		{"unknown verb", config.NodeMutateConfig{Enabled: boolPtr(true), Nodes: []string{"*"}, Verbs: []string{"evict"}}, `mutate.node.verbs: unknown verb "evict"`},
		{"bad node pattern", config.NodeMutateConfig{Enabled: boolPtr(true), Nodes: []string{"*-worker"}, Verbs: []string{"drain"}}, "mutate.node.nodes:"},
		{"timeout too small", config.NodeMutateConfig{Enabled: boolPtr(true), Nodes: []string{"*"}, Verbs: []string{"drain"}, MaxTimeoutSec: 10}, "mutate.node.max_timeout_sec must be between 30 and 86400"},
		{"timeout too large", config.NodeMutateConfig{Enabled: boolPtr(true), Nodes: []string{"*"}, Verbs: []string{"drain"}, MaxTimeoutSec: 90000}, "mutate.node.max_timeout_sec must be between 30 and 86400"},
		{"empty nodes is allowed (matches nothing, warned at boot)", config.NodeMutateConfig{Enabled: boolPtr(true), Verbs: []string{"cordon"}}, ""},
		{"valid", config.NodeMutateConfig{Enabled: boolPtr(true), Nodes: []string{"aks-*"}, Verbs: []string{"cordon", "uncordon", "drain"}, MaxTimeoutSec: 600}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := config.Config{Mutate: config.MutateConfig{Node: tc.node}}
			got := config.ValidateMutateNodeForTest(&c)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("unexpected violations: %v", got)
				}
				return
			}
			joined := strings.Join(got, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("violations %q do not contain %q", joined, tc.want)
			}
		})
	}
}

// Validate() must consult the node section even when mutate.enabled is
// false -- validateMutate short-circuits for a disabled mutate.rules, and
// that short-circuit must not swallow mutate.node.
func TestValidateReachesMutateNodeWithMutateDisabled(t *testing.T) {
	c := minimalValidConfig(t) // whatever helper the existing tests use to get a Config that passes Validate()
	c.Mutate = config.MutateConfig{Node: config.NodeMutateConfig{Enabled: boolPtr(true), Nodes: []string{"*"}, Verbs: []string{"nope"}}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), `mutate.node.verbs: unknown verb "nope"`) {
		t.Fatalf("Validate() = %v, want the mutate.node violation", err)
	}
}
