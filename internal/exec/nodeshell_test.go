package exec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

func TestHelperPodSpec(t *testing.T) {
	owner := &OwnPod{Name: "kubexa-agent-abc", Namespace: "kubexa", UID: "uid-1"}
	p := helperPod(helperSpec{sessionID: "sid-1", node: "n1", namespace: "kubexa", image: "busybox:1.36",
		owner: owner, maxSession: 30 * time.Minute})
	if !strings.HasPrefix(p.Name, "kubexa-node-shell-") || len(p.Name) != len("kubexa-node-shell-")+8 {
		t.Fatalf("name = %q", p.Name)
	}
	if p.Labels["app.kubernetes.io/name"] != HelperLabelName || p.Labels[helperSessionLabel] != "sid-1" || p.Labels["kubexa.dev/node"] != "n1" {
		t.Fatalf("labels = %v", p.Labels)
	}
	if len(p.OwnerReferences) != 1 || p.OwnerReferences[0].UID != "uid-1" || p.OwnerReferences[0].Kind != "Pod" {
		t.Fatalf("ownerReferences = %+v", p.OwnerReferences)
	}
	s := p.Spec
	if s.NodeName != "n1" || !s.HostPID || !s.HostNetwork || !s.HostIPC || s.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("spec = %+v", s)
	}
	if s.ActiveDeadlineSeconds == nil || *s.ActiveDeadlineSeconds != 1860 {
		t.Fatalf("activeDeadlineSeconds = %v", s.ActiveDeadlineSeconds)
	}
	if s.AutomountServiceAccountToken == nil || *s.AutomountServiceAccountToken {
		t.Fatal("service account token must not be mounted")
	}
	if len(s.Tolerations) != 1 || s.Tolerations[0].Operator != corev1.TolerationOpExists {
		t.Fatalf("tolerations = %+v", s.Tolerations)
	}
	c := s.Containers[0]
	if c.Name != "shell" || c.Image != "busybox:1.36" || len(c.Command) != 2 || c.Command[0] != "sleep" || c.Command[1] != "infinity" {
		t.Fatalf("container = %+v", c)
	}
	if c.SecurityContext == nil || c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged {
		t.Fatal("container must be privileged")
	}
	// No owner: no ownerReference, nothing else changes.
	p2 := helperPod(helperSpec{sessionID: "sid-1", node: "n1", namespace: "other", image: "busybox:1.36", maxSession: time.Minute})
	if len(p2.OwnerReferences) != 0 {
		t.Fatal("an override namespace must carry no ownerReference")
	}
	if p2.Name != p.Name {
		t.Fatal("the name derives from the session id alone")
	}
}

func TestCreateHelperTreatsAlreadyExistsAsSuccess(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := helperSpec{sessionID: "sid", node: "n1", namespace: "kubexa", image: "busybox", maxSession: time.Minute}
	if _, err := createHelper(context.Background(), cs, s); err != nil {
		t.Fatal(err)
	}
	if _, err := createHelper(context.Background(), cs, s); err != nil {
		t.Fatalf("second create: %v", err)
	}
}

func TestCreateHelperMapsAdmissionAndRBACToHelperRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"forbidden by PSA", apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x",
			errors.New(`violates PodSecurity "restricted:latest": privileged`))},
		{"forbidden by RBAC", apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x",
			errors.New(`User "system:serviceaccount:kubexa:kubexa-agent" cannot create resource "pods"`))},
		{"quota", apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", errors.New("exceeded quota"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})
			_, err := createHelper(context.Background(), cs, helperSpec{sessionID: "sid", node: "n1", namespace: "kubexa", image: "busybox", maxSession: time.Minute})
			var he *helperError
			if !errors.As(err, &he) || he.Reason != agentv1.ExecExitReason_EXEC_EXIT_REASON_HELPER_REJECTED {
				t.Fatalf("err = %v", err)
			}
			if !strings.Contains(he.Msg, tc.err.Error()) {
				t.Fatalf("message %q must carry the API server's text", he.Msg)
			}
		})
	}
}

func podWithPhase(ns, name string, phase corev1.PodPhase, waiting string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Status: corev1.PodStatus{Phase: phase}}
	if waiting != "" {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "shell", State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: waiting, Message: "pull access denied"}}}}
	}
	return p
}

func TestAwaitHelperRunning(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		cs := fake.NewSimpleClientset(podWithPhase("kubexa", "h", corev1.PodRunning, ""))
		if err := awaitHelperRunning(context.Background(), cs, "kubexa", "h", time.Second, time.Millisecond); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("image pull fails early", func(t *testing.T) {
		cs := fake.NewSimpleClientset(podWithPhase("kubexa", "h", corev1.PodPending, "ErrImagePull"))
		start := time.Now()
		err := awaitHelperRunning(context.Background(), cs, "kubexa", "h", 5*time.Second, time.Millisecond)
		var he *helperError
		if !errors.As(err, &he) || !strings.Contains(he.Msg, "ErrImagePull") || !strings.Contains(he.Msg, "pull access denied") {
			t.Fatalf("err = %v", err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("a pull failure must end the wait before the timeout")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		cs := fake.NewSimpleClientset(podWithPhase("kubexa", "h", corev1.PodPending, ""))
		err := awaitHelperRunning(context.Background(), cs, "kubexa", "h", 20*time.Millisecond, time.Millisecond)
		var he *helperError
		if !errors.As(err, &he) || !strings.Contains(he.Msg, "not running after") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("pod failed", func(t *testing.T) {
		cs := fake.NewSimpleClientset(podWithPhase("kubexa", "h", corev1.PodFailed, ""))
		err := awaitHelperRunning(context.Background(), cs, "kubexa", "h", time.Second, time.Millisecond)
		var he *helperError
		if !errors.As(err, &he) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestDeleteHelperTreatsNotFoundAsSuccess(t *testing.T) {
	cs := fake.NewSimpleClientset()
	if err := deleteHelper(context.Background(), cs, "kubexa", "missing"); err != nil {
		t.Fatal(err)
	}
}

func TestNodeLabelValueFitsPodValidation(t *testing.T) {
	short := strings.Repeat("a", 63)
	if got := nodeLabelValue(short); got != short {
		t.Fatalf("63-char name must be stored verbatim, got %q", got)
	}
	long := strings.Repeat("b", 64)
	got := nodeLabelValue(long)
	if len(got) != 63 {
		t.Fatalf("len = %d, want 63: %q", len(got), got)
	}
	if !strings.HasPrefix(got, strings.Repeat("b", 55)+"-") {
		t.Fatalf("prefix wrong: %q", got)
	}
	if errs := validation.IsValidLabelValue(got); len(errs) != 0 {
		t.Fatalf("not a valid label value: %v", errs)
	}
	// Two names sharing a 55-char prefix get different values.
	other := strings.Repeat("b", 55) + strings.Repeat("c", 198)
	if o := nodeLabelValue(other); o == got {
		t.Fatalf("collision: %q for both", o)
	}
	// helperPod uses it.
	p := helperPod(helperSpec{sessionID: "s", node: long, namespace: "kubexa", image: "busybox", maxSession: time.Minute})
	if p.Labels[helperNodeLabel] != got || p.Spec.NodeName != long {
		t.Fatalf("label = %q nodeName = %q", p.Labels[helperNodeLabel], p.Spec.NodeName)
	}
}

func TestSweepHelpersDeletesEveryLabelledPod(t *testing.T) {
	mk := func(name string, labelled bool) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "kubexa", Name: name}}
		if labelled {
			p.Labels = map[string]string{"app.kubernetes.io/name": HelperLabelName}
		}
		return p
	}
	cs := fake.NewSimpleClientset(mk("kubexa-node-shell-a", true), mk("kubexa-node-shell-b", true), mk("kubexa-agent-x", false))
	if n := SweepHelpers(context.Background(), cs, "kubexa", nil); n != 2 {
		t.Fatalf("swept %d, want 2", n)
	}
	left, _ := cs.CoreV1().Pods("kubexa").List(context.Background(), metav1.ListOptions{})
	if len(left.Items) != 1 || left.Items[0].Name != "kubexa-agent-x" {
		t.Fatalf("left = %+v", left.Items)
	}
}

func TestResolveOwnPod(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "kubexa", Name: "kubexa-agent-1", UID: "u1"}})
	got, err := ResolveOwnPod(context.Background(), cs, "kubexa-agent-1", "kubexa")
	if err != nil || got.UID != "u1" || got.Namespace != "kubexa" {
		t.Fatalf("got %+v err %v", got, err)
	}
	if _, err := ResolveOwnPod(context.Background(), cs, "", "kubexa"); err == nil {
		t.Fatal("empty name must fail: the chart did not render POD_NAME")
	}
	if _, err := ResolveOwnPod(context.Background(), cs, "ghost", "kubexa"); err == nil {
		t.Fatal("a missing pod must fail")
	}
}
