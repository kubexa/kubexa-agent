package policy_test

import (
	"strings"
	"testing"

	"github.com/kubexa/kubexa-agent/internal/exec/policy"
	"github.com/kubexa/kubexa-agent/pkg/config"
)

func on() *bool { b := true; return &b }

func compile(t *testing.T, enabled bool, rules ...config.PodExecRule) *policy.Policy {
	t.Helper()
	c := &config.Config{}
	if enabled {
		c.Exec.Pod.Enabled = on()
	}
	c.Exec.Pod.Rules = rules
	p, err := policy.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDisabledDeniesEverything(t *testing.T) {
	p := compile(t, false, config.PodExecRule{})
	d := p.Decide("dev", "web-1", "nginx")
	if d.Allowed || !strings.Contains(d.Reason, "disabled") {
		t.Fatalf("got %+v", d)
	}
	if p.AllowsAnyPod() {
		t.Fatal("disabled policy must report no exec capability")
	}
}

func TestNoRulesDeniesEverything(t *testing.T) {
	p := compile(t, true)
	if d := p.Decide("dev", "web-1", "nginx"); d.Allowed {
		t.Fatalf("enabled with no rules must deny, got %+v", d)
	}
}

func TestFirstMatchingRuleAuthorizes(t *testing.T) {
	p := compile(t, true,
		config.PodExecRule{ID: "prod-ro", Namespace: "prod", Names: []string{"api-*"}, Containers: []string{"app"}},
		config.PodExecRule{ID: "dev-all", Namespace: "dev-*"},
	)
	cases := []struct {
		ns, pod, ctr string
		allowed      bool
		rule         string
	}{
		{"prod", "api-1", "app", true, "prod-ro"},
		{"prod", "api-1", "sidecar", false, ""},
		{"prod", "web-1", "app", false, ""},
		{"dev-a", "anything", "anything", true, "dev-all"},
		{"staging", "api-1", "app", false, ""},
	}
	for _, tc := range cases {
		d := p.Decide(tc.ns, tc.pod, tc.ctr)
		if d.Allowed != tc.allowed || d.RuleID != tc.rule {
			t.Errorf("Decide(%q,%q,%q) = %+v, want allowed=%v rule=%q", tc.ns, tc.pod, tc.ctr, d, tc.allowed, tc.rule)
		}
	}
	if !p.AllowsAnyPod() {
		t.Fatal("a policy with rules must report exec capability")
	}
}

func TestEmptyContainerIsNotAMatchAll(t *testing.T) {
	p := compile(t, true, config.PodExecRule{Containers: []string{"app"}})
	if d := p.Decide("dev", "web", ""); d.Allowed {
		t.Fatal("an empty container name must not satisfy a containers: allowlist")
	}
}

func TestCompileValidatesEvenWhenDisabled(t *testing.T) {
	c := &config.Config{}
	c.Exec.Pod.Rules = []config.PodExecRule{{Names: []string{"a*b"}}}
	if _, err := policy.Compile(c); err == nil {
		t.Fatal("an invalid rule must fail Compile even with the section disabled")
	}
}

func TestNilConfigCompiles(t *testing.T) {
	p, err := policy.Compile(nil)
	if err != nil || p.Decide("a", "b", "c").Allowed {
		t.Fatalf("nil config: err=%v", err)
	}
}
