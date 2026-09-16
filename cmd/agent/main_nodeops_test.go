package main

import (
	"errors"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/pkg/config"
)

func nodeOpsCfg(enabled bool, verbs ...string) *config.Config {
	c := &config.Config{}
	c.Mutate.Node = config.NodeMutateConfig{Enabled: &enabled, Nodes: []string{"*"}, Verbs: verbs}
	return c
}

func TestCompileNodeOpsPolicyRuling(t *testing.T) {
	log := logger.New("test")
	if _, err := compileNodeOpsPolicy(nodeOpsCfg(true, "bogus"), log); err == nil {
		t.Fatal("enabled + invalid must be fatal")
	}
	p, err := compileNodeOpsPolicy(nodeOpsCfg(false, "bogus"), log)
	if err != nil || p != nil {
		t.Fatalf("disabled + invalid must be a warning and a nil policy, got %v %v", p, err)
	}
	if p, err := compileNodeOpsPolicy(nodeOpsCfg(true, "drain"), log); err != nil || p == nil {
		t.Fatalf("valid: %v %v", p, err)
	}
}

func TestBuildNodeJobResponder(t *testing.T) {
	log := logger.New("test")
	calls := 0
	factory := func() (kubernetes.Interface, error) { calls++; return fake.NewSimpleClientset(), nil }

	// disabled: no clients built, nil responder
	p, _ := compileNodeOpsPolicy(nodeOpsCfg(false, "drain"), log)
	r, err := buildNodeJobResponder(nodeOpsCfg(false, "drain"), p, factory, log)
	if err != nil || r != nil || calls != 0 {
		t.Fatalf("disabled: r=%v err=%v calls=%d", r, err, calls)
	}

	// enabled: clients built once, responder returned
	cfg := nodeOpsCfg(true, "drain")
	p, _ = compileNodeOpsPolicy(cfg, log)
	r, err = buildNodeJobResponder(cfg, p, factory, log)
	if err != nil || r == nil || calls != 1 {
		t.Fatalf("enabled: r=%v err=%v calls=%d", r, err, calls)
	}

	// enabled but the policy is nil (unreachable through compile, a backstop)
	if _, err := buildNodeJobResponder(cfg, nil, factory, log); err == nil || !strings.Contains(err.Error(), "mutate.node") {
		t.Fatalf("nil policy while enabled: %v", err)
	}

	// client factory failure is fatal
	if _, err := buildNodeJobResponder(cfg, p, func() (kubernetes.Interface, error) { return nil, errors.New("boom") }, log); err == nil {
		t.Fatal("factory error swallowed")
	}
}
