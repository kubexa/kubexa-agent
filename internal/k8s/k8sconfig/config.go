// Package k8sconfig holds configuration for the Kubernetes API client.
package k8sconfig

import "github.com/prometheus/client_golang/prometheus"

const (
	// DefaultQPS is the default client-go REST client QPS limit.
	DefaultQPS float32 = 50
	// DefaultBurst is the default client-go REST client burst limit.
	DefaultBurst int = 100
)

// Config configures how the Kubernetes client connects to the API server.
type Config struct {
	// InCluster selects in-cluster REST configuration when non-nil.
	// When nil, the client auto-detects in-cluster mode via rest.InClusterConfig()
	// and falls back to kubeconfig when detection fails.
	InCluster *bool

	// KubeconfigPath is the kubeconfig file path for local development or
	// self-hosted deployments. Defaults to ~/.kube/config when empty.
	KubeconfigPath string

	// QPS sets the client-go rate limiter QPS. Defaults to DefaultQPS.
	QPS float32

	// Burst sets the client-go rate limiter burst. Defaults to DefaultBurst.
	Burst int

	// MetricsRegisterer registers Prometheus metrics. When nil, metrics are not registered.
	MetricsRegisterer prometheus.Registerer
}
