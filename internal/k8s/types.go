package k8s

import "time"

// ResourceKind identifies a Kubernetes resource type for watch operations.
type ResourceKind string

const (
	// ResourceKindPod watches Pod resources.
	ResourceKindPod ResourceKind = "Pod"
	// ResourceKindService watches Service resources.
	ResourceKindService ResourceKind = "Service"
	// ResourceKindSecret watches Secret resources.
	ResourceKindSecret ResourceKind = "Secret"
	// ResourceKindDeployment watches Deployment resources.
	ResourceKindDeployment ResourceKind = "Deployment"
	// ResourceKindNode watches Node resources.
	ResourceKindNode ResourceKind = "Node"
	// ResourceKindNamespace watches Namespace resources.
	ResourceKindNamespace ResourceKind = "Namespace"
	// ResourceKindConfigMap watches ConfigMap resources.
	ResourceKindConfigMap ResourceKind = "ConfigMap"
	// ResourceKindIngress watches Ingress resources.
	ResourceKindIngress ResourceKind = "Ingress"
)

// LogOptions configures pod log streaming.
type LogOptions struct {
	// TailLines limits log output to the last N lines.
	TailLines int64
	// Follow streams logs as they are written.
	Follow bool
	// Since returns logs newer than this duration relative to the request time.
	Since time.Duration
	// Timestamps prefixes each log line with a timestamp.
	Timestamps bool
}

// WatchOptions configures resource watch operations.
type WatchOptions struct {
	// LabelSelector filters watched objects by labels.
	LabelSelector string
	// FieldSelector filters watched objects by fields.
	FieldSelector string
	// ResyncPeriod is reserved for informer-style resync; not sent to the API watch call.
	ResyncPeriod time.Duration
}

// NodeMetric holds current CPU and memory usage for a Kubernetes node.
type NodeMetric struct {
	Name          string
	Namespace     string
	CPUMillicores int64
	MemoryBytes   int64
	Timestamp     time.Time
}

// PodMetric holds current CPU and memory usage for a Kubernetes pod.
type PodMetric struct {
	Name          string
	Namespace     string
	CPUMillicores int64
	MemoryBytes   int64
	Timestamp     time.Time
}
