// internal/nodeops/engine_test.go
package nodeops

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubexa/kubexa-agent/pkg/config"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

var podsGVR = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

type recorder struct {
	mu     sync.Mutex
	events []*agentv1.NodeJobEvent
	done   chan *agentv1.NodeJobEvent
}

func newRecorder() *recorder { return &recorder{done: make(chan *agentv1.NodeJobEvent, 1)} }

func (r *recorder) emit(ev *agentv1.NodeJobEvent) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	switch ev.GetPhase() {
	case agentv1.NodeJobPhase_NODE_JOB_PHASE_SUCCEEDED, agentv1.NodeJobPhase_NODE_JOB_PHASE_FAILED,
		agentv1.NodeJobPhase_NODE_JOB_PHASE_CANCELLED, agentv1.NodeJobPhase_NODE_JOB_PHASE_REFUSED:
		r.done <- ev
	}
}

func (r *recorder) terminal(t *testing.T) *agentv1.NodeJobEvent {
	t.Helper()
	select {
	case ev := <-r.done:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no terminal event within 5s")
		return nil
	}
}

func (r *recorder) phases() []agentv1.NodeJobPhase {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]agentv1.NodeJobPhase, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.GetPhase())
	}
	return out
}

func (r *recorder) sawPodState(ns, name string, st agentv1.NodeJobPodState) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		for _, p := range e.GetPods() {
			if p.GetNamespace() == ns && p.GetName() == name && p.GetState() == st {
				return true
			}
		}
	}
	return false
}

func podRow(ev *agentv1.NodeJobEvent, ns, name string) *agentv1.NodeJobPod {
	for _, p := range ev.GetPods() {
		if p.GetNamespace() == ns && p.GetName() == name {
			return p
		}
	}
	return nil
}

func enabledPolicy(t *testing.T, verbs ...string) *Policy {
	t.Helper()
	p, err := Compile(&config.Config{Mutate: config.MutateConfig{Node: config.NodeMutateConfig{Enabled: on(), Nodes: []string{"w*"}, Verbs: verbs}}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newEngine(t *testing.T, cs *fake.Clientset, p *Policy) *Engine {
	t.Helper()
	e, err := New(Options{
		Clientset:     cs,
		Policy:        p,
		Own:           OwnPod{Namespace: "kubexa", Name: "kubexa-agent-1"},
		MaxTimeout:    time.Minute,
		MinTimeout:    50 * time.Millisecond,
		FixedTimeout:  2 * time.Second,
		PollInterval:  5 * time.Millisecond,
		RetryInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func nodeW1() *corev1.Node { return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}} }

func onNode(ns, name string, mut func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("uid-" + name)}, Spec: corev1.PodSpec{NodeName: "w1"}}
	if mut != nil {
		mut(p)
	}
	return p
}

// evictionReactor answers every pods/eviction create with fn. The default
// fake tracker has no idea what an eviction is, so every test installs one.
func evictionReactor(cs *fake.Clientset, fn func(ns, name string, opts *metav1.DeleteOptions) error) {
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		ev := a.(k8stesting.CreateAction).GetObject().(*policyv1.Eviction)
		return true, nil, fn(a.GetNamespace(), ev.Name, ev.DeleteOptions)
	})
}

func deleteFromTracker(cs *fake.Clientset) func(ns, name string, _ *metav1.DeleteOptions) error {
	return func(ns, name string, _ *metav1.DeleteOptions) error {
		return cs.Tracker().Delete(podsGVR, ns, name)
	}
}

func drainReq(id string, mut func(*agentv1.DrainOptions)) *agentv1.NodeJobRequest {
	d := &agentv1.DrainOptions{GracePeriodSeconds: -1, TimeoutSec: 300}
	if mut != nil {
		mut(d)
	}
	return &agentv1.NodeJobRequest{JobId: id, Node: "w1", Verb: agentv1.NodeJobVerb_NODE_JOB_VERB_DRAIN, Drain: d}
}

func TestCordonThenUncordon(t *testing.T) {
	cs := fake.NewSimpleClientset(nodeW1())
	e := newEngine(t, cs, enabledPolicy(t, "cordon", "uncordon"))

	r := newRecorder()
	e.Start(&agentv1.NodeJobRequest{JobId: "j1", Node: "w1", Verb: agentv1.NodeJobVerb_NODE_JOB_VERB_CORDON}, r.emit)
	ev := r.terminal(t)
	if ev.GetPhase() != agentv1.NodeJobPhase_NODE_JOB_PHASE_SUCCEEDED || !ev.GetNodeCordoned() || ev.GetJobId() != "j1" {
		t.Fatalf("cordon: %+v", ev)
	}
	if ph := r.phases(); ph[0] != agentv1.NodeJobPhase_NODE_JOB_PHASE_ACCEPTED {
		t.Fatalf("first event %v, want ACCEPTED", ph[0])
	}
	n, _ := cs.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Fatal("node not cordoned")
	}

	r = newRecorder()
	e.Start(&agentv1.NodeJobRequest{JobId: "j2", Node: "w1", Verb: agentv1.NodeJobVerb_NODE_JOB_VERB_UNCORDON}, r.emit)
	ev = r.terminal(t)
	if ev.GetPhase() != agentv1.NodeJobPhase_NODE_JOB_PHASE_SUCCEEDED || ev.GetNodeCordoned() {
		t.Fatalf("uncordon: %+v", ev)
	}
	r = newRecorder()
	e.Start(&agentv1.NodeJobRequest{JobId: "j3", Node: "w1", Verb: agentv1.NodeJobVerb_NODE_JOB_VERB_UNCORDON}, r.emit)
	if ev = r.terminal(t); !strings.Contains(ev.GetMessage(), "already") {
		t.Fatalf("second uncordon message %q, want 'already'", ev.GetMessage())
	}
}

func TestRefusalsBeforeAnyWrite(t *testing.T) {
	cs := fake.NewSimpleClientset(nodeW1())
	writes := 0
	cs.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) { writes++; return false, nil, nil })

	// policy: drain not granted
	e := newEngine(t, cs, enabledPolicy(t, "cordon"))
	r := newRecorder()
	e.Start(drainReq("j1", nil), r.emit)
	ev := r.terminal(t)
	if ev.GetPhase() != agentv1.NodeJobPhase_NODE_JOB_PHASE_REFUSED || ev.GetError().GetCode() != agentv1.NodeJobErrorCode_NODE_JOB_ERROR_POLICY_DENIED {
		t.Fatalf("policy: %+v", ev)
	}

	// unknown node
	e = newEngine(t, cs, enabledPolicy(t, "cordon", "drain"))
	r = newRecorder()
	e.Start(&agentv1.NodeJobRequest{JobId: "j2", Node: "w9", Verb: agentv1.NodeJobVerb_NODE_JOB_VERB_CORDON}, r.emit)
	if ev = r.terminal(t); ev.GetError().GetCode() != agentv1.NodeJobErrorCode_NODE_JOB_ERROR_NOT_FOUND {
		t.Fatalf("unknown node: %+v", ev)
	}

	// unspecified verb
	r = newRecorder()
	e.Start(&agentv1.NodeJobRequest{JobId: "j3", Node: "w1"}, r.emit)
	if ev = r.terminal(t); ev.GetPhase() != agentv1.NodeJobPhase_NODE_JOB_PHASE_REFUSED {
		t.Fatalf("unspecified verb: %+v", ev)
	}
	if writes != 0 {
		t.Fatalf("a refusal wrote to the node %d times", writes)
	}
}

func TestDrainSkipsEvictsAndSucceeds(t *testing.T) {
	cs := fake.NewSimpleClientset(nodeW1(),
		onNode("kube-system", "kube-proxy-1", func(p *corev1.Pod) { p.OwnerReferences = ctrl("DaemonSet") }),
		onNode("kubexa", "kubexa-agent-1", func(p *corev1.Pod) { p.OwnerReferences = ctrl("ReplicaSet") }),
		onNode("app", "web-1", func(p *corev1.Pod) { p.OwnerReferences = ctrl("ReplicaSet") }),
		onNode("app", "web-2", func(p *corev1.Pod) { p.OwnerReferences = ctrl("ReplicaSet") }),
	)
	var evicted []string
	evictionReactor(cs, func(ns, name string, opts *metav1.DeleteOptions) error {
		evicted = append(evicted, ns+"/"+name)
		return cs.Tracker().Delete(podsGVR, ns, name)
	})
	e := newEngine(t, cs, enabledPolicy(t, "drain"))
	r := newRecorder()
	e.Start(drainReq("j1", nil), r.emit)
	ev := r.terminal(t)
	if ev.GetPhase() != agentv1.NodeJobPhase_NODE_JOB_PHASE_SUCCEEDED {
		t.Fatalf("drain: %+v", ev)
	}
	if !ev.GetNodeCordoned() || ev.GetPodsTotal() != 4 || ev.GetPodsEvicted() != 2 || ev.GetPodsSkipped() != 2 || ev.GetPodsPending() != 0 {
		t.Fatalf("counters: %+v", ev)
	}
	if len(evicted) != 2 {
		t.Fatalf("evicted %v, want the two app pods only", evicted)
	}
	if p := podRow(ev, "kubexa", "kubexa-agent-1"); p.GetState() != agentv1.NodeJobPodState_NODE_JOB_POD_STATE_SKIPPED || p.GetReason() != ReasonAgentSelf {
		t.Fatalf("own pod: %+v", p)
	}
	n, _ := cs.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Fatal("drain did not cordon")
	}
	if ph := r.phases(); ph[0] != agentv1.NodeJobPhase_NODE_JOB_PHASE_ACCEPTED || ph[1] != agentv1.NodeJobPhase_NODE_JOB_PHASE_RUNNING {
		t.Fatalf("phases %v, want ACCEPTED, RUNNING, ...", ph)
	}
}

func TestDrainPreflightRefusesWithoutTouchingTheNode(t *testing.T) {
	cs := fake.NewSimpleClientset(nodeW1(), onNode("app", "bare", nil))
	evictions := 0
	evictionReactor(cs, func(string, string, *metav1.DeleteOptions) error { evictions++; return nil })
	e := newEngine(t, cs, enabledPolicy(t, "drain"))
	r := newRecorder()
	e.Start(drainReq("j1", nil), r.emit)
	ev := r.terminal(t)
	if ev.GetPhase() != agentv1.NodeJobPhase_NODE_JOB_PHASE_REFUSED || ev.GetError().GetCode() != agentv1.NodeJobErrorCode_NODE_JOB_ERROR_NEEDS_FORCE {
		t.Fatalf("preflight: %+v", ev)
	}
	if !strings.Contains(ev.GetError().GetMessage(), "app/bare") {
		t.Fatalf("message %q does not name the pod", ev.GetError().GetMessage())
	}
	if p := podRow(ev, "app", "bare"); p.GetReason() != ReasonNoController {
		t.Fatalf("table row: %+v", p)
	}
	n, _ := cs.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{})
	if n.Spec.Unschedulable || evictions != 0 || ev.GetNodeCordoned() {
		t.Fatalf("a refused drain changed the cluster: cordoned=%v evictions=%d", n.Spec.Unschedulable, evictions)
	}

	// force clears it
	r = newRecorder()
	evictionReactor(cs, deleteFromTracker(cs))
	e.Start(drainReq("j2", func(d *agentv1.DrainOptions) { d.Force = true }), r.emit)
	if ev = r.terminal(t); ev.GetPhase() != agentv1.NodeJobPhase_NODE_JOB_PHASE_SUCCEEDED {
		t.Fatalf("forced drain: %+v", ev)
	}
}

func TestDrainRetriesAPodBlockedByAPDB(t *testing.T) {
	cs := fake.NewSimpleClientset(nodeW1(), onNode("kube-system", "coredns-1", func(p *corev1.Pod) { p.OwnerReferences = ctrl("ReplicaSet") }))
	calls := 0
	evictionReactor(cs, func(ns, name string, _ *metav1.DeleteOptions) error {
		calls++
		if calls < 3 {
			return apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 1)
		}
		return cs.Tracker().Delete(podsGVR, ns, name)
	})
	e := newEngine(t, cs, enabledPolicy(t, "drain"))
	r := newRecorder()
	e.Start(drainReq("j1", nil), r.emit)
	ev := r.terminal(t)
	if ev.GetPhase() != agentv1.NodeJobPhase_NODE_JOB_PHASE_SUCCEEDED {
		t.Fatalf("drain: %+v", ev)
	}
	if !r.sawPodState("kube-system", "coredns-1", agentv1.NodeJobPodState_NODE_JOB_POD_STATE_BLOCKED) {
		t.Fatal("BLOCKED never reported")
	}
	if calls != 3 {
		t.Fatalf("eviction calls = %d, want 3 (two 429s, then accepted)", calls)
	}
}

func TestDrainTimesOutListingWhatIsLeft(t *testing.T) {
	cs := fake.NewSimpleClientset(nodeW1(), onNode("app", "stuck", func(p *corev1.Pod) { p.OwnerReferences = ctrl("StatefulSet") }))
	evictionReactor(cs, func(string, string, *metav1.DeleteOptions) error { return nil }) // accepted, never leaves
	e := newEngine(t, cs, enabledPolicy(t, "drain"))
	r := newRecorder()
	e.Start(drainReq("j1", func(d *agentv1.DrainOptions) { d.TimeoutSec = 0 }), r.emit) // 0 floors to MinTimeout (50ms); 1s is not < MinTimeout and would NOT be floored
	ev := r.terminal(t)
	if ev.GetPhase() != agentv1.NodeJobPhase_NODE_JOB_PHASE_FAILED || ev.GetError().GetCode() != agentv1.NodeJobErrorCode_NODE_JOB_ERROR_TIMEOUT {
		t.Fatalf("timeout: %+v", ev)
	}
	if p := podRow(ev, "app", "stuck"); p.GetState() != agentv1.NodeJobPodState_NODE_JOB_POD_STATE_EVICTING {
		t.Fatalf("stuck pod: %+v", p)
	}
	if !ev.GetNodeCordoned() || ev.GetPodsPending() != 1 {
		t.Fatalf("counters: %+v", ev)
	}
}

func TestCancelStopsEvictionAndLeavesTheCordon(t *testing.T) {
	cs := fake.NewSimpleClientset(nodeW1(), onNode("app", "slow", func(p *corev1.Pod) { p.OwnerReferences = ctrl("ReplicaSet") }))
	evictionReactor(cs, func(string, string, *metav1.DeleteOptions) error { return nil })
	e := newEngine(t, cs, enabledPolicy(t, "drain"))
	r := newRecorder()
	e.Start(drainReq("j1", nil), r.emit)

	// busy while it runs
	r2 := newRecorder()
	e.Start(drainReq("j2", nil), r2.emit)
	if ev := r2.terminal(t); ev.GetError().GetCode() != agentv1.NodeJobErrorCode_NODE_JOB_ERROR_BUSY {
		t.Fatalf("second job: %+v", ev)
	}
	if e.Running() != "j1" {
		t.Fatalf("Running() = %q", e.Running())
	}

	if !e.Cancel("j1") {
		t.Fatal("Cancel returned false for the running job")
	}
	ev := r.terminal(t)
	if ev.GetPhase() != agentv1.NodeJobPhase_NODE_JOB_PHASE_CANCELLED || !ev.GetNodeCordoned() {
		t.Fatalf("cancel: %+v", ev)
	}
	if e.Cancel("j1") {
		t.Fatal("Cancel returned true for a finished job")
	}
	if e.Cancel("nope") {
		t.Fatal("Cancel returned true for an unknown job")
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	cs := fake.NewSimpleClientset(nodeW1(),
		onNode("app", "web-1", func(p *corev1.Pod) { p.OwnerReferences = ctrl("ReplicaSet") }),
		onNode("kube-system", "coredns-1", func(p *corev1.Pod) { p.OwnerReferences = ctrl("ReplicaSet") }),
	)
	// The fake ObjectTracker does not honor PatchOptions.DryRun (it always
	// applies the patch) -- see cordon_test.go's
	// TestSetUnschedulableDryRunCarriesDryRunAll, which the same task
	// established this same pattern for. A real API server would run
	// admission and answer without writing; here the test must emulate
	// that itself, or the cordon patch would land despite dry_run.
	var nodeDryRun []string
	cs.PrependReactor("patch", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
		nodeDryRun = a.(k8stesting.PatchActionImpl).GetPatchOptions().DryRun
		return true, nodeW1(), nil
	})
	var dryRuns []string
	evictionReactor(cs, func(ns, name string, opts *metav1.DeleteOptions) error {
		if opts != nil {
			dryRuns = append(dryRuns, strings.Join(opts.DryRun, ","))
		}
		if name == "coredns-1" {
			return apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 1)
		}
		return nil // accepted; the pod is NOT deleted -- a dry run
	})
	e := newEngine(t, cs, enabledPolicy(t, "drain"))
	r := newRecorder()
	e.Start(drainReq("j1", func(d *agentv1.DrainOptions) { d.DryRun = true }), r.emit)
	ev := r.terminal(t)
	if ev.GetPhase() != agentv1.NodeJobPhase_NODE_JOB_PHASE_SUCCEEDED || ev.GetNodeCordoned() {
		t.Fatalf("dry run: %+v", ev)
	}
	if p := podRow(ev, "app", "web-1"); p.GetState() != agentv1.NodeJobPodState_NODE_JOB_POD_STATE_GONE || p.GetReason() != ReasonDryRun {
		t.Fatalf("web-1: %+v", p)
	}
	if p := podRow(ev, "kube-system", "coredns-1"); p.GetState() != agentv1.NodeJobPodState_NODE_JOB_POD_STATE_BLOCKED || p.GetReason() != ReasonPDB {
		t.Fatalf("coredns-1: %+v", p)
	}
	if len(dryRuns) != 2 || dryRuns[0] != "All" || dryRuns[1] != "All" {
		t.Fatalf("eviction DryRun options = %v, want [All All]", dryRuns)
	}
	if len(nodeDryRun) != 1 || nodeDryRun[0] != metav1.DryRunAll {
		t.Fatalf("cordon patch DryRun = %v, want [All]", nodeDryRun)
	}
	n, _ := cs.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{})
	if n.Spec.Unschedulable {
		t.Fatal("dry run cordoned the node")
	}
	if _, err := cs.CoreV1().Pods("app").Get(context.Background(), "web-1", metav1.GetOptions{}); err != nil {
		t.Fatal("dry run removed a pod")
	}
}

func TestRBACDeniedNamesTheVerb(t *testing.T) {
	cs := fake.NewSimpleClientset(nodeW1())
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(podsGVR.GroupResource(), "", errors.New("forbidden"))
	})
	e := newEngine(t, cs, enabledPolicy(t, "drain"))
	r := newRecorder()
	e.Start(drainReq("j1", nil), r.emit)
	ev := r.terminal(t)
	if ev.GetPhase() != agentv1.NodeJobPhase_NODE_JOB_PHASE_REFUSED || ev.GetError().GetCode() != agentv1.NodeJobErrorCode_NODE_JOB_ERROR_RBAC_DENIED {
		t.Fatalf("rbac: %+v", ev)
	}
	if !strings.Contains(ev.GetError().GetMessage(), "pods list") {
		t.Fatalf("message %q does not name the verb", ev.GetError().GetMessage())
	}
}

// TestDrainWaitListForbiddenEndsRBACDenied guards Finding I1: the wait loop
// polls with ONE "pods list" per tick, never a per-pod "pods get". The
// preflight list (building the table) succeeds; every list after that
// (the wait loop's) is answered 403, as it would be for an RBAC binding
// missing "pods list" -- which must never be read as "still evicting".
func TestDrainWaitListForbiddenEndsRBACDenied(t *testing.T) {
	cs := fake.NewSimpleClientset(nodeW1(), onNode("app", "web-1", func(p *corev1.Pod) { p.OwnerReferences = ctrl("ReplicaSet") }))
	evictionReactor(cs, func(string, string, *metav1.DeleteOptions) error { return nil }) // accepted; pod stays (never deleted)
	calls := 0
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return false, nil, nil // let the preflight list through to the tracker
		}
		return true, nil, apierrors.NewForbidden(podsGVR.GroupResource(), "", errors.New("forbidden"))
	})
	e := newEngine(t, cs, enabledPolicy(t, "drain"))
	r := newRecorder()
	e.Start(drainReq("j1", nil), r.emit)
	ev := r.terminal(t)
	if ev.GetPhase() != agentv1.NodeJobPhase_NODE_JOB_PHASE_FAILED || ev.GetError().GetCode() != agentv1.NodeJobErrorCode_NODE_JOB_ERROR_RBAC_DENIED {
		t.Fatalf("wait-list forbidden: %+v", ev)
	}
	if !strings.Contains(ev.GetError().GetMessage(), "pods list") {
		t.Fatalf("message %q does not name pods list", ev.GetError().GetMessage())
	}
	if calls < 2 {
		t.Fatalf("list calls = %d, want at least 2 (preflight, then the wait loop)", calls)
	}
}

// TestNilDrainOptionsUsesThePodsOwnGrace guards Finding I3: a nil Drain must
// mean "the pod's own grace period", the same as GracePeriodSeconds: -1 --
// never an explicit 0 (immediate SIGKILL of every pod on the node).
// d.GetGracePeriodSeconds() on a nil d silently returns 0, which is why the
// fix reads the field only after a nil check instead of through the getter.
func TestNilDrainOptionsUsesThePodsOwnGrace(t *testing.T) {
	cs := fake.NewSimpleClientset(nodeW1(), onNode("app", "web-1", func(p *corev1.Pod) { p.OwnerReferences = ctrl("ReplicaSet") }))
	var opts *metav1.DeleteOptions
	var sawEviction bool
	evictionReactor(cs, func(ns, name string, o *metav1.DeleteOptions) error {
		sawEviction = true
		opts = o
		return cs.Tracker().Delete(podsGVR, ns, name)
	})
	e := newEngine(t, cs, enabledPolicy(t, "drain"))
	r := newRecorder()
	e.Start(&agentv1.NodeJobRequest{JobId: "j1", Node: "w1", Verb: agentv1.NodeJobVerb_NODE_JOB_VERB_DRAIN, Drain: nil}, r.emit)
	ev := r.terminal(t)
	if ev.GetPhase() != agentv1.NodeJobPhase_NODE_JOB_PHASE_SUCCEEDED {
		t.Fatalf("nil drain: %+v", ev)
	}
	if !sawEviction {
		t.Fatal("no eviction observed")
	}
	if opts != nil {
		t.Fatalf("eviction DeleteOptions = %+v, want nil (grace period is the pod's own)", opts)
	}
}

// TestContextCancelDuringEvictionEndsCancelledNotFailed guards Finding 1's
// first bullet: a Cancel (or the deadline) firing WHILE an eviction call is
// in flight must end the job CANCELLED, and must never mark the
// still-present pod FAILED for what is really a cancellation. The fake
// clientset ignores ctx entirely, so the eviction reactor cancels the
// engine itself and then hands back the error a real client would return
// once its request context was cancelled mid-flight.
func TestContextCancelDuringEvictionEndsCancelledNotFailed(t *testing.T) {
	cs := fake.NewSimpleClientset(nodeW1(), onNode("app", "slow", func(p *corev1.Pod) { p.OwnerReferences = ctrl("ReplicaSet") }))
	e := newEngine(t, cs, enabledPolicy(t, "drain"))
	evictionReactor(cs, func(string, string, *metav1.DeleteOptions) error {
		e.Cancel("j1")
		return context.Canceled
	})
	r := newRecorder()
	e.Start(drainReq("j1", nil), r.emit)
	ev := r.terminal(t)
	if ev.GetPhase() != agentv1.NodeJobPhase_NODE_JOB_PHASE_CANCELLED {
		t.Fatalf("phase = %v, want CANCELLED: %+v", ev.GetPhase(), ev)
	}
	if p := podRow(ev, "app", "slow"); p.GetState() == agentv1.NodeJobPodState_NODE_JOB_POD_STATE_FAILED {
		t.Fatalf("pod marked FAILED for a context cancellation: %+v", p)
	}
}

// TestContextDeadlineBeforeAcceptedEndsTimeoutNotInternal guards Finding
// 1's second bullet: the deadline firing before ACCEPTED (here, during the
// pods List call inside runDrain) must end the job FAILED/TIMEOUT, not
// REFUSED/INTERNAL. The reactor sleeps past MinTimeout so the job's REAL
// context (from context.WithTimeout in Start) is genuinely expired by the
// time it returns -- ctx.Err() must be the thing that decides this, not
// the shape of the error the fake handed back.
func TestContextDeadlineBeforeAcceptedEndsTimeoutNotInternal(t *testing.T) {
	cs := fake.NewSimpleClientset(nodeW1(), onNode("app", "x", func(p *corev1.Pod) { p.OwnerReferences = ctrl("ReplicaSet") }))
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		time.Sleep(150 * time.Millisecond) // outlast the floored MinTimeout (50ms)
		return true, nil, context.DeadlineExceeded
	})
	e := newEngine(t, cs, enabledPolicy(t, "drain"))
	r := newRecorder()
	// TimeoutSec 0 floors to MinTimeout (50ms) via timeoutFor -- TimeoutSec
	// 1 would set a real 1s deadline (1s is not < MinTimeout, so it is
	// NOT floored), which the reactor's sleep would need to outlast too.
	e.Start(drainReq("j1", func(d *agentv1.DrainOptions) { d.TimeoutSec = 0 }), r.emit)
	ev := r.terminal(t)
	if ev.GetPhase() != agentv1.NodeJobPhase_NODE_JOB_PHASE_FAILED || ev.GetError().GetCode() != agentv1.NodeJobErrorCode_NODE_JOB_ERROR_TIMEOUT {
		t.Fatalf("deadline before accepted: %+v", ev)
	}
}
