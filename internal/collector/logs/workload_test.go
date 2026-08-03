package logs

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func podOwnedBy(kind, name string) *corev1.Pod {
	ctrl := true
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "api-1", Namespace: "stage",
		OwnerReferences: []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &ctrl}},
	}}
}

// A pod's direct owner is a ReplicaSet whose name changes on every rollout.
// Labelling logs with it would split one deployment's history in two on each
// deploy, which is the opposite of what the label is for.
func TestResolveWalksReplicaSetToDeployment(t *testing.T) {
	ctrl := true
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "api-7d9f", Namespace: "stage",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api", Controller: &ctrl}},
	}}
	r := newWorkloadResolver(fake.NewSimpleClientset(rs), time.Minute)

	got := r.Resolve(context.Background(), podOwnedBy("ReplicaSet", "api-7d9f"))
	if got.Kind != "Deployment" || got.Name != "api" {
		t.Fatalf("resolved = %+v, want Deployment/api", got)
	}
}

func TestResolveUsesTheDirectOwnerWhenItIsTheWorkload(t *testing.T) {
	r := newWorkloadResolver(fake.NewSimpleClientset(), time.Minute)
	got := r.Resolve(context.Background(), podOwnedBy("StatefulSet", "db"))
	if got.Kind != "StatefulSet" || got.Name != "db" {
		t.Fatalf("resolved = %+v", got)
	}
}

// An uncontrolled pod has no workload. Falling back to the pod name would put
// pod cardinality into a label whose whole purpose is to survive pod churn.
func TestResolveLeavesAnUncontrolledPodEmpty(t *testing.T) {
	r := newWorkloadResolver(fake.NewSimpleClientset(), time.Minute)
	got := r.Resolve(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "debug", Namespace: "stage"},
	})
	if got.Name != "" || got.Kind != "" {
		t.Fatalf("resolved = %+v, want empty", got)
	}
}

// One API call per ReplicaSet, not per log line.
func TestResolveCachesTheLookup(t *testing.T) {
	ctrl := true
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "api-7d9f", Namespace: "stage",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api", Controller: &ctrl}},
	}}
	client := fake.NewSimpleClientset(rs)
	r := newWorkloadResolver(client, time.Minute)
	pod := podOwnedBy("ReplicaSet", "api-7d9f")

	r.Resolve(context.Background(), pod)
	r.Resolve(context.Background(), pod)

	gets := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == "get" && a.GetResource().Resource == "replicasets" {
			gets++
		}
	}
	if gets != 1 {
		t.Fatalf("replicaset GETs = %d, want 1", gets)
	}
}

// A lookup that fails must not cost a log line its identity or retry forever.
func TestResolveFallsBackToTheDirectOwnerOnLookupFailure(t *testing.T) {
	r := newWorkloadResolver(fake.NewSimpleClientset(), time.Minute)
	got := r.Resolve(context.Background(), podOwnedBy("ReplicaSet", "missing-rs"))
	if got.Kind != "ReplicaSet" || got.Name != "missing-rs" {
		t.Fatalf("resolved = %+v, want the direct owner", got)
	}
}
