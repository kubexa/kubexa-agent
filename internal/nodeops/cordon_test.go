package nodeops

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestSetUnschedulablePatchesOnlyOnChange(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}})
	patches := 0
	cs.PrependReactor("patch", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
		patches++
		return false, nil, nil // let the tracker apply it
	})
	changed, err := SetUnschedulable(context.Background(), cs, "w1", true, false)
	if err != nil || !changed {
		t.Fatalf("cordon: changed=%v err=%v", changed, err)
	}
	n, _ := cs.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Fatal("node not cordoned")
	}
	changed, err = SetUnschedulable(context.Background(), cs, "w1", true, false)
	if err != nil || changed {
		t.Fatalf("second cordon: changed=%v err=%v (must be a no-op)", changed, err)
	}
	if patches != 1 {
		t.Fatalf("patches = %d, want 1", patches)
	}
}

func TestSetUnschedulableDryRunCarriesDryRunAll(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}})
	var seen []string
	cs.PrependReactor("patch", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
		seen = a.(k8stesting.PatchActionImpl).GetPatchOptions().DryRun
		return true, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}}, nil
	})
	if _, err := SetUnschedulable(context.Background(), cs, "w1", true, true); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != metav1.DryRunAll {
		t.Fatalf("DryRun = %v, want [All]", seen)
	}
	n, _ := cs.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{})
	if n.Spec.Unschedulable {
		t.Fatal("dry run changed the node")
	}
}
