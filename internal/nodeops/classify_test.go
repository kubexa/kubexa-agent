package nodeops

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

func ctrl(kind string) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{Kind: kind, Name: "owner", Controller: &yes}}
}

func pod(ns, name string, mut func(*corev1.Pod)) corev1.Pod {
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("uid-" + name)}}
	if mut != nil {
		mut(&p)
	}
	return p
}

func find(t *testing.T, pre Preflight, ns, name string) PodClass {
	t.Helper()
	for _, p := range pre.Pods {
		if p.Namespace == ns && p.Name == name {
			return p
		}
	}
	t.Fatalf("%s/%s not in the table", ns, name)
	return PodClass{}
}

func TestClassifyRules(t *testing.T) {
	own := OwnPod{Namespace: "kubexa", Name: "kubexa-agent-1"}
	now := metav1.Now()
	pods := []corev1.Pod{
		pod("kube-system", "kube-proxy-1", func(p *corev1.Pod) { p.OwnerReferences = ctrl("DaemonSet") }),
		pod("kube-system", "etcd-node", func(p *corev1.Pod) {
			p.Annotations = map[string]string{"kubernetes.io/config.mirror": "abc"}
		}),
		pod("kubexa", "kubexa-agent-1", func(p *corev1.Pod) { p.OwnerReferences = ctrl("ReplicaSet") }),
		pod("app", "web-1", func(p *corev1.Pod) { p.OwnerReferences = ctrl("ReplicaSet") }),
		pod("app", "leaving", func(p *corev1.Pod) { p.OwnerReferences = ctrl("ReplicaSet"); p.DeletionTimestamp = &now }),
		pod("app", "bare", nil),
		pod("app", "done", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }),
		pod("app", "scratch", func(p *corev1.Pod) {
			p.OwnerReferences = ctrl("StatefulSet")
			p.Spec.Volumes = []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		}),
	}

	pre := Classify(pods, own, false, false)
	want := map[string]struct {
		state  agentv1.NodeJobPodState
		reason string
	}{
		"kube-proxy-1":   {agentv1.NodeJobPodState_NODE_JOB_POD_STATE_SKIPPED, ReasonDaemonSet},
		"etcd-node":      {agentv1.NodeJobPodState_NODE_JOB_POD_STATE_SKIPPED, ReasonMirror},
		"kubexa-agent-1": {agentv1.NodeJobPodState_NODE_JOB_POD_STATE_SKIPPED, ReasonAgentSelf},
		"web-1":          {agentv1.NodeJobPodState_NODE_JOB_POD_STATE_PENDING, ""},
		"leaving":        {agentv1.NodeJobPodState_NODE_JOB_POD_STATE_EVICTING, ReasonTerminating},
		"bare":           {agentv1.NodeJobPodState_NODE_JOB_POD_STATE_PENDING, ReasonNoController},
		"done":           {agentv1.NodeJobPodState_NODE_JOB_POD_STATE_PENDING, ""}, // finished pods need no force (kubectl parity)
		"scratch":        {agentv1.NodeJobPodState_NODE_JOB_POD_STATE_PENDING, ReasonEmptyDir},
	}
	if len(pre.Pods) != len(pods) {
		t.Fatalf("table has %d rows, want %d (every pod is listed, skipped ones included)", len(pre.Pods), len(pods))
	}
	for name, w := range want {
		ns := "app"
		if name == "kube-proxy-1" || name == "etcd-node" {
			ns = "kube-system"
		}
		if name == "kubexa-agent-1" {
			ns = "kubexa"
		}
		got := find(t, pre, ns, name)
		if got.State != w.state || got.Reason != w.reason {
			t.Errorf("%s: %v/%q, want %v/%q", name, got.State, got.Reason, w.state, w.reason)
		}
	}
	if len(pre.NeedsForce) != 1 || pre.NeedsForce[0] != "app/bare" {
		t.Errorf("NeedsForce = %v, want [app/bare]", pre.NeedsForce)
	}
	if len(pre.NeedsEmptyDir) != 1 || pre.NeedsEmptyDir[0] != "app/scratch" {
		t.Errorf("NeedsEmptyDir = %v, want [app/scratch]", pre.NeedsEmptyDir)
	}

	// With both flags the same two pods are plain PENDING and nothing is
	// demanded.
	pre = Classify(pods, own, true, true)
	if len(pre.NeedsForce)+len(pre.NeedsEmptyDir) != 0 {
		t.Fatalf("flags did not clear the demands: %v %v", pre.NeedsForce, pre.NeedsEmptyDir)
	}
	if got := find(t, pre, "app", "bare"); got.State != agentv1.NodeJobPodState_NODE_JOB_POD_STATE_PENDING || got.Reason != "" {
		t.Fatalf("forced bare pod: %v/%q", got.State, got.Reason)
	}
}

func TestClassifyWithNoOwnPodSkipsNothingAsSelf(t *testing.T) {
	pods := []corev1.Pod{pod("kubexa", "kubexa-agent-1", func(p *corev1.Pod) { p.OwnerReferences = ctrl("ReplicaSet") })}
	pre := Classify(pods, OwnPod{}, false, false)
	if got := pre.Pods[0]; got.State != agentv1.NodeJobPodState_NODE_JOB_POD_STATE_PENDING {
		t.Fatalf("an empty OwnPod matched a pod: %+v", got)
	}
}
