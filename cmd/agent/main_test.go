package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/kubexa/kubexa-agent/internal/exec"
	execpolicy "github.com/kubexa/kubexa-agent/internal/exec/policy"
	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/logger"
	mutatepolicy "github.com/kubexa/kubexa-agent/internal/mutate/policy"
	"github.com/kubexa/kubexa-agent/internal/query/policy"
	"github.com/kubexa/kubexa-agent/pkg/config"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
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

// fakeMutateClientsFactory adapts fakeMutateClients to the
// func() (*k8s.QueryClients, error) shape buildMutationResponder now takes,
// for the tests that don't care whether or how often it was called.
func fakeMutateClientsFactory() func() (*k8s.QueryClients, error) {
	return func() (*k8s.QueryClients, error) {
		c := fakeMutateClients()
		return &c, nil
	}
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

	responder, opts, err := buildMutationResponder(cfg, mutatePolicy, fakeMutateClientsFactory(), logger.New("test"), nil)
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

			_, opts, err := buildMutationResponder(cfg, mutatePolicy, fakeMutateClientsFactory(), logger.New("test"), nil)
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
	responder, opts, err := buildMutationResponder(cfg, nil, fakeMutateClientsFactory(), logger.New("test"), nil)
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

// TestBuildMutationResponderFactoryCalledOnlyWhenEnabled is the point of
// this round: a disabled agent must not resolve a REST config, build a
// dynamic client, or start a rate limiter for a mutate client pool it is
// about to throw away -- and a NewQueryClients failure on that path must
// not become a fatal error for an agent that wants nothing from Kubernetes.
// The factory itself must not run before the MutateEnabled gate.
//
// Both directions are asserted, not just the zero case: a factory that is
// unconditionally never called would pass the disabled subtest vacuously,
// so the enabled subtest asserting a non-zero count is load-bearing too.
func TestBuildMutationResponderFactoryCalledOnlyWhenEnabled(t *testing.T) {
	countingFactory := func(calls *int) func() (*k8s.QueryClients, error) {
		return func() (*k8s.QueryClients, error) {
			*calls++
			c := fakeMutateClients()
			return &c, nil
		}
	}

	t.Run("disabled: factory not called", func(t *testing.T) {
		disabled := false
		cfg := &config.Config{Mutate: config.MutateConfig{Enabled: &disabled}}

		var calls int
		if _, _, err := buildMutationResponder(cfg, nil, countingFactory(&calls), logger.New("test"), nil); err != nil {
			t.Fatalf("buildMutationResponder: %v", err)
		}
		if calls != 0 {
			t.Fatalf("factory calls = %d, want 0 when mutate.enabled is false", calls)
		}
	})

	t.Run("enabled: factory called", func(t *testing.T) {
		enabled := true
		cfg := &config.Config{Mutate: config.MutateConfig{Enabled: &enabled}}
		mutatePolicy := compiledMutatePolicy(t)

		var calls int
		if _, _, err := buildMutationResponder(cfg, mutatePolicy, countingFactory(&calls), logger.New("test"), nil); err != nil {
			t.Fatalf("buildMutationResponder: %v", err)
		}
		if calls == 0 {
			t.Fatal("factory calls = 0, want non-zero when mutate.enabled is true -- the assertion above would be vacuous otherwise")
		}
	})
}

// compiledQueryPolicy compiles a valid, non-nil query policy -- the "what a
// real serve() call site would hand buildCapabilityReporterOptions" shape,
// distinct in TYPE (internal/query/policy.Policy) from the mutate policy
// compiledMutatePolicy builds, for the same reason that function's own
// comment gives.
func compiledQueryPolicy(t *testing.T) *policy.Policy {
	t.Helper()
	p, err := policy.Compile(&config.Config{})
	if err != nil {
		t.Fatalf("policy.Compile: %v", err)
	}
	return p
}

// compiledExecPolicy compiles a valid, non-nil exec policy -- the "what a
// real serve() call site would hand buildCapabilityReporterOptions" shape,
// distinct in TYPE (internal/exec/policy.Policy) from both the query and
// mutate policies above, for the same reason compiledMutatePolicy's own
// comment gives.
func compiledExecPolicy(t *testing.T) *execpolicy.Policy {
	t.Helper()
	enabled := true
	cfg := &config.Config{}
	cfg.Exec.Pod.Enabled = &enabled
	cfg.Exec.Pod.Rules = []config.PodExecRule{{Namespace: "dev"}}
	p, err := execpolicy.Compile(cfg)
	if err != nil {
		t.Fatalf("execpolicy.Compile: %v", err)
	}
	return p
}

// compiledNodeExecPolicy compiles a valid, non-nil node exec policy, mirroring
// compiledExecPolicy's shape for exec.node instead of exec.pod.
func compiledNodeExecPolicy(t *testing.T) *execpolicy.NodePolicy {
	t.Helper()
	enabled := true
	cfg := &config.Config{}
	cfg.Exec.Node.Enabled = &enabled
	cfg.Exec.Node.Image = "busybox"
	cfg.Exec.Node.Nodes = []string{"*"}
	p, err := execpolicy.CompileNode(cfg)
	if err != nil {
		t.Fatalf("execpolicy.CompileNode: %v", err)
	}
	return p
}

// TestBuildCapabilityReporterOptionsWiresBothPolicies pins the wiring gap
// Task 6 shipped without: nothing previously asserted that a mutate policy
// reaches capability.Options at all, so an omitted MutatePolicy line left
// can_patch/can_delete/can_create/policy_* false for every resource forever,
// whatever mutate.rules said -- and every OTHER test in this package stayed
// green, which is exactly why the gap survived the task. Extended by the pod
// console's own Task 6 to pin ExecPolicy the same way: an omitted line here
// leaves can_exec/policy_exec false for the pods entry forever, whatever
// exec.pod.rules says.
//
// All three fields are asserted by IDENTITY, in the same test: Policy must
// still be the exact query policy passed in, MutatePolicy must be the exact
// mutate policy passed in, and ExecPolicy must be the exact exec policy
// passed in. Checking all three together, rather than one field at a time or
// merely non-nil, is what catches a regression that drops one policy while
// leaving the others correct -- the shape this task's actual defect took.
func TestBuildCapabilityReporterOptionsWiresBothPolicies(t *testing.T) {
	cfg := &config.Config{}
	queryPolicy := compiledQueryPolicy(t)
	mutatePolicy := compiledMutatePolicy(t)
	execPolicy := compiledExecPolicy(t)
	nodeExecPolicy := compiledNodeExecPolicy(t)

	opts := buildCapabilityReporterOptions(cfg, nil, nil, queryPolicy, mutatePolicy, execPolicy, nodeExecPolicy, "kubexa")

	if opts.Policy != queryPolicy {
		t.Fatalf("opts.Policy = %p, want the exact queryPolicy passed in (%p)", opts.Policy, queryPolicy)
	}
	if opts.MutatePolicy != mutatePolicy {
		t.Fatalf("opts.MutatePolicy = %p, want the exact mutatePolicy passed in (%p)", opts.MutatePolicy, mutatePolicy)
	}
	if opts.ExecPolicy != execPolicy {
		t.Fatalf("opts.ExecPolicy = %p, want the exact execPolicy passed in (%p)", opts.ExecPolicy, execPolicy)
	}
	if opts.NodeExecPolicy != nodeExecPolicy {
		t.Fatalf("opts.NodeExecPolicy = %p, want the exact nodeExecPolicy passed in (%p)", opts.NodeExecPolicy, nodeExecPolicy)
	}
	if opts.HelperNamespace != "kubexa" {
		t.Fatalf("opts.HelperNamespace = %q, want %q", opts.HelperNamespace, "kubexa")
	}
}

// fakeExecClientsFactory satisfies exec.New's "clients are required" check
// without touching a real cluster; nothing here opens a session.
func fakeExecClientsFactory() func() (*k8s.ExecClients, error) {
	return func() (*k8s.ExecClients, error) {
		return &k8s.ExecClients{
			Clientset: k8sfake.NewSimpleClientset(),
			REST:      &rest.Config{Host: "https://example"},
		}, nil
	}
}

// nilExecDialer is never called by these tests: the transport dials only
// once an exec_open arrives.
func nilExecDialer(context.Context) (agentv1.AgentService_ExecSessionClient, error) {
	return nil, errors.New("not dialled in tests")
}

// nilResolveOwnPod is never called by tests whose exec.node is disabled --
// buildExecResponder only calls resolveOwnPod inside the ExecNodeEnabled
// branch.
func nilResolveOwnPod(context.Context, kubernetes.Interface) (exec.OwnPod, error) {
	return exec.OwnPod{}, errors.New("not resolved in tests")
}

// TestCompileExecPolicyWarnsAndContinuesWhenDisabled pins the ruling
// compileMutatePolicy established, applied to exec.pod: a bad rule under a
// DISABLED section is a warning naming the rule and a nil policy, never a
// crash-loop; under an ENABLED section it is fatal.
func TestCompileExecPolicyWarnsAndContinuesWhenDisabled(t *testing.T) {
	bad := []config.PodExecRule{{Namespace: "de*v"}}

	t.Run("disabled", func(t *testing.T) {
		disabled := false
		cfg := &config.Config{}
		cfg.Exec.Pod.Enabled = &disabled
		cfg.Exec.Pod.Rules = bad
		var buf bytes.Buffer
		p, err := compileExecPolicy(cfg, logger.New("test", logger.WithWriter(&buf)))
		if err != nil {
			t.Fatalf("compileExecPolicy() error = %v, want nil for a disabled section", err)
		}
		if p != nil {
			t.Fatalf("compileExecPolicy() policy = %+v, want nil -- the console must stay disabled", p)
		}
		if logged := buf.String(); !strings.Contains(logged, "exec.pod.rules[0]") || !strings.Contains(strings.ToLower(logged), "warn") {
			t.Fatalf("warning log = %q, want a warn line naming exec.pod.rules[0]", logged)
		}
	})

	t.Run("enabled", func(t *testing.T) {
		enabled := true
		cfg := &config.Config{}
		cfg.Exec.Pod.Enabled = &enabled
		cfg.Exec.Pod.Rules = bad
		p, err := compileExecPolicy(cfg, logger.New("test"))
		if err == nil {
			t.Fatal("compileExecPolicy() error = nil, want an error for an invalid rule with exec.pod enabled")
		}
		if p != nil {
			t.Fatalf("compileExecPolicy() policy = %+v, want nil alongside a fatal error", p)
		}
	})
}

// TestCompileNodeExecPolicyWarnsAndContinuesWhenDisabled mirrors
// TestCompileExecPolicyWarnsAndContinuesWhenDisabled for exec.node instead
// of exec.pod.
func TestCompileNodeExecPolicyWarnsAndContinuesWhenDisabled(t *testing.T) {
	bad := []string{"de*v"}

	t.Run("disabled", func(t *testing.T) {
		disabled := false
		cfg := &config.Config{}
		cfg.Exec.Node.Enabled = &disabled
		cfg.Exec.Node.Nodes = bad
		var buf bytes.Buffer
		p, err := compileNodeExecPolicy(cfg, logger.New("test", logger.WithWriter(&buf)))
		if err != nil {
			t.Fatalf("compileNodeExecPolicy() error = %v, want nil for a disabled section", err)
		}
		if p != nil {
			t.Fatalf("compileNodeExecPolicy() policy = %+v, want nil -- the console must stay disabled", p)
		}
		if logged := buf.String(); !strings.Contains(logged, "exec.node") || !strings.Contains(strings.ToLower(logged), "warn") {
			t.Fatalf("warning log = %q, want a warn line naming exec.node", logged)
		}
	})

	t.Run("enabled", func(t *testing.T) {
		enabled := true
		cfg := &config.Config{}
		cfg.Exec.Node.Enabled = &enabled
		cfg.Exec.Node.Nodes = bad
		p, err := compileNodeExecPolicy(cfg, logger.New("test"))
		if err == nil {
			t.Fatal("compileNodeExecPolicy() error = nil, want an error for an invalid pattern with exec.node enabled")
		}
		if p != nil {
			t.Fatalf("compileNodeExecPolicy() policy = %+v, want nil alongside a fatal error", p)
		}
	})
}

// TestBuildExecResponderIsNilWhenDisabled mirrors
// TestBuildMutationResponderNilResponderWhenDisabled and its factory
// counterpart: a disabled agent gets a nil responder (so the dispatcher
// drops exec_open) and never resolves exec clients for it.
func TestBuildExecResponderIsNilWhenDisabled(t *testing.T) {
	disabled := false
	cfg := &config.Config{}
	cfg.Exec.Pod.Enabled = &disabled

	calls := 0
	factory := func() (*k8s.ExecClients, error) {
		calls++
		return fakeExecClientsFactory()()
	}
	responder, opts, err := buildExecResponder(cfg, nil, nil, factory, nilExecDialer, logger.New("test"), nil, nilResolveOwnPod)
	if err != nil {
		t.Fatalf("buildExecResponder: %v", err)
	}
	if responder != nil {
		t.Fatalf("responder = %v, want nil when exec.pod.enabled is false", responder)
	}
	if calls != 0 {
		t.Fatalf("clients factory called %d times, want 0 for a disabled section", calls)
	}
	if opts.Policy != nil {
		t.Fatalf("opts.Policy = %v, want nil (the policy passed in was nil)", opts.Policy)
	}
}

// TestBuildExecResponderWiresPolicyAndSettings pins the wiring the way
// TestBuildMutationResponderWiresTheMutatePolicy does: the exact policy
// lands on Options, the settings come from cfg.ExecPodSettings() (with the
// operator's limits, not the defaults), and an enabled section yields a
// non-nil responder built from the factory's clients.
func TestBuildExecResponderWiresPolicyAndSettings(t *testing.T) {
	enabled := true
	cfg := &config.Config{}
	cfg.Exec.Pod.Enabled = &enabled
	cfg.Exec.Pod.Rules = []config.PodExecRule{{Namespace: "dev"}}
	cfg.Exec.Pod.MaxSessions = 2
	cfg.Exec.Pod.MaxSessionSec = 120
	execPolicy, err := execpolicy.Compile(cfg)
	if err != nil {
		t.Fatalf("execpolicy.Compile: %v", err)
	}

	calls := 0
	factory := func() (*k8s.ExecClients, error) {
		calls++
		return fakeExecClientsFactory()()
	}
	responder, opts, err := buildExecResponder(cfg, execPolicy, nil, factory, nilExecDialer, logger.New("test"), nil, nilResolveOwnPod)
	if err != nil {
		t.Fatalf("buildExecResponder: %v", err)
	}
	if responder == nil {
		t.Fatal("responder = nil, want a transport when exec.pod.enabled is true")
	}
	if calls != 1 {
		t.Fatalf("clients factory called %d times, want exactly 1", calls)
	}
	if opts.Policy != execPolicy {
		t.Fatalf("opts.Policy = %p, want the exact policy passed in (%p)", opts.Policy, execPolicy)
	}
	if opts.Settings.MaxSessions != 2 || opts.Settings.MaxSessionSec != 120 {
		t.Fatalf("opts.Settings = %+v, want MaxSessions 2 and MaxSessionSec 120 from cfg", opts.Settings)
	}
	if opts.Clients.Clientset == nil || opts.Clients.REST == nil {
		t.Fatalf("opts.Clients = %+v, want the factory's clients", opts.Clients)
	}
}

// TestBuildExecResponderWiresNodeOptions pins the Node wiring the way
// TestBuildExecResponderWiresPolicyAndSettings pins the pod console's own:
// the own-Pod identity lands on Node.Owner and Node.Namespace, an operator
// override to a namespace other than the agent's own drops the owner (an
// ownerReference cannot cross namespaces), and a failure to resolve the
// agent's own identity disables the node console alone -- the pod console
// (if configured) is unaffected -- with a warning naming why.
func TestBuildExecResponderWiresNodeOptions(t *testing.T) {
	newCfg := func() *config.Config {
		enabled := true
		disabled := false
		cfg := &config.Config{}
		cfg.Exec.Node.Enabled = &enabled
		cfg.Exec.Node.Image = "busybox"
		cfg.Exec.Node.Nodes = []string{"*"}
		cfg.Exec.Pod.Enabled = &disabled
		return cfg
	}
	compilePolicies := func(t *testing.T, cfg *config.Config) (*execpolicy.Policy, *execpolicy.NodePolicy) {
		t.Helper()
		execPolicy, err := execpolicy.Compile(cfg)
		if err != nil {
			t.Fatalf("execpolicy.Compile: %v", err)
		}
		nodePolicy, err := execpolicy.CompileNode(cfg)
		if err != nil {
			t.Fatalf("execpolicy.CompileNode: %v", err)
		}
		return execPolicy, nodePolicy
	}

	t.Run("own Pod identity", func(t *testing.T) {
		cfg := newCfg()
		execPolicy, nodePolicy := compilePolicies(t, cfg)
		factory := func() (*k8s.ExecClients, error) { return fakeExecClientsFactory()() }
		resolve := func(context.Context, kubernetes.Interface) (exec.OwnPod, error) {
			return exec.OwnPod{Name: "a", Namespace: "kubexa", UID: "u"}, nil
		}

		responder, opts, err := buildExecResponder(cfg, execPolicy, nodePolicy, factory, nilExecDialer, logger.New("test"), nil, resolve)
		if err != nil {
			t.Fatalf("buildExecResponder: %v", err)
		}
		if responder == nil {
			t.Fatal("responder = nil, want a transport when exec.node.enabled is true")
		}
		if opts.Node == nil {
			t.Fatal("opts.Node = nil, want a NodeOptions when exec.node.enabled is true")
		}
		if opts.Node.Namespace != "kubexa" {
			t.Fatalf("opts.Node.Namespace = %q, want %q (the own Pod's namespace)", opts.Node.Namespace, "kubexa")
		}
		if opts.Node.Owner == nil || opts.Node.Owner.UID != "u" {
			t.Fatalf("opts.Node.Owner = %+v, want a non-nil owner with UID %q", opts.Node.Owner, "u")
		}
		if opts.Node.Settings.Image != "busybox" {
			t.Fatalf("opts.Node.Settings.Image = %q, want %q", opts.Node.Settings.Image, "busybox")
		}
	})

	t.Run("override namespace drops the owner", func(t *testing.T) {
		cfg := newCfg()
		cfg.Exec.Node.Namespace = "shells"
		execPolicy, nodePolicy := compilePolicies(t, cfg)
		factory := func() (*k8s.ExecClients, error) { return fakeExecClientsFactory()() }
		resolve := func(context.Context, kubernetes.Interface) (exec.OwnPod, error) {
			return exec.OwnPod{Name: "a", Namespace: "kubexa", UID: "u"}, nil
		}

		_, opts, err := buildExecResponder(cfg, execPolicy, nodePolicy, factory, nilExecDialer, logger.New("test"), nil, resolve)
		if err != nil {
			t.Fatalf("buildExecResponder: %v", err)
		}
		if opts.Node == nil {
			t.Fatal("opts.Node = nil, want a NodeOptions when exec.node.enabled is true")
		}
		if opts.Node.Namespace != "shells" {
			t.Fatalf("opts.Node.Namespace = %q, want the configured override %q", opts.Node.Namespace, "shells")
		}
		if opts.Node.Owner != nil {
			t.Fatalf("opts.Node.Owner = %+v, want nil: an ownerReference cannot cross namespaces", opts.Node.Owner)
		}
	})

	t.Run("identity failure disables only the node console", func(t *testing.T) {
		cfg := newCfg()
		execPolicy, nodePolicy := compilePolicies(t, cfg)
		factory := func() (*k8s.ExecClients, error) { return fakeExecClientsFactory()() }
		resolve := func(context.Context, kubernetes.Interface) (exec.OwnPod, error) {
			return exec.OwnPod{}, errors.New("POD_NAME and POD_NAMESPACE are not set")
		}

		var buf bytes.Buffer
		responder, opts, err := buildExecResponder(
			cfg, execPolicy, nodePolicy, factory, nilExecDialer, logger.New("test", logger.WithWriter(&buf)), nil, resolve,
		)
		if err != nil {
			t.Fatalf("buildExecResponder: %v", err)
		}
		// exec.pod may be on independently of exec.node's identity failure --
		// here it happens to be off too, but the responder still builds
		// because opts.Policy (the pod policy) and opts.Clients are both
		// present; only opts.Node is affected.
		if responder == nil {
			t.Fatal("responder = nil, want a transport still built despite the node identity failure")
		}
		if opts.Node != nil {
			t.Fatalf("opts.Node = %+v, want nil when the agent could not resolve its own Pod", opts.Node)
		}
		if logged := buf.String(); !strings.Contains(logged, "node console disabled") {
			t.Fatalf("log = %q, want a warning containing %q", logged, "node console disabled")
		}
	})
}
