package main

import (
	"bytes"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/logger"
	mutatepolicy "github.com/kubexa/kubexa-agent/internal/mutate/policy"
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

// fakeMutateClients returns a k8s.QueryClients whose Dynamic is a fake
// client -- enough to satisfy mutate.New's "dynamic client is required"
// check without touching a real cluster. Nothing in these tests calls
// Execute, so no objects or list kinds need registering.
func fakeMutateClients() k8s.QueryClients {
	return k8s.QueryClients{Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())}
}

// compiledMutatePolicy compiles a valid, non-nil mutate policy -- the
// "what a real serve() call site would hand buildMutationResponder" shape,
// distinct in TYPE (internal/mutate/policy.Policy) from the query policy
// serve() also holds (internal/query/policy.Policy), so the two cannot be
// interchanged without a compile error.
func compiledMutatePolicy(t *testing.T) *mutatepolicy.Policy {
	t.Helper()
	enabled := true
	p, err := mutatepolicy.Compile(&config.Config{Mutate: config.MutateConfig{Enabled: &enabled}})
	if err != nil {
		t.Fatalf("mutatepolicy.Compile: %v", err)
	}
	return p
}

// TestBuildMutationResponderWiresTheMutatePolicy pins that the policy
// buildMutationResponder is handed is exactly the policy that ends up on
// the executor's Options -- not silently substituted or dropped. The
// parameter type (*mutatepolicy.Policy) already rules out handing it the
// query policy by mistake; this pins the identity through the function
// body itself.
func TestBuildMutationResponderWiresTheMutatePolicy(t *testing.T) {
	enabled := true
	cfg := &config.Config{Mutate: config.MutateConfig{Enabled: &enabled}}
	mutatePolicy := compiledMutatePolicy(t)

	responder, opts, err := buildMutationResponder(cfg, mutatePolicy, fakeMutateClients(), logger.New("test"), nil)
	if err != nil {
		t.Fatalf("buildMutationResponder: %v", err)
	}
	if opts.Policy != mutatePolicy {
		t.Fatalf("opts.Policy = %p, want the exact mutatePolicy passed in (%p)", opts.Policy, mutatePolicy)
	}
	if responder == nil {
		t.Fatal("responder = nil, want a non-nil executor when mutate.enabled is true")
	}
}

// TestBuildMutationResponderWiresRedactSecrets pins the second deferred
// item from Task 4: mutate.Options.RedactSecrets must equal
// cfg.QueryRedactSecrets(), in both directions. Hardcoding it to false (or
// deleting the field) would hand live Secret values back on every mutation
// regardless of what the owner configured -- and every other test in this
// package would still pass. See task-5-report.md for the RED/GREEN proof
// that this specific test catches that regression.
func TestBuildMutationResponderWiresRedactSecrets(t *testing.T) {
	for _, want := range []bool{true, false} {
		want := want
		t.Run(map[bool]string{true: "redact_true", false: "redact_false"}[want], func(t *testing.T) {
			enabled := true
			cfg := &config.Config{
				Mutate: config.MutateConfig{Enabled: &enabled},
				Query:  config.QueryConfig{RedactSecrets: &want},
			}
			mutatePolicy := compiledMutatePolicy(t)

			_, opts, err := buildMutationResponder(cfg, mutatePolicy, fakeMutateClients(), logger.New("test"), nil)
			if err != nil {
				t.Fatalf("buildMutationResponder: %v", err)
			}
			if opts.RedactSecrets != want {
				t.Fatalf("opts.RedactSecrets = %v, want %v (cfg.QueryRedactSecrets())", opts.RedactSecrets, want)
			}
		})
	}
}

// TestBuildMutationResponderNilResponderWhenDisabled pins the third
// requirement: the responder is nil when mutate.enabled is false, and
// non-nil when it is true -- an old gateway and a disabled agent must be
// indistinguishable to the dispatcher, and a nil responder is what makes
// handleMutation refuse to answer.
func TestBuildMutationResponderNilResponderWhenDisabled(t *testing.T) {
	disabled := false
	cfg := &config.Config{Mutate: config.MutateConfig{Enabled: &disabled}}

	// A nil policy is exactly what compileMutatePolicy would hand back for
	// a disabled section that compiled cleanly (or warned and swallowed an
	// error) -- buildMutationResponder must not dereference it before
	// checking MutateEnabled.
	responder, opts, err := buildMutationResponder(cfg, nil, fakeMutateClients(), logger.New("test"), nil)
	if err != nil {
		t.Fatalf("buildMutationResponder: %v", err)
	}
	if responder != nil {
		t.Fatalf("responder = %v, want nil when mutate.enabled is false", responder)
	}
	if opts.Policy != nil {
		t.Fatalf("opts.Policy = %v, want nil (the policy passed in was nil)", opts.Policy)
	}
}
