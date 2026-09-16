package nodeops

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// SetUnschedulable cordons (true) or uncordons (false) a node with a merge
// patch on spec.unschedulable. It reads the node first and patches only on
// a change, so "already cordoned" is a no-op the job can say out loud rather
// than a write. dryRun sends dryRun=All: the API server runs admission and
// answers, the node is untouched.
func SetUnschedulable(ctx context.Context, cs kubernetes.Interface, node string, unschedulable, dryRun bool) (bool, error) {
	n, err := cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if n.Spec.Unschedulable == unschedulable {
		return false, nil
	}
	patch := fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, unschedulable)
	opts := metav1.PatchOptions{}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	if _, err := cs.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, []byte(patch), opts); err != nil {
		return false, err
	}
	return true, nil
}
