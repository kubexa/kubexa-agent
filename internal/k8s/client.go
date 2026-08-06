// Package k8s provides a production-grade Kubernetes API client for kubexa-agent.
package k8s

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/flowcontrol"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/kubexa/kubexa-agent/internal/k8s/k8sconfig"
	"github.com/kubexa/kubexa-agent/internal/logger"
)

// Client exposes typed Kubernetes API operations used by kubexa-agent.
type Client interface {
	// Pods returns a pod lister/watcher for the given namespace.
	Pods(namespace string) PodClient

	// Logs streams logs for a given pod/container.
	Logs(ctx context.Context, namespace, pod, container string, opts LogOptions) (io.ReadCloser, error)

	// Watch returns a watcher for the given resource kind and namespace.
	Watch(ctx context.Context, kind ResourceKind, namespace string, opts WatchOptions) (watch.Interface, error)

	// NodeMetrics returns current CPU/memory metrics for all nodes.
	NodeMetrics(ctx context.Context) ([]NodeMetric, error)

	// PodMetrics returns current CPU/memory metrics for pods in namespace.
	PodMetrics(ctx context.Context, namespace string) ([]PodMetric, error)

	// NamespaceUID returns the UID of a namespace (used for cluster_id generation).
	NamespaceUID(ctx context.Context, name string) (string, error)

	// Ready returns nil if the Kubernetes API is reachable.
	Ready(ctx context.Context) error

	// EnableMetrics attaches Prometheus recorders for Kubernetes API calls.
	EnableMetrics(rec MetricsRecorder)

	// Clientset returns the underlying Kubernetes client for shared informers.
	Clientset() kubernetes.Interface

	// Dynamic returns the dynamic client for arbitrary API resources (CRDs).
	Dynamic() dynamic.Interface
}

// inClusterConfigFunc resolves in-cluster REST config; overridden in tests.
var inClusterConfigFunc = rest.InClusterConfig

type client struct {
	kube    kubernetes.Interface
	dynamic dynamic.Interface
	metrics metricsclientset.Interface
	log     *logger.Logger
	api     *apiMetrics
}

// New constructs a Kubernetes Client from cfg and logger.
func New(cfg *k8sconfig.Config, log *logger.Logger) (Client, error) {
	if cfg == nil {
		return nil, fmt.Errorf("k8s client: config is required")
	}

	restCfg, err := resolveRESTConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s client: resolve rest config: %w", err)
	}

	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s client: create clientset: %w", err)
	}

	metricsClient, err := metricsclientset.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s client: create metrics clientset: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s client: create dynamic clientset: %w", err)
	}

	var apiMetrics *apiMetrics

	return newClient(kube, dynamicClient, metricsClient, log, apiMetrics), nil
}

// NewProbeClientset builds a second clientset from the same resolved REST
// config but with its own QPS/burst budget.
//
// It exists so a several-hundred-GVR capability sweep cannot drain the token
// bucket that the informers share — starving live data collection with our own
// bookkeeping would be a poor trade.
func NewProbeClientset(cfg *k8sconfig.Config, qps float32, burst int) (kubernetes.Interface, error) {
	if cfg == nil {
		return nil, fmt.Errorf("k8s probe client: config is required")
	}
	restCfg, err := resolveRESTConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s probe client: resolve rest config: %w", err)
	}
	if qps > 0 {
		restCfg.QPS = qps
	}
	if burst > 0 {
		restCfg.Burst = burst
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s probe client: create clientset: %w", err)
	}
	return cs, nil
}

// EnableMetrics attaches Prometheus recorders for Kubernetes API calls.
func (c *client) EnableMetrics(rec MetricsRecorder) {
	if c == nil {
		return
	}
	c.api = newAPIMetrics(rec)
}

func newClient(kube kubernetes.Interface, dyn dynamic.Interface, metrics metricsclientset.Interface, log *logger.Logger, api *apiMetrics) Client {
	return &client{
		kube:    kube,
		dynamic: dyn,
		metrics: metrics,
		log:     log,
		api:     api,
	}
}

// Pods returns a namespace-scoped pod client.
func (c *client) Pods(namespace string) PodClient {
	return &podClient{client: c, namespace: namespace}
}

// Logs streams logs for the given pod and container.
func (c *client) Logs(ctx context.Context, namespace, pod, container string, opts LogOptions) (io.ReadCloser, error) {
	logOpts := &corev1.PodLogOptions{
		Container:  container,
		Follow:     opts.Follow,
		Timestamps: opts.Timestamps,
	}
	if opts.TailLines > 0 {
		logOpts.TailLines = &opts.TailLines
	}
	if !opts.SinceTime.IsZero() {
		since := metav1.NewTime(opts.SinceTime.UTC())
		logOpts.SinceTime = &since
	} else if opts.Since > 0 {
		since := metav1.NewTime(time.Now().Add(-opts.Since))
		logOpts.SinceTime = &since
	}

	var stream io.ReadCloser
	err := c.withMetrics(ctx, "get", "pod_logs", func(ctx context.Context) error {
		req := c.kube.CoreV1().Pods(namespace).GetLogs(pod, logOpts)
		var streamErr error
		stream, streamErr = req.Stream(ctx)
		return streamErr
	})
	if err != nil {
		return nil, fmt.Errorf("k8s client: stream pod logs: %w", err)
	}
	return stream, nil
}

// Watch returns a watcher for the given resource kind and namespace.
func (c *client) Watch(ctx context.Context, kind ResourceKind, namespace string, opts WatchOptions) (watch.Interface, error) {
	listOpts := metav1.ListOptions{
		LabelSelector: opts.LabelSelector,
		FieldSelector: opts.FieldSelector,
	}

	method, resource, watchFn, err := c.watchFunc(kind, namespace, listOpts)
	if err != nil {
		return nil, err
	}

	var watcher watch.Interface
	watchErr := c.withMetrics(ctx, method, resource, func(ctx context.Context) error {
		var err error
		watcher, err = watchFn(ctx)
		return err
	})
	if watchErr != nil {
		return nil, fmt.Errorf("k8s client: watch %s: %w", kind, watchErr)
	}
	return watcher, nil
}

// NodeMetrics returns current CPU and memory usage for all nodes.
func (c *client) NodeMetrics(ctx context.Context) ([]NodeMetric, error) {
	var list *metricsv1beta1.NodeMetricsList
	err := c.withMetrics(ctx, "list", "node_metrics", func(ctx context.Context) error {
		var listErr error
		list, listErr = c.metrics.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
		return listErr
	})
	if err != nil {
		return nil, fmt.Errorf("k8s client: list node metrics: %w", err)
	}

	out := make([]NodeMetric, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, nodeMetricFrom(item))
	}
	return out, nil
}

// PodMetrics returns current CPU and memory usage for pods in the namespace.
func (c *client) PodMetrics(ctx context.Context, namespace string) ([]PodMetric, error) {
	var list *metricsv1beta1.PodMetricsList
	err := c.withMetrics(ctx, "list", "pod_metrics", func(ctx context.Context) error {
		var listErr error
		list, listErr = c.metrics.MetricsV1beta1().PodMetricses(namespace).List(ctx, metav1.ListOptions{})
		return listErr
	})
	if err != nil {
		return nil, fmt.Errorf("k8s client: list pod metrics: %w", err)
	}

	out := make([]PodMetric, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, podMetricFrom(item))
	}
	return out, nil
}

// NamespaceUID returns the UID of the named namespace.
func (c *client) NamespaceUID(ctx context.Context, name string) (string, error) {
	var ns *corev1.Namespace
	err := c.withMetrics(ctx, "get", "namespaces", func(ctx context.Context) error {
		var getErr error
		ns, getErr = c.kube.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		return getErr
	})
	if err != nil {
		return "", fmt.Errorf("k8s client: get namespace %q: %w", name, err)
	}
	return string(ns.UID), nil
}

// Clientset returns the underlying Kubernetes clientset.
func (c *client) Clientset() kubernetes.Interface {
	return c.kube
}

// Dynamic returns the underlying dynamic clientset.
func (c *client) Dynamic() dynamic.Interface {
	return c.dynamic
}

// Ready returns nil when the Kubernetes API server is reachable.
func (c *client) Ready(ctx context.Context) error {
	err := c.withMetrics(ctx, "list", "namespaces", func(ctx context.Context) error {
		_, listErr := c.kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 1})
		return listErr
	})
	if err != nil {
		return fmt.Errorf("k8s client: readiness check failed: %w", err)
	}
	return nil
}

func (c *client) withMetrics(ctx context.Context, method, resource string, fn func(context.Context) error) error {
	start := time.Now()
	err := withRetry(ctx, c.log, fmt.Sprintf("%s %s", method, resource), fn)
	elapsed := time.Since(start).Seconds()

	status := "success"
	if err != nil {
		status = "error"
		if c.api != nil {
			c.api.observeError(method, resource, classifyErrorType(err))
		}
	}
	if c.api != nil {
		c.api.observe(method, resource, status, elapsed)
	}
	return err
}

func resolveRESTConfig(cfg *k8sconfig.Config) (*rest.Config, error) {
	useInCluster, err := detectInCluster(cfg)
	if err != nil {
		return nil, err
	}

	var restCfg *rest.Config
	if useInCluster {
		restCfg, err = inClusterConfigFunc()
		if err != nil {
			return nil, fmt.Errorf("in-cluster config: %w", err)
		}
	} else {
		restCfg, err = kubeconfigRESTConfig(cfg.KubeconfigPath)
		if err != nil {
			return nil, err
		}
	}

	qps := cfg.QPS
	if qps <= 0 {
		qps = k8sconfig.DefaultQPS
	}
	burst := cfg.Burst
	if burst <= 0 {
		burst = k8sconfig.DefaultBurst
	}
	restCfg.QPS = qps
	restCfg.Burst = burst

	return restCfg, nil
}

func detectInCluster(cfg *k8sconfig.Config) (bool, error) {
	if cfg.InCluster != nil {
		return *cfg.InCluster, nil
	}

	if _, err := inClusterConfigFunc(); err == nil {
		return true, nil
	}
	return false, nil
}

func kubeconfigRESTConfig(path string) (*rest.Config, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("k8s client: resolve home directory: %w", err)
		}
		path = filepath.Join(home, ".kube", "config")
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = path

	clientCfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
	restCfg, err := clientCfg.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("k8s client: load kubeconfig %q: %w", path, err)
	}
	return restCfg, nil
}

func (c *client) watchFunc(
	kind ResourceKind,
	namespace string,
	listOpts metav1.ListOptions,
) (method, resource string, fn func(context.Context) (watch.Interface, error), err error) {
	switch kind {
	case ResourceKindPod:
		return "watch", "pods", func(ctx context.Context) (watch.Interface, error) {
			return c.kube.CoreV1().Pods(namespace).Watch(ctx, listOpts)
		}, nil
	case ResourceKindService:
		return "watch", "services", func(ctx context.Context) (watch.Interface, error) {
			return c.kube.CoreV1().Services(namespace).Watch(ctx, listOpts)
		}, nil
	case ResourceKindSecret:
		return "watch", "secrets", func(ctx context.Context) (watch.Interface, error) {
			return c.kube.CoreV1().Secrets(namespace).Watch(ctx, listOpts)
		}, nil
	case ResourceKindDeployment:
		return "watch", "deployments", func(ctx context.Context) (watch.Interface, error) {
			return c.kube.AppsV1().Deployments(namespace).Watch(ctx, listOpts)
		}, nil
	case ResourceKindNode:
		return "watch", "nodes", func(ctx context.Context) (watch.Interface, error) {
			return c.kube.CoreV1().Nodes().Watch(ctx, listOpts)
		}, nil
	case ResourceKindNamespace:
		return "watch", "namespaces", func(ctx context.Context) (watch.Interface, error) {
			return c.kube.CoreV1().Namespaces().Watch(ctx, listOpts)
		}, nil
	case ResourceKindConfigMap:
		return "watch", "configmaps", func(ctx context.Context) (watch.Interface, error) {
			return c.kube.CoreV1().ConfigMaps(namespace).Watch(ctx, listOpts)
		}, nil
	case ResourceKindIngress:
		return "watch", "ingresses", func(ctx context.Context) (watch.Interface, error) {
			return c.kube.NetworkingV1().Ingresses(namespace).Watch(ctx, listOpts)
		}, nil
	default:
		return "", "", nil, fmt.Errorf("k8s client: unsupported resource kind %q", kind)
	}
}

func nodeMetricFrom(item metricsv1beta1.NodeMetrics) NodeMetric {
	cpu := item.Usage[corev1.ResourceCPU]
	memory := item.Usage[corev1.ResourceMemory]
	return NodeMetric{
		Name:          item.Name,
		CPUNanocores: cpu.ScaledValue(resource.Nano),
		MemoryBytes:   memory.Value(),
		Timestamp:     item.Timestamp.Time,
		Window:        item.Window.Duration,
	}
}

func podMetricFrom(item metricsv1beta1.PodMetrics) PodMetric {
	var totalCPU, totalMemory int64
	for _, container := range item.Containers {
		if q, ok := container.Usage[corev1.ResourceCPU]; ok {
			totalCPU += q.ScaledValue(resource.Nano)
		}
		if q, ok := container.Usage[corev1.ResourceMemory]; ok {
			totalMemory += q.Value()
		}
	}
	return PodMetric{
		Name:          item.Name,
		Namespace:     item.Namespace,
		CPUNanocores: totalCPU,
		MemoryBytes:   totalMemory,
		Timestamp:     item.Timestamp.Time,
		Window:        item.Window.Duration,
	}
}

// maxCachedRESTClients caps how many per-GroupVersion REST clients the
// live-query path keeps. See the comment at the cache write in
// newQueryClientsFromRest for why a cap is needed and why exceeding it
// degrades instead of failing.
const maxCachedRESTClients = 512

// QueryClients bundles the two Kubernetes clients the live-query path needs.
//
// Both are built from the same REST config but with their own QPS/burst
// budget, for the same reason NewProbeClientset exists: ad-hoc user queries
// must not drain the token bucket the informers share. A user hammering
// refresh should slow down their own queries, not stop data collection.
type QueryClients struct {
	// Dynamic serves the FULL view: whole objects for any GVR, CRDs included.
	Dynamic dynamic.Interface
	// RESTFor serves the TABLE view. Table rendering is an Accept-header
	// negotiation on the raw request, and dynamic.Interface exposes no way to
	// set one, so the executor needs the REST client underneath.
	RESTFor func(gv schema.GroupVersion) (rest.Interface, error)
}

// NewQueryClients builds the live-query clients. Zero qps/burst takes the same
// budget as the main client while still getting a separate limiter.
func NewQueryClients(cfg *k8sconfig.Config, qps float32, burst int) (*QueryClients, error) {
	if cfg == nil {
		return nil, fmt.Errorf("k8s query client: config is required")
	}
	restCfg, err := resolveRESTConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s query client: resolve rest config: %w", err)
	}
	if qps > 0 {
		restCfg.QPS = qps
	}
	if burst > 0 {
		restCfg.Burst = burst
	}
	return newQueryClientsFromRest(restCfg)
}

// newQueryClientsFromRest is the seam tests use to supply a REST config
// without a cluster.
func newQueryClientsFromRest(restCfg *rest.Config) (*QueryClients, error) {
	// One rate limiter, shared by the dynamic client and every per-GroupVersion
	// REST client built below. Setting it explicitly is the whole budget.
	//
	// rest.RESTClientFor builds its OWN token bucket whenever
	// config.RateLimiter is nil (client-go rest/config.go:370-383), so leaving
	// it unset gives every GroupVersion an independent full-strength budget:
	// a query sweep touching N resource kinds would get N buckets and multiply
	// the ceiling by N, defeating the only reason this constructor exists.
	// kubernetes.NewForConfig avoids this by setting the limiter once
	// (clientset.go:498-502), which is what makes NewProbeClientset's separate
	// budget real rather than nominal.
	//
	// Copy first: the limiter must not be attached to the caller's config.
	restCfg = rest.CopyConfig(restCfg)
	if restCfg.RateLimiter == nil {
		qps, burst := restCfg.QPS, restCfg.Burst
		if qps == 0 {
			qps = rest.DefaultQPS
		}
		if burst == 0 {
			burst = rest.DefaultBurst
		}
		if qps > 0 {
			restCfg.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(qps, burst)
		}
	}

	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s query client: create dynamic client: %w", err)
	}

	var mu sync.Mutex
	cache := map[schema.GroupVersion]rest.Interface{}

	restFor := func(gv schema.GroupVersion) (rest.Interface, error) {
		mu.Lock()
		defer mu.Unlock()
		if c, ok := cache[gv]; ok {
			return c, nil
		}
		cfg := rest.CopyConfig(restCfg)
		cfg.GroupVersion = &gv
		// The core group lives under /api; every other group under /apis.
		// Swapping these yields 404s at request time only, on every core
		// resource, which is a miserable thing to debug.
		if gv.Group == "" {
			cfg.APIPath = "/api"
		} else {
			cfg.APIPath = "/apis"
		}
		cfg.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
		c, err := rest.RESTClientFor(cfg)
		if err != nil {
			return nil, fmt.Errorf("k8s query client: rest client for %s: %w", gv, err)
		}
		// Cache only while the cache is small. group/version arrive on the
		// wire, and a query rule of resources: ["*"] permits any of them, so
		// without a ceiling a loop of queries naming fresh GroupVersions grows
		// this map -- and the transports its clients hold -- until the pod is
		// OOM-killed inside the customer's cluster.
		//
		// Past the cap the client is built and returned UNCACHED rather than
		// refused: the cap is far above any real cluster's group/version count
		// (a large cluster with many CRDs lands in the low hundreds), so
		// reaching it means abuse or something equally unexpected, and neither
		// is a reason to break a legitimate cluster's live queries. The
		// degradation is one extra client construction per call.
		if len(cache) < maxCachedRESTClients {
			cache[gv] = c
		}
		return c, nil
	}

	return &QueryClients{Dynamic: dyn, RESTFor: restFor}, nil
}
