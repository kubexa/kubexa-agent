package logs

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
)

func TestConfigFromRootCheckpointDir(t *testing.T) {
	t.Parallel()

	root := pkgconfig.Default()
	root.Collect.Logs.CheckpointDir = "/data/checkpoints"
	root.Normalize()

	cfg := ConfigFromRoot(root)
	if cfg.CheckpointDir != "/data/checkpoints" {
		t.Fatalf("CheckpointDir = %q, want /data/checkpoints", cfg.CheckpointDir)
	}
}

func TestConfigFromRootExcludeNamespaces(t *testing.T) {
	t.Parallel()

	root := pkgconfig.Default()
	root.Collect.Logs.ExcludeNamespaces = []string{"kube-system", "kube-public"}
	root.Normalize()

	cfg := ConfigFromRoot(root)
	if len(cfg.ExcludeNamespaces) != 2 {
		t.Fatalf("ExcludeNamespaces = %v, want 2 entries", cfg.ExcludeNamespaces)
	}
	if cfg.ExcludeNamespaces[0] != "kube-system" || cfg.ExcludeNamespaces[1] != "kube-public" {
		t.Fatalf("ExcludeNamespaces = %v", cfg.ExcludeNamespaces)
	}
}

func TestPodEligibleExcludeNamespaces(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "coredns-abc"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if podEligible(pod, []string{"kube-system"}) {
		t.Fatal("kube-system pod should be excluded")
	}
	if !podEligible(pod, nil) {
		t.Fatal("pod should be eligible when exclude list is empty")
	}

	terminating := pod.DeepCopy()
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	if podEligible(terminating, nil) {
		t.Fatal("terminating pod should not be eligible")
	}
}
