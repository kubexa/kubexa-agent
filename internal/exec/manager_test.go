package exec

import (
	"context"
	"net/url"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
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
