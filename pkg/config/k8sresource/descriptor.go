// Package k8sresource maps configured resource names to Kubernetes API metadata.
package k8sresource

import (
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Descriptor identifies a Kubernetes API resource for state collection.
type Descriptor struct {
	GVR       schema.GroupVersionResource
	ProtoKind agentv1.ResourceKind
	// MetricLabel is the plural resource name used in Prometheus labels.
	MetricLabel string
	// SkipAdded omits ADDED events (used for secrets).
	SkipAdded bool
}

// APIVersion returns the group/version string for proto export (e.g. "batch/v1").
func (d Descriptor) APIVersion() string {
	gv := d.GVR.GroupVersion()
	if gv.Group == "" {
		return gv.Version
	}
	return gv.String()
}
