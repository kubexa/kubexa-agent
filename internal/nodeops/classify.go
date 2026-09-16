package nodeops

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// Reasons carried in NodeJobPod.reason. The app keys its copy on these.
const (
	ReasonDaemonSet    = "daemonset"
	ReasonMirror       = "mirror"
	ReasonAgentSelf    = "agent_self"
	ReasonTerminating  = "terminating"
	ReasonPDB          = "pdb"
	ReasonNoController = "no_controller"
	ReasonEmptyDir     = "emptydir"
	ReasonDryRun       = "dry_run"
)

const mirrorAnnotation = "kubernetes.io/config.mirror"

// PodClass is one row of the job's pod table.
type PodClass struct {
	Namespace string
	Name      string
	UID       types.UID
	State     agentv1.NodeJobPodState
	Reason    string
}

// Preflight is the classified table plus what the drain demands before it
// may start. NeedsForce / NeedsEmptyDir hold "namespace/name" keys.
type Preflight struct {
	Pods          []PodClass
	NeedsForce    []string
	NeedsEmptyDir []string
}

// Classify applies kubectl drain's filters, in this order: mirror pods and
// DaemonSet pods are skipped; the agent's own pod is skipped (decision 6);
// a pod already terminating is waited for, not re-evicted; a pod with no
// controller needs force; a pod mounting emptyDir needs
// delete_emptydir_data. Finished pods (Succeeded/Failed) need neither, as
// in kubectl. Every pod stays in the table -- the user sees what was left
// alone and why.
func Classify(pods []corev1.Pod, own OwnPod, force, deleteEmptyDir bool) Preflight {
	pre := Preflight{Pods: make([]PodClass, 0, len(pods))}
	for i := range pods {
		p := &pods[i]
		row := PodClass{Namespace: p.Namespace, Name: p.Name, UID: p.UID, State: agentv1.NodeJobPodState_NODE_JOB_POD_STATE_PENDING}
		key := p.Namespace + "/" + p.Name
		switch {
		case p.Annotations[mirrorAnnotation] != "":
			row.State, row.Reason = agentv1.NodeJobPodState_NODE_JOB_POD_STATE_SKIPPED, ReasonMirror
		case controllerKind(p) == "DaemonSet":
			row.State, row.Reason = agentv1.NodeJobPodState_NODE_JOB_POD_STATE_SKIPPED, ReasonDaemonSet
		case own.Is(p):
			row.State, row.Reason = agentv1.NodeJobPodState_NODE_JOB_POD_STATE_SKIPPED, ReasonAgentSelf
		case p.DeletionTimestamp != nil:
			row.State, row.Reason = agentv1.NodeJobPodState_NODE_JOB_POD_STATE_EVICTING, ReasonTerminating
		case !finished(p) && controllerKind(p) == "" && !force:
			row.Reason = ReasonNoController
			pre.NeedsForce = append(pre.NeedsForce, key)
		case !finished(p) && hasEmptyDir(p) && !deleteEmptyDir:
			row.Reason = ReasonEmptyDir
			pre.NeedsEmptyDir = append(pre.NeedsEmptyDir, key)
		}
		pre.Pods = append(pre.Pods, row)
	}
	return pre
}

func controllerKind(p *corev1.Pod) string {
	for _, ref := range p.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return ref.Kind
		}
	}
	return ""
}

func finished(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

func hasEmptyDir(p *corev1.Pod) bool {
	for _, v := range p.Spec.Volumes {
		if v.EmptyDir != nil {
			return true
		}
	}
	return false
}
