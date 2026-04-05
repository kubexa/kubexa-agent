package k8s

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
)

const maxChangeHints = 32
const maxAnnotationHints = 8

// PodChangeHints returns short strings describing what changed between old and new.
// Nil old is treated as no hints (caller may use ADDED semantics).
func PodChangeHints(old, cur *corev1.Pod) []string {
	if old == nil || cur == nil || old.Name != cur.Name || old.Namespace != cur.Namespace {
		return nil
	}

	var hints []string

	if old.Status.Phase != cur.Status.Phase {
		hints = append(hints, fmt.Sprintf("status.phase: %s -> %s", old.Status.Phase, cur.Status.Phase))
	}

	if old.Spec.NodeName != cur.Spec.NodeName {
		hints = append(hints, fmt.Sprintf("spec.nodeName: %q -> %q", old.Spec.NodeName, cur.Spec.NodeName))
	}

	hints = append(hints, stringMapDiffHints("metadata.labels", old.Labels, cur.Labels)...)

	ann := stringMapDiffHints("metadata.annotations", old.Annotations, cur.Annotations)
	if len(ann) > maxAnnotationHints {
		ann = ann[:maxAnnotationHints]
	}
	hints = append(hints, ann...)

	hints = append(hints, containerStatusDiffHints(old.Status.ContainerStatuses, cur.Status.ContainerStatuses)...)

	if len(hints) > maxChangeHints {
		hints = hints[:maxChangeHints]
		hints = append(hints, "... (truncated)")
	}
	return hints
}

func stringMapDiffHints(prefix string, oldM, curM map[string]string) []string {
	if len(oldM) == 0 && len(curM) == 0 {
		return nil
	}
	keys := make(map[string]struct{})
	for k := range oldM {
		keys[k] = struct{}{}
	}
	for k := range curM {
		keys[k] = struct{}{}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	var out []string
	for _, k := range sorted {
		ov, oOk := oldM[k]
		nv, nOk := curM[k]
		switch {
		case !oOk && nOk:
			out = append(out, fmt.Sprintf("%s.%s: (none) -> %q", prefix, k, nv))
		case oOk && !nOk:
			out = append(out, fmt.Sprintf("%s.%s: %q -> (removed)", prefix, k, ov))
		case oOk && nOk && ov != nv:
			out = append(out, fmt.Sprintf("%s.%s: %q -> %q", prefix, k, ov, nv))
		}
	}
	return out
}

func containerStatusDiffHints(oldCS, curCS []corev1.ContainerStatus) []string {
	oldByName := make(map[string]*corev1.ContainerStatus, len(oldCS))
	for i := range oldCS {
		st := &oldCS[i]
		oldByName[st.Name] = st
	}

	var hints []string
	for i := range curCS {
		cur := &curCS[i]
		old := oldByName[cur.Name]
		prefix := fmt.Sprintf("status.containerStatuses.%s", cur.Name)
		if old == nil {
			hints = append(hints, fmt.Sprintf("%s: (new container status)", prefix))
			continue
		}
		if old.Ready != cur.Ready {
			hints = append(hints, fmt.Sprintf("%s.ready: %v -> %v", prefix, old.Ready, cur.Ready))
		}
		if old.RestartCount != cur.RestartCount {
			hints = append(hints, fmt.Sprintf("%s.restartCount: %d -> %d", prefix, old.RestartCount, cur.RestartCount))
		}
		if old.Image != cur.Image {
			hints = append(hints, fmt.Sprintf("%s.image: %q -> %q", prefix, old.Image, cur.Image))
		}
		os := containerStateString(old)
		cs := containerStateString(cur)
		if os != cs {
			hints = append(hints, fmt.Sprintf("%s.state: %s -> %s", prefix, os, cs))
		}
		or := containerReasonString(old)
		cr := containerReasonString(cur)
		if or != cr && (or != "" || cr != "") {
			hints = append(hints, fmt.Sprintf("%s.reason: %q -> %q", prefix, or, cr))
		}
	}
	return hints
}

func containerStateString(st *corev1.ContainerStatus) string {
	if st == nil {
		return ""
	}
	switch {
	case st.State.Running != nil:
		return "running"
	case st.State.Waiting != nil:
		return "waiting"
	case st.State.Terminated != nil:
		return "terminated"
	default:
		return "unknown"
	}
}

func containerReasonString(st *corev1.ContainerStatus) string {
	if st == nil {
		return ""
	}
	if st.State.Waiting != nil {
		return st.State.Waiting.Reason
	}
	if st.State.Terminated != nil {
		return st.State.Terminated.Reason
	}
	return ""
}
