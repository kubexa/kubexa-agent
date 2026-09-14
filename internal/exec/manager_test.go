package exec

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/kubexa/kubexa-agent/internal/exec/policy"
	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/pkg/config"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

type runtimeObject = runtime.Object

func newTestManager(t *testing.T, rules []config.PodExecRule, maxSessions int, pods ...*corev1.Pod) (*Manager, *fakeExecutor) {
	t.Helper()
	c := &config.Config{}
	on := true
	c.Exec.Pod.Enabled = &on
	c.Exec.Pod.Rules = rules
	c.Exec.Pod.MaxSessions = maxSessions
	pol, err := policy.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	objs := make([]runtimeObject, 0, len(pods))
	for _, p := range pods {
		objs = append(objs, p)
	}
	fe := &fakeExecutor{started: make(chan struct{}, 8)}
	m, err := New(Options{
		Policy:   pol,
		Clients:  k8s.ExecClients{Clientset: fake.NewSimpleClientset(objs...), REST: &rest.Config{Host: "https://example"}},
		Settings: c.ExecPodSettings(),
		newExecutor: func(*rest.Config, *url.URL) (remotecommand.Executor, error) {
			return fe, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, fe
}

func webPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "dev", Name: "web-1",
			Annotations: map[string]string{"kubectl.kubernetes.io/default-container": "app"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sidecar"}, {Name: "app"}}},
	}
}

func open(ns, name, ctr string) *agentv1.ExecOpen {
	return &agentv1.ExecOpen{SessionId: "sid", Tty: true, Stdin: true,
		ResumeWindowSec: 60, MaxSessionSec: 600,
		Target: &agentv1.ExecTarget{Target: &agentv1.ExecTarget_Pod{Pod: &agentv1.PodTarget{
			Namespace: ns, Name: name, Container: ctr}}}}
}

func TestOpenResolvesDefaultContainerBeforePolicy(t *testing.T) {
	m, _ := newTestManager(t, []config.PodExecRule{{Namespace: "dev", Containers: []string{"app"}}}, 4, webPod())
	s, refusal := m.Open(context.Background(), open("dev", "web-1", ""))
	if refusal != nil {
		t.Fatalf("refused: %v", refusal)
	}
	if s.target.Container != "app" {
		t.Fatalf("default container = %q, want app (annotation)", s.target.Container)
	}
	s.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "")
}

func TestOpenRefusesByPolicy(t *testing.T) {
	m, _ := newTestManager(t, []config.PodExecRule{{Namespace: "prod"}}, 4, webPod())
	_, refusal := m.Open(context.Background(), open("dev", "web-1", "app"))
	if refusal.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_POLICY_DENIED {
		t.Fatalf("got %v", refusal)
	}
}

func TestOpenRefusesUnknownPodAndContainer(t *testing.T) {
	m, _ := newTestManager(t, []config.PodExecRule{{}}, 4, webPod())
	if _, r := m.Open(context.Background(), open("dev", "nope", "")); r.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_NOT_FOUND {
		t.Fatalf("missing pod: %v", r)
	}
	if _, r := m.Open(context.Background(), open("dev", "web-1", "ghost")); r.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_NOT_FOUND {
		t.Fatalf("missing container: %v", r)
	}
}

func TestOpenEnforcesMaxSessions(t *testing.T) {
	m, _ := newTestManager(t, []config.PodExecRule{{}}, 1, webPod())
	s1, r := m.Open(context.Background(), open("dev", "web-1", "app"))
	if r != nil {
		t.Fatal(r)
	}
	o2 := open("dev", "web-1", "app")
	o2.SessionId = "sid2"
	if _, r := m.Open(context.Background(), o2); r.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_TOO_MANY_SESSIONS {
		t.Fatalf("second session: %v", r)
	}
	s1.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "")
	<-s1.Done()
	time.Sleep(50 * time.Millisecond) // the slot is released on Done
	s2, r := m.Open(context.Background(), o2)
	if r != nil {
		t.Fatalf("after close the slot must be free: %v", r)
	}
	s2.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "")
}

func TestOpenClampsToOwnLimits(t *testing.T) {
	m, _ := newTestManager(t, []config.PodExecRule{{}}, 4, webPod())
	o := open("dev", "web-1", "app")
	o.MaxSessionSec = 999999
	o.ResumeWindowSec = 999
	s, r := m.Open(context.Background(), o)
	if r != nil {
		t.Fatal(r)
	}
	if s.spec.maxSession != 1800*time.Second || s.spec.resumeWindow != 60*time.Second {
		t.Fatalf("spec = %+v, want the agent's own caps", s.spec)
	}
	s.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "")
}

func TestOpenRefusesDuplicateSessionID(t *testing.T) {
	m, _ := newTestManager(t, []config.PodExecRule{{}}, 4, webPod())
	s, r := m.Open(context.Background(), open("dev", "web-1", "app"))
	if r != nil {
		t.Fatal(r)
	}
	defer s.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "")
	if _, r := m.Open(context.Background(), open("dev", "web-1", "app")); r.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL {
		t.Fatalf("duplicate id: %v", r)
	}
}

// The URL is the whole request to pods/exec; a wrong path or a missing
// stdin/tty flag would only show up against a live API server.
func TestOpenBuildsExecURL(t *testing.T) {
	m, _ := newTestManager(t, []config.PodExecRule{{}}, 4, webPod())
	var got *url.URL
	m.opts.newExecutor = func(_ *rest.Config, u *url.URL) (remotecommand.Executor, error) {
		got = u
		return &fakeExecutor{started: make(chan struct{})}, nil
	}
	s, r := m.Open(context.Background(), open("dev", "web-1", ""))
	if r != nil {
		t.Fatal(r)
	}
	defer s.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "")
	if got.Host != "example" || got.Path != "/api/v1/namespaces/dev/pods/web-1/exec" {
		t.Fatalf("url = %s", got)
	}
	q := got.Query()
	if q.Get("container") != "app" || q.Get("command") != "/bin/sh" ||
		q.Get("stdin") != "true" || q.Get("stdout") != "true" || q.Get("tty") != "true" || q.Has("stderr") {
		t.Fatalf("query = %s", got.RawQuery)
	}
}

// hangingPods is a clientset whose pod Get blocks until its ctx ends -- an
// API server round trip into a TCP black hole.
type hangingPods struct{ kubernetes.Interface }

func (h hangingPods) CoreV1() corev1client.CoreV1Interface {
	return hangingCoreV1{h.Interface.CoreV1()}
}

type hangingCoreV1 struct{ corev1client.CoreV1Interface }

func (h hangingCoreV1) Pods(ns string) corev1client.PodInterface {
	return hangingPodInterface{h.CoreV1Interface.Pods(ns)}
}

type hangingPodInterface struct{ corev1client.PodInterface }

func (hangingPodInterface) Get(ctx context.Context, _ string, _ metav1.GetOptions) (*corev1.Pod, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// Open reserves the max_sessions slot before the pod lookup. A hung API
// server must not pin that slot for TCP's own timeout: the Get is bounded,
// the refusal says so, and the slot is free again when Open returns.
func TestOpenBoundsThePodLookupAndReleasesTheSlot(t *testing.T) {
	m, _ := newTestManager(t, []config.PodExecRule{{}}, 1, webPod())
	m.opts.Clients.Clientset = hangingPods{m.opts.Clients.Clientset}
	m.opts.podLookup = 200 * time.Millisecond

	start := time.Now()
	s, r := m.Open(context.Background(), open("dev", "web-1", "app"))
	took := time.Since(start)
	if s != nil || r.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL || r.GetMessage() != "pod lookup timed out" {
		t.Fatalf("open = %v, %v; want an INTERNAL refusal saying the lookup timed out", s, r)
	}
	if took < 200*time.Millisecond || took > 2*time.Second {
		t.Fatalf("Open returned after %v, want about the lookup timeout", took)
	}
	m.mu.Lock()
	held := len(m.sessions)
	m.mu.Unlock()
	if held != 0 {
		t.Fatalf("%d session slot(s) still held after the lookup timed out, want 0", held)
	}
}

// optionsRecordingExecutor hands the test the StreamOptions it was started
// with and then runs until the session ends.
type optionsRecordingExecutor struct {
	opts chan remotecommand.StreamOptions
}

func (optionsRecordingExecutor) Stream(remotecommand.StreamOptions) error { panic("unused") }
func (e optionsRecordingExecutor) StreamWithContext(ctx context.Context, o remotecommand.StreamOptions) error {
	e.opts <- o
	<-ctx.Done()
	return ctx.Err()
}

// ExecOpen.stdin false tells the kubelet (PodExecOptions.Stdin) not to
// expect a stdin stream; the executor must then not open one, and a stdin
// write must be refused rather than block forever in a pipe nobody reads.
func TestOpenWithoutStdinOpensNoStdinStream(t *testing.T) {
	m, _ := newTestManager(t, []config.PodExecRule{{}}, 4, webPod())
	ex := optionsRecordingExecutor{opts: make(chan remotecommand.StreamOptions, 1)}
	m.opts.newExecutor = func(*rest.Config, *url.URL) (remotecommand.Executor, error) { return ex, nil }
	o := open("dev", "web-1", "app")
	o.Stdin = false
	s, r := m.Open(context.Background(), o)
	if r != nil {
		t.Fatal(r)
	}
	defer s.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "")

	select {
	case got := <-ex.opts:
		if got.Stdin != nil {
			t.Fatal("StreamOptions.Stdin is set on a session opened with stdin=false")
		}
		if got.Stdout == nil || got.Stderr == nil {
			t.Fatal("stdout/stderr must still be streamed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the executor was never started")
	}
	done := make(chan error, 1)
	go func() { done <- s.WriteStdin([]byte("x")) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("WriteStdin succeeded on a session with no stdin stream")
		}
	case <-time.After(time.Second):
		t.Fatal("WriteStdin blocked on a session with no stdin stream")
	}
}

func newNodeTestManager(t *testing.T, patterns []string, maxSessions int, objs ...runtimeObject) (*Manager, *fakeExecutor, *fake.Clientset) {
	t.Helper()
	c := &config.Config{}
	on := true
	c.Exec.Node.Enabled = &on
	c.Exec.Node.Nodes = patterns
	c.Exec.Node.Image = "busybox:1.36"
	c.Exec.Node.MaxSessions = maxSessions
	c.Exec.Node.HelperReadyTimeoutSec = 5
	np, err := policy.CompileNode(c)
	if err != nil {
		t.Fatal(err)
	}
	pp, _ := policy.Compile(&config.Config{})
	cs := fake.NewSimpleClientset(objs...)
	// The fake clientset never runs a kubelet: flip every created helper
	// to Running so awaitHelperRunning returns.
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		p := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		p.Status.Phase = corev1.PodRunning
		return false, nil, nil
	})
	fe := &fakeExecutor{started: make(chan struct{}, 8)}
	m, err := New(Options{
		Policy:   pp,
		Clients:  k8s.ExecClients{Clientset: cs, REST: &rest.Config{Host: "https://example"}},
		Settings: (&config.Config{}).ExecPodSettings(),
		Node: &NodeOptions{
			Policy:    np,
			Settings:  c.ExecNodeSettings(),
			Owner:     &OwnPod{Name: "kubexa-agent-1", Namespace: "kubexa", UID: "u1"},
			Namespace: "kubexa",
		},
		newExecutor: func(*rest.Config, *url.URL) (remotecommand.Executor, error) { return fe, nil },
		helperPoll:  time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, fe, cs
}

func nodeOpen(node string) *agentv1.ExecOpen {
	return &agentv1.ExecOpen{SessionId: "nsid", Tty: true, Stdin: true, ResumeWindowSec: 60, MaxSessionSec: 600,
		Target: &agentv1.ExecTarget{Target: &agentv1.ExecTarget_Node{Node: &agentv1.NodeTarget{Name: node}}}}
}

func node(name string) *corev1.Node { return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}} }

func TestOpenNodeCreatesHelperAndExecsNsenter(t *testing.T) {
	m, _, cs := newNodeTestManager(t, []string{"*"}, 1, node("n1"))
	var got *url.URL
	m.opts.newExecutor = func(_ *rest.Config, u *url.URL) (remotecommand.Executor, error) {
		got = u
		return &fakeExecutor{started: make(chan struct{})}, nil
	}
	s, r := m.Open(context.Background(), nodeOpen("n1"))
	if r != nil {
		t.Fatal(r)
	}
	name := helperName("nsid")
	if got.Path != "/api/v1/namespaces/kubexa/pods/"+name+"/exec" {
		t.Fatalf("url = %s", got)
	}
	q := got.Query()
	if q.Get("container") != "shell" || q.Get("tty") != "true" {
		t.Fatalf("query = %s", got.RawQuery)
	}
	cmd := q["command"]
	want := []string{"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "/bin/sh", "-l"}
	if strings.Join(cmd, " ") != strings.Join(want, " ") {
		t.Fatalf("command = %v", cmd)
	}
	if _, err := cs.CoreV1().Pods("kubexa").Get(context.Background(), name, metav1.GetOptions{}); err != nil {
		t.Fatalf("helper missing: %v", err)
	}
	// Ending the session deletes the helper.
	s.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := cs.CoreV1().Pods("kubexa").Get(context.Background(), name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper pod not deleted after the session ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOpenNodeRefusals(t *testing.T) {
	t.Run("unknown node", func(t *testing.T) {
		m, _, _ := newNodeTestManager(t, []string{"*"}, 1)
		_, r := m.Open(context.Background(), nodeOpen("ghost"))
		if r == nil || r.Reason != agentv1.ExecExitReason_EXEC_EXIT_REASON_NOT_FOUND {
			t.Fatalf("r = %v", r)
		}
	})
	t.Run("policy", func(t *testing.T) {
		m, _, _ := newNodeTestManager(t, []string{"aks-*"}, 1, node("gke-1"))
		_, r := m.Open(context.Background(), nodeOpen("gke-1"))
		if r == nil || r.Reason != agentv1.ExecExitReason_EXEC_EXIT_REASON_POLICY_DENIED {
			t.Fatalf("r = %v", r)
		}
	})
	t.Run("helper rejected deletes nothing left behind", func(t *testing.T) {
		m, _, cs := newNodeTestManager(t, []string{"*"}, 1, node("n1"))
		cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", errors.New(`violates PodSecurity "restricted:latest"`))
		})
		_, r := m.Open(context.Background(), nodeOpen("n1"))
		if r == nil || r.Reason != agentv1.ExecExitReason_EXEC_EXIT_REASON_HELPER_REJECTED || !strings.Contains(r.Message, "PodSecurity") {
			t.Fatalf("r = %v", r)
		}
		if m.nodeSessions != 0 {
			t.Fatal("the node slot must be released on refusal")
		}
	})
	t.Run("node console off", func(t *testing.T) {
		m, _ := newTestManager(t, []config.PodExecRule{{}}, 4, webPod()) // pod-only manager
		_, r := m.Open(context.Background(), nodeOpen("n1"))
		if r == nil || r.Reason != agentv1.ExecExitReason_EXEC_EXIT_REASON_POLICY_DENIED {
			t.Fatalf("r = %v", r)
		}
	})
	t.Run("max sessions is per kind", func(t *testing.T) {
		m, _, _ := newNodeTestManager(t, []string{"*"}, 1, node("n1"))
		s, r := m.Open(context.Background(), nodeOpen("n1"))
		if r != nil {
			t.Fatal(r)
		}
		defer s.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "")
		o := nodeOpen("n1")
		o.SessionId = "nsid-2"
		_, r = m.Open(context.Background(), o)
		if r == nil || r.Reason != agentv1.ExecExitReason_EXEC_EXIT_REASON_TOO_MANY_SESSIONS || !strings.Contains(r.Message, "exec.node.max_sessions") {
			t.Fatalf("r = %v", r)
		}
	})
}

// helperDeleted reports whether the fake clientset recorded a delete of
// the named helper Pod. Read through the action log rather than a Get: the
// tests below install reactors on "get" that answer for the helper.
func helperDeleted(cs *fake.Clientset, name string) bool {
	for _, a := range cs.Actions() {
		d, ok := a.(k8stesting.DeleteAction)
		if ok && a.GetResource().Resource == "pods" && d.GetName() == name {
			return true
		}
	}
	return false
}

// Two failure paths AFTER the helper exists must each delete it and give
// both the node slot and the sessions entry back: no refusal may leave a
// privileged Pod for activeDeadlineSeconds or the next boot's sweep, and
// no refusal may leave a phantom session counted against max_sessions.
func TestOpenNodeFailureAfterHelperCleansUp(t *testing.T) {
	t.Run("helper never running", func(t *testing.T) {
		m, _, cs := newNodeTestManager(t, []string{"*"}, 1, node("n1"))
		m.opts.Node.Settings.HelperReadyTimeoutSec = 1
		// Every lookup sees the helper still Pending: the reactor answers
		// for the create reactor's Running copy in the tracker.
		cs.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
			g := a.(k8stesting.GetAction)
			return true, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: g.GetName(), Namespace: g.GetNamespace()},
				Status:     corev1.PodStatus{Phase: corev1.PodPending},
			}, nil
		})
		_, r := m.Open(context.Background(), nodeOpen("n1"))
		if r == nil || r.Reason != agentv1.ExecExitReason_EXEC_EXIT_REASON_HELPER_REJECTED || !strings.Contains(r.Message, "not running after") {
			t.Fatalf("r = %v", r)
		}
		if !helperDeleted(cs, helperName("nsid")) {
			t.Fatal("the helper was not deleted after the ready wait expired")
		}
		if m.nodeSessions != 0 || len(m.sessions) != 0 {
			t.Fatalf("nodeSessions = %d, sessions = %d; want both 0", m.nodeSessions, len(m.sessions))
		}
	})
	t.Run("executor construction fails", func(t *testing.T) {
		m, _, cs := newNodeTestManager(t, []string{"*"}, 1, node("n1"))
		m.opts.newExecutor = func(*rest.Config, *url.URL) (remotecommand.Executor, error) {
			return nil, errors.New("spdy: no transport")
		}
		_, r := m.Open(context.Background(), nodeOpen("n1"))
		if r == nil || r.Reason != agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL || !strings.Contains(r.Message, "spdy") {
			t.Fatalf("r = %v", r)
		}
		if !helperDeleted(cs, helperName("nsid")) {
			t.Fatal("the helper was not deleted after the executor failed to build")
		}
		if m.nodeSessions != 0 || len(m.sessions) != 0 {
			t.Fatalf("nodeSessions = %d, sessions = %d; want both 0", m.nodeSessions, len(m.sessions))
		}
	})
}

func TestNodeReady(t *testing.T) {
	m, _, _ := newNodeTestManager(t, []string{"*"}, 1)
	if !m.NodeReady() {
		t.Fatal("ready")
	}
	pm, _ := newTestManager(t, nil, 4)
	if pm.NodeReady() {
		t.Fatal("a pod-only manager is not node-ready")
	}
	tr := NewTransport(m, nil, Identity{}, nil)
	if !tr.NodeConsoleReady() {
		t.Fatal("transport must report the manager's readiness")
	}
}

// The pod cap and the node cap are separate sections' limits; a node
// session must not consume a pod slot. Regression for a bug where openNode
// inserted into the same m.sessions map the pod path counts against.
func TestOpenPodMaxSessionsExcludesNodeSessions(t *testing.T) {
	c := &config.Config{}
	on := true
	c.Exec.Pod.Enabled = &on
	c.Exec.Pod.Rules = []config.PodExecRule{{}}
	c.Exec.Pod.MaxSessions = 1
	pol, err := policy.Compile(c)
	if err != nil {
		t.Fatal(err)
	}

	nc := &config.Config{}
	nc.Exec.Node.Enabled = &on
	nc.Exec.Node.Nodes = []string{"*"}
	nc.Exec.Node.Image = "busybox:1.36"
	nc.Exec.Node.MaxSessions = 1
	nc.Exec.Node.HelperReadyTimeoutSec = 5
	np, err := policy.CompileNode(nc)
	if err != nil {
		t.Fatal(err)
	}

	cs := fake.NewSimpleClientset(node("n1"), webPod())
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		p := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		p.Status.Phase = corev1.PodRunning
		return false, nil, nil
	})
	fe := &fakeExecutor{started: make(chan struct{}, 8)}
	m, err := New(Options{
		Policy:   pol,
		Clients:  k8s.ExecClients{Clientset: cs, REST: &rest.Config{Host: "https://example"}},
		Settings: c.ExecPodSettings(),
		Node: &NodeOptions{
			Policy:    np,
			Settings:  nc.ExecNodeSettings(),
			Owner:     &OwnPod{Name: "kubexa-agent-1", Namespace: "kubexa", UID: "u1"},
			Namespace: "kubexa",
		},
		newExecutor: func(*rest.Config, *url.URL) (remotecommand.Executor, error) { return fe, nil },
		helperPoll:  time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A node session first -- it must not consume the pod slot.
	ns, r := m.Open(context.Background(), nodeOpen("n1"))
	if r != nil {
		t.Fatal(r)
	}
	defer ns.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "")

	// Pod cap is 1: exactly one pod session must still succeed...
	ps, r := m.Open(context.Background(), open("dev", "web-1", "app"))
	if r != nil {
		t.Fatalf("pod session refused despite a free pod slot: %v", r)
	}
	defer ps.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "")

	// ...and the next one refuses on the pod cap, not the node cap.
	o := open("dev", "web-1", "app")
	o.SessionId = "pod-2"
	if _, r := m.Open(context.Background(), o); r == nil || r.Reason != agentv1.ExecExitReason_EXEC_EXIT_REASON_TOO_MANY_SESSIONS {
		t.Fatalf("r = %v", r)
	}
}
