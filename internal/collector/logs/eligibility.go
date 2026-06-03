package logs

import (
	corev1 "k8s.io/api/core/v1"
)

// podEligible reports whether a pod should be considered for log collection.
func podEligible(pod *corev1.Pod, excluded []string) bool {
	if pod == nil || pod.DeletionTimestamp != nil {
		return false
	}
	for _, ns := range excluded {
		if pod.Namespace == ns {
			return false
		}
	}
	switch pod.Status.Phase {
	case corev1.PodRunning, corev1.PodPending, corev1.PodFailed, corev1.PodSucceeded:
		return true
	default:
		return false
	}
}

// containerLoggable reports whether logs may be read for the container in its current state.
func containerLoggable(pod *corev1.Pod, name string) bool {
	if pod == nil || !containerInSpec(pod, name) {
		return false
	}
	st := containerStatus(pod, name)
	if st == nil {
		// Scheduled pod; kubelet has not reported status yet.
		return pod.Status.Phase == corev1.PodPending
	}
	switch {
	case st.State.Running != nil:
		return true
	case st.State.Waiting != nil:
		// CrashLoopBackOff and image pull backoff still expose prior log bytes.
		return true
	case st.State.Terminated != nil:
		return true
	default:
		return false
	}
}

func containerInSpec(pod *corev1.Pod, name string) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return true
		}
	}
	return false
}

func containerStatus(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// containersForPod returns container names to stream for the pod given an optional allow-list.
// An empty allow-list collects all loggable containers in the pod spec.
func containersForPod(pod *corev1.Pod, allowed []string) []string {
	if pod == nil {
		return nil
	}
	if len(allowed) == 0 {
		names := make([]string, 0, len(pod.Spec.Containers))
		for _, c := range pod.Spec.Containers {
			if containerLoggable(pod, c.Name) {
				names = append(names, c.Name)
			}
		}
		return names
	}
	out := make([]string, 0, len(allowed))
	for _, name := range allowed {
		if containerLoggable(pod, name) {
			out = append(out, name)
		}
	}
	return out
}

// mergeContainerNames unions rule-level container filters.
// A nil slice means all containers in the pod; if either side is nil, the result is nil.
func mergeContainerNames(existing, add []string) []string {
	if existing == nil || add == nil {
		return nil
	}
	if len(add) == 0 {
		return append([]string(nil), existing...)
	}
	if len(existing) == 0 {
		return append([]string(nil), add...)
	}
	seen := make(map[string]struct{}, len(existing)+len(add))
	out := make([]string, 0, len(existing)+len(add))
	for _, name := range append(existing, add...) {
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}
