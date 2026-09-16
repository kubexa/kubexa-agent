package nodeops

import (
	"os"

	corev1 "k8s.io/api/core/v1"
)

// OwnPod is the agent's own Pod, from the downward API the chart renders
// (POD_NAME, POD_NAMESPACE -- the same contract internal/exec/nodeshell.go
// reads). An empty Name means "unknown": nothing is skipped as self, and
// cmd/agent/main.go logs that once at boot.
type OwnPod struct {
	Namespace string
	Name      string
}

// OwnPodFromEnv reads the downward API variables.
func OwnPodFromEnv() OwnPod {
	return OwnPod{Namespace: os.Getenv("POD_NAMESPACE"), Name: os.Getenv("POD_NAME")}
}

// Is reports whether p is this agent's own Pod.
func (o OwnPod) Is(p *corev1.Pod) bool {
	return o.Name != "" && p.Namespace == o.Namespace && p.Name == o.Name
}
