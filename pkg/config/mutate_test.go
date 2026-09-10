package config_test

import (
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
