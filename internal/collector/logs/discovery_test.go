package logs

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubexa/kubexa-agent/internal/logger"
	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
)

func TestMergeRuleView(t *testing.T) {
	t.Parallel()

	a := ruleView{id: "a", ruleIDs: []string{"a"}, tailLines: 50, follow: false, containers: []string{"api"}}
	b := ruleView{id: "b", ruleIDs: []string{"b"}, tailLines: 200, follow: true, containers: []string{"sidecar"}}

	got := mergeRuleView(a, b)
	if got.tailLines != 200 {
		t.Fatalf("tailLines = %d, want 200", got.tailLines)
	}
	if !got.follow {
		t.Fatal("follow = false, want true")
	}
	if len(got.ruleIDs) != 2 {
		t.Fatalf("ruleIDs = %v, want 2 entries", got.ruleIDs)
	}
	if len(got.containers) != 2 {
		t.Fatalf("containers = %v, want union of api and sidecar", got.containers)
	}

	got = mergeRuleView(
		ruleView{containers: []string{"api"}},
		ruleView{containers: []string{"sidecar", "api"}},
	)
	if len(got.containers) != 2 {
		t.Fatalf("containers = %v, want union of two names", got.containers)
	}
}

func TestMergeContainerNames(t *testing.T) {
	t.Parallel()

	if got := mergeContainerNames([]string{"a"}, []string{}); len(got) != 1 || got[0] != "a" {
		t.Fatalf("merge with empty add = %v, want [a]", got)
	}
	if got := mergeContainerNames(nil, nil); got != nil {
		t.Fatalf("merge all + all = %v, want nil", got)
	}
	if got := mergeContainerNames([]string{"a"}, []string{"b", "a"}); len(got) != 2 {
		t.Fatalf("union = %v, want [a b]", got)
	}
	if got := mergeContainerNames(nil, []string{"b"}); got != nil {
		t.Fatalf("merge with all containers = %v, want nil", got)
	}
}

func TestContainerLoggable_waiting(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app",
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
				},
			}},
		},
	}
	if !containerLoggable(pod, "app") {
		t.Fatal("CrashLoopBackOff container should be loggable")
	}
}

func TestPodEligible_pending(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	if !podEligible(pod, nil) {
		t.Fatal("pending pod should be eligible")
	}
}

func TestTargetsForPod_mergesRules(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.Rules = []pkgconfig.LogNamespaceRule{
		{ID: "r1", Namespace: "prod", TailLines: ptrInt64(10), Follow: ptrBool(false)},
		{ID: "r2", Namespace: "prod", TailLines: ptrInt64(100), Follow: ptrBool(true)},
	}
	cfg.ApplyDefaults()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-1",
			Namespace: "prod",
			UID:       "uid-1",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "api"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "api",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}

	c := &Collector{cfg: cfg, log: logger.New("test")}
	targets := c.targetsForPod(pod)
	key := streamKey{podUID: "uid-1", containerName: "api"}
	target, ok := targets[key]
	if !ok {
		t.Fatal("expected merged stream target")
	}
	if target.rule.tailLines != 100 {
		t.Fatalf("tailLines = %d, want 100 from merged rules", target.rule.tailLines)
	}
	if !target.rule.follow {
		t.Fatal("follow = false, want true from merged rules")
	}
	if len(target.rule.ruleIDs) != 2 {
		t.Fatalf("ruleIDs = %v, want both rules", target.rule.ruleIDs)
	}
}

func ptrInt64(v int64) *int64 { return &v }

func ptrBool(v bool) *bool { return &v }
