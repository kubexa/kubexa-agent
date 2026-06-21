package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	testingk8s "k8s.io/client-go/testing"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"

	"github.com/kubexa/kubexa-agent/internal/k8s/k8sconfig"
	"github.com/kubexa/kubexa-agent/internal/logger"
	agentmetrics "github.com/kubexa/kubexa-agent/internal/metrics"
)

func testClient(t *testing.T, kube *fake.Clientset, metrics *metricsfake.Clientset, api *apiMetrics) Client {
	t.Helper()
	if metrics == nil {
		metrics = metricsfake.NewSimpleClientset()
	}
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	return newClient(kube, dyn, metrics, logger.New("k8s-test"), api)
}

func TestDetectInClusterExplicit(t *testing.T) {
	t.Parallel()

	trueVal := true
	falseVal := false

	tests := []struct {
		name string
		cfg  *k8sconfig.Config
		want bool
	}{
		{
			name: "explicit in-cluster",
			cfg:  &k8sconfig.Config{InCluster: &trueVal},
			want: true,
		},
		{
			name: "explicit kubeconfig",
			cfg:  &k8sconfig.Config{InCluster: &falseVal},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := detectInCluster(tt.cfg)
			if err != nil {
				t.Fatalf("detectInCluster() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("detectInCluster() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDetectInClusterAuto(t *testing.T) {
	orig := inClusterConfigFunc
	t.Cleanup(func() { inClusterConfigFunc = orig })

	t.Run("auto-detect in-cluster available", func(t *testing.T) {
		inClusterConfigFunc = func() (*rest.Config, error) {
			return &rest.Config{Host: "https://kubernetes.default.svc"}, nil
		}

		got, err := detectInCluster(&k8sconfig.Config{})
		if err != nil {
			t.Fatalf("detectInCluster() error = %v", err)
		}
		if !got {
			t.Fatalf("detectInCluster() = false, want true")
		}
	})

	t.Run("auto-detect falls back to kubeconfig", func(t *testing.T) {
		inClusterConfigFunc = func() (*rest.Config, error) {
			return nil, errors.New("not in cluster")
		}

		got, err := detectInCluster(&k8sconfig.Config{})
		if err != nil {
			t.Fatalf("detectInCluster() error = %v", err)
		}
		if got {
			t.Fatalf("detectInCluster() = true, want false")
		}
	})
}

func TestResolveRESTConfigDefaults(t *testing.T) {
	orig := inClusterConfigFunc
	t.Cleanup(func() { inClusterConfigFunc = orig })

	falseVal := false
	inClusterConfigFunc = func() (*rest.Config, error) {
		return nil, errors.New("not in cluster")
	}

	dir := t.TempDir()
	kubeconfig := dir + "/config"
	if err := writeTestKubeconfig(kubeconfig); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	cfg := &k8sconfig.Config{
		InCluster:      &falseVal,
		KubeconfigPath: kubeconfig,
	}

	restCfg, err := resolveRESTConfig(cfg)
	if err != nil {
		t.Fatalf("resolveRESTConfig() error = %v", err)
	}
	if restCfg.QPS != k8sconfig.DefaultQPS {
		t.Fatalf("QPS = %v, want %v", restCfg.QPS, k8sconfig.DefaultQPS)
	}
	if restCfg.Burst != k8sconfig.DefaultBurst {
		t.Fatalf("Burst = %v, want %v", restCfg.Burst, k8sconfig.DefaultBurst)
	}
}

func TestNamespaceUID(t *testing.T) {
	t.Parallel()

	const wantUID = "abc-123"
	kube := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kube-system",
			UID:  wantUID,
		},
	})

	c := testClient(t, kube, nil, nil)
	got, err := c.NamespaceUID(context.Background(), "kube-system")
	if err != nil {
		t.Fatalf("NamespaceUID() error = %v", err)
	}
	if got != wantUID {
		t.Fatalf("NamespaceUID() = %q, want %q", got, wantUID)
	}
}

func TestReady(t *testing.T) {
	t.Parallel()

	kube := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	})
	c := testClient(t, kube, nil, nil)

	if err := c.Ready(context.Background()); err != nil {
		t.Fatalf("Ready() error = %v", err)
	}
}

func TestReadyFailure(t *testing.T) {
	t.Parallel()

	kube := fake.NewSimpleClientset()
	kube.PrependReactor("list", "namespaces", func(action testingk8s.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("api unavailable")
	})

	c := testClient(t, kube, nil, nil)
	err := c.Ready(context.Background())
	if err == nil {
		t.Fatal("Ready() expected error")
	}
}

func TestRetryOnTransientError(t *testing.T) {
	attempts := 0
	kube := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kube-system",
			UID:  "uid-1",
		},
	})
	kube.PrependReactor("get", "namespaces", func(action testingk8s.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts < 3 {
			return true, nil, apierrors.NewServiceUnavailable("temporary")
		}
		return false, nil, nil
	})

	logBuf := &bytes.Buffer{}
	log := logger.New("k8s-test", logger.WithWriter(logBuf), logger.WithLevel(logger.LevelWarn))

	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	c := newClient(kube, dyn, metricsfake.NewSimpleClientset(), log, nil)
	uid, err := c.NamespaceUID(context.Background(), "kube-system")
	if err != nil {
		t.Fatalf("NamespaceUID() error = %v", err)
	}
	if uid != "uid-1" {
		t.Fatalf("NamespaceUID() = %q", uid)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if !strings.Contains(logBuf.String(), "retrying") {
		t.Fatalf("expected retry warning log, got: %s", logBuf.String())
	}
}

func TestMetricsRegistration(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := agentmetrics.New(reg, "test", "cluster", "agent")
	if err != nil {
		t.Fatalf("metrics.New() error = %v", err)
	}

	kube := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	})

	c := testClient(t, kube, nil, newAPIMetrics(m.K8s()))
	if err := c.Ready(context.Background()); err != nil {
		t.Fatalf("Ready() error = %v", err)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}

	wantMetrics := map[string]bool{
		"kubexa_k8s_api_requests_total":           false,
		"kubexa_k8s_api_request_duration_seconds": false,
	}
	for _, mf := range mfs {
		if _, ok := wantMetrics[mf.GetName()]; ok {
			wantMetrics[mf.GetName()] = true
		}
	}
	for name, found := range wantMetrics {
		if !found {
			t.Fatalf("metric %q not observed after api call", name)
		}
	}

	var requestCount float64
	for _, mf := range mfs {
		if mf.GetName() != "kubexa_k8s_api_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if hasLabel(m, "status", "success") {
				requestCount += m.GetCounter().GetValue()
			}
		}
	}
	if requestCount < 1 {
		t.Fatalf("expected successful request counter increment, got %v", requestCount)
	}
}

func TestEnableMetrics(t *testing.T) {
	orig := inClusterConfigFunc
	t.Cleanup(func() { inClusterConfigFunc = orig })

	inClusterConfigFunc = func() (*rest.Config, error) {
		return &rest.Config{Host: "https://example.invalid"}, nil
	}

	reg := prometheus.NewRegistry()
	m, err := agentmetrics.New(reg, "test", "cluster", "agent")
	if err != nil {
		t.Fatalf("metrics.New() error = %v", err)
	}

	trueVal := true
	client, err := New(&k8sconfig.Config{
		InCluster: &trueVal,
	}, logger.New("k8s-test"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	client.EnableMetrics(m.K8s())
}

func TestIsTransientError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"service unavailable", apierrors.NewServiceUnavailable("x"), true},
		{"too many requests", apierrors.NewTooManyRequests("x", 1), true},
		{"not found", apierrors.NewNotFound(corev1.Resource("pods"), "x"), false},
		{"nil", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientError(tt.err); got != tt.want {
				t.Fatalf("isTransientError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPodMetrics(t *testing.T) {
	t.Parallel()

	now := metav1.Now()
	want := metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo",
			Namespace: "default",
		},
		Timestamp: now,
		Containers: []metricsv1beta1.ContainerMetrics{
			{
				Name: "app",
				Usage: corev1.ResourceList{
					corev1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
					corev1.ResourceMemory: *resource.NewQuantity(1024, resource.BinarySI),
				},
			},
			{
				Name: "sidecar",
				Usage: corev1.ResourceList{
					corev1.ResourceCPU:    *resource.NewMilliQuantity(50, resource.DecimalSI),
					corev1.ResourceMemory: *resource.NewQuantity(512, resource.BinarySI),
				},
			},
		},
	}

	metricsClient := metricsfake.NewSimpleClientset()
	metricsClient.PrependReactor("list", "pods", func(action testingk8s.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() != "default" {
			return false, nil, nil
		}
		return true, &metricsv1beta1.PodMetricsList{Items: []metricsv1beta1.PodMetrics{want}}, nil
	})

	c := testClient(t, fake.NewSimpleClientset(), metricsClient, nil)
	got, err := c.PodMetrics(context.Background(), "default")
	if err != nil {
		t.Fatalf("PodMetrics() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("PodMetrics() len = %d, want 1", len(got))
	}
	if got[0].CPUMillicores != 150 {
		t.Fatalf("CPUMillicores = %d, want 150", got[0].CPUMillicores)
	}
	if got[0].MemoryBytes != 1536 {
		t.Fatalf("MemoryBytes = %d, want 1536", got[0].MemoryBytes)
	}
}

func writeTestKubeconfig(path string) error {
	cfg := map[string]any{
		"apiVersion": "v1",
		"kind":       "Config",
		"clusters": []map[string]any{
			{
				"name": "test",
				"cluster": map[string]any{
					"server": "https://127.0.0.1:6443",
				},
			},
		},
		"contexts": []map[string]any{
			{
				"name": "test",
				"context": map[string]any{
					"cluster": "test",
					"user":    "test",
				},
			},
		},
		"current-context": "test",
		"users": []map[string]any{
			{
				"name": "test",
				"user": map[string]any{},
			},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func hasLabel(m *dto.Metric, key, value string) bool {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == key && lp.GetValue() == value {
			return true
		}
	}
	return false
}
