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
	// Ignored when SinceTime is set.
	Since time.Duration
	// SinceTime returns logs at or after this instant (API semantics: strictly after).
	// Used on reconnect to avoid re-reading the full log file.
	SinceTime time.Time
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
	CPUNanocores int64
	MemoryBytes  int64
	Timestamp    time.Time
	Window       time.Duration
}

// PodMetric holds current CPU and memory usage for a Kubernetes pod.
type PodMetric struct {
	Name         string
	Namespace    string
	CPUNanocores int64
	MemoryBytes   int64
	Timestamp     time.Time
	Window        time.Duration
}

// NodeInfo is one node as the scrape path needs to see it: an address to
// reach, the kubelet's port, and whether the node is in a state worth
// scraping. It is deliberately not the whole corev1.Node -- holding those
// would put every node's full status in the agent's heap on every refresh.
type NodeInfo struct {
	Name string
	// InternalIP is the node's InternalIP address. Nodes without one are not
	// returned at all: they cannot be scraped, and a target with an empty host
	// reports as broken rather than as absent.
	InternalIP string
	// KubeletPort is status.daemonEndpoints.kubeletEndpoint.Port, which is
	// 10250 on a stock cluster but is configurable and is read rather than
	// assumed. Zero is replaced with 10250 by the caller building the URL.
	KubeletPort int32
	// Ready is the NodeReady condition. A not-ready node is still returned:
	// its kubelet often still serves /metrics/cadvisor, and dropping it would
	// turn a node problem into missing data with no explanation.
	Ready  bool
	Labels map[string]string
}
