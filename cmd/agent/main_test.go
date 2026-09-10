package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/pkg/config"
)

// invalidMutateRule fails ValidateMutateRules for the simplest possible
// reason (no verbs named) -- see pkg/config/mutate.go's ValidateMutateRules,
// which mutatepolicy.Compile calls unconditionally, even for a disabled
// mutate section.
func invalidMutateRule() config.MutateRule {
	return config.MutateRule{Resources: []string{"pods"}}
}

// TestCompileMutatePolicyFatalWhenEnabled pins the half of the ruling that
// matches the query policy: an operator who turned mutate ON and shipped a
// broken rule cannot have mutations, so the error must propagate (serve
// treats it as fatal, exactly like the query policy's own compile error).
func TestCompileMutatePolicyFatalWhenEnabled(t *testing.T) {
	enabled := true
	cfg := &config.Config{Mutate: config.MutateConfig{
		Enabled: &enabled,
		Rules:   []config.MutateRule{invalidMutateRule()},
	}}

	var buf bytes.Buffer
	log := logger.New("test", logger.WithWriter(&buf))

	p, err := compileMutatePolicy(cfg, log)
	if err == nil {
		t.Fatal("compileMutatePolicy() error = nil, want an error for an invalid rule with mutate enabled")
	}
	if p != nil {
		t.Fatalf("compileMutatePolicy() policy = %+v, want nil alongside a fatal error", p)
	}
}

// TestCompileMutatePolicyWarnsAndContinuesWhenDisabled pins the ruling this
// task exists to implement: a leftover invalid rule under a DISABLED mutate
// section must never crash-loop the agent (the chart's restartPolicy:
// Always would turn that into total agent unavailability -- logs, metrics,
// live query, everything). It must be a warning naming the offending rule,
// and the agent must start with mutation answered by a nil responder.
func TestCompileMutatePolicyWarnsAndContinuesWhenDisabled(t *testing.T) {
	disabled := false
	cfg := &config.Config{Mutate: config.MutateConfig{
		Enabled: &disabled,
		Rules:   []config.MutateRule{invalidMutateRule()},
	}}

	var buf bytes.Buffer
	log := logger.New("test", logger.WithWriter(&buf))

	p, err := compileMutatePolicy(cfg, log)
	if err != nil {
		t.Fatalf("compileMutatePolicy() error = %v, want nil -- a disabled section's bad rule must not be fatal", err)
	}
	if p != nil {
		t.Fatalf("compileMutatePolicy() policy = %+v, want nil -- mutation must stay disabled", p)
	}

	logged := buf.String()
	if !strings.Contains(logged, "mutate.rules[0]") {
		t.Fatalf("warning log = %q, want it to name the offending rule (mutate.rules[0])", logged)
	}
	if !strings.Contains(strings.ToLower(logged), "warn") {
		t.Fatalf("warning log = %q, want it logged at warn level", logged)
	}
}
