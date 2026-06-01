package metrics

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/kubexa/kubexa-agent/internal/logger"
)

func TestNewRegistersAllMetrics(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := New(reg, "1.0.0", "cluster-a", "agent-1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if m == nil {
		t.Fatal("New() returned nil metrics")
	}

	// Vec metrics are omitted from Gather until at least one label set is observed.
	m.Queue().SetDepth("memory", 0)
	m.Stream().IncStreamError("init")
	m.Stream().IncRequest("init", "init")
	m.Stream().ObserveRequestDuration("init", 0)
	m.K8s().ObserveRequest("init", "init", "init", 0)
	m.K8s().IncError("init", "init", "init")
	m.Collector().IncLogsCollected("init")
	m.Collector().AddLogsBytes("init", 0)
	m.Collector().IncStateEvent("init", "init")
	m.Collector().IncMetricScrape("init", "init")
	m.Collector().SetActiveStreams("init", 0)
	m.Health().SetHealthy("init")
	m.Connection().SetState("idle")

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}

	want := map[string]bool{
		"kubexa_agent_info":                           false,
		"kubexa_queue_depth":                          false,
		"kubexa_queue_enqueued_total":                 false,
		"kubexa_queue_dequeued_total":                 false,
		"kubexa_queue_dropped_total":                  false,
		"kubexa_queue_ack_total":                      false,
		"kubexa_queue_nack_total":                     false,
		"kubexa_queue_disk_bytes":                     false,
		"kubexa_grpc_stream_active":                   false,
		"kubexa_grpc_stream_messages_sent_total":      false,
		"kubexa_grpc_stream_messages_received_total":  false,
		"kubexa_grpc_stream_errors_total":             false,
		"kubexa_grpc_requests_total":                  false,
		"kubexa_grpc_request_duration_seconds":        false,
		"kubexa_k8s_api_requests_total":               false,
		"kubexa_k8s_api_request_duration_seconds":     false,
		"kubexa_k8s_api_errors_total":                 false,
		"kubexa_collector_logs_collected_total":       false,
		"kubexa_collector_logs_bytes_total":           false,
		"kubexa_collector_state_events_total":         false,
		"kubexa_collector_metric_scrapes_total":       false,
		"kubexa_collector_active_streams":             false,
		"kubexa_health_status":                        false,
		"kubexa_connection_reconnects_total":          false,
		"kubexa_connection_state":                     false,
	}

	for _, mf := range families {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric %q was not registered", name)
		}
	}

	assertGaugeValue(t, reg, "kubexa_agent_info", 1, map[string]string{
		"version":    "1.0.0",
		"cluster_id": "cluster-a",
		"agent_id":   "agent-1",
	})
}

func TestNewDuplicateRegistrationFails(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	if _, err := New(reg, "1.0.0", "cluster-a", "agent-1"); err != nil {
		t.Fatalf("first New() error = %v", err)
	}

	if _, err := New(reg, "1.0.0", "cluster-a", "agent-1"); err == nil {
		t.Fatal("second New() error = nil, want duplicate registration error")
	}
}

func TestQueueMetricsAccessors(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := New(reg, "dev", "c", "a")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	q := m.Queue()
	q.IncEnqueued()
	q.IncDequeued()
	q.IncDropped()
	q.IncAck()
	q.IncNack(3)
	q.SetDepth("memory", 10)
	q.SetDepth("disk", 2)
	q.SetDiskBytes(4096)
	q.AddDiskBytes(512)

	assertCounterValue(t, reg, "kubexa_queue_enqueued_total", 1)
	assertCounterValue(t, reg, "kubexa_queue_dequeued_total", 1)
	assertCounterValue(t, reg, "kubexa_queue_dropped_total", 1)
	assertCounterValue(t, reg, "kubexa_queue_ack_total", 1)
	assertCounterValue(t, reg, "kubexa_queue_nack_total", 3)
	assertGaugeValue(t, reg, "kubexa_queue_depth", 10, map[string]string{"tier": "memory"})
	assertGaugeValue(t, reg, "kubexa_queue_depth", 2, map[string]string{"tier": "disk"})
	assertGaugeValue(t, reg, "kubexa_queue_disk_bytes", 4608, nil)
}

func TestStreamMetricsAccessors(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := New(reg, "dev", "c", "a")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	s := m.Stream()
	s.SetStreamActive(true)
	s.IncMessagesSent()
	s.IncMessagesReceived()
	s.IncStreamError("send")
	s.IncRequest("/agent.v1.Agent/Connect", "OK")
	s.ObserveRequestDuration("/agent.v1.Agent/Connect", 0.05)

	assertGaugeValue(t, reg, "kubexa_grpc_stream_active", 1, nil)
	assertCounterValue(t, reg, "kubexa_grpc_stream_messages_sent_total", 1)
	assertCounterValue(t, reg, "kubexa_grpc_stream_messages_received_total", 1)
	assertCounterValue(t, reg, "kubexa_grpc_stream_errors_total", 1, map[string]string{"error_type": "send"})
	assertCounterValue(t, reg, "kubexa_grpc_requests_total", 1, map[string]string{
		"method": "/agent.v1.Agent/Connect",
		"status": "OK",
	})
}

func TestK8sMetricsAccessors(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := New(reg, "dev", "c", "a")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	k := m.K8s()
	k.ObserveRequest("list", "pods", "success", 0.12)
	k.IncError("watch", "pods", "timeout")

	assertCounterValue(t, reg, "kubexa_k8s_api_requests_total", 1, map[string]string{
		"method":   "list",
		"resource": "pods",
		"status":   "success",
	})
	assertCounterValue(t, reg, "kubexa_k8s_api_errors_total", 1, map[string]string{
		"method":     "watch",
		"resource":   "pods",
		"error_type": "timeout",
	})
}

func TestCollectorMetricsAccessors(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := New(reg, "dev", "c", "a")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	c := m.Collector()
	c.IncLogsCollected("default")
	c.AddLogsBytes("default", 128)
	c.IncStateEvent("Pod", "Added")
	c.IncMetricScrape("kube-state", "success")
	c.SetActiveStreams("default", 4)

	assertCounterValue(t, reg, "kubexa_collector_logs_collected_total", 1, map[string]string{"namespace": "default"})
	assertCounterValue(t, reg, "kubexa_collector_logs_bytes_total", 128, map[string]string{"namespace": "default"})
	assertCounterValue(t, reg, "kubexa_collector_state_events_total", 1, map[string]string{
		"kind":       "Pod",
		"event_type": "Added",
	})
	assertCounterValue(t, reg, "kubexa_collector_metric_scrapes_total", 1, map[string]string{
		"endpoint": "kube-state",
		"status":   "success",
	})
	assertGaugeValue(t, reg, "kubexa_collector_active_streams", 4, map[string]string{"namespace": "default"})
}

func TestHealthMetricsAccessors(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := New(reg, "dev", "c", "a")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	h := m.Health()
	h.SetHealthy("k8s")
	h.SetUnhealthy("gateway")

	assertGaugeValue(t, reg, "kubexa_health_status", 1, map[string]string{"component": "k8s"})
	assertGaugeValue(t, reg, "kubexa_health_status", 0, map[string]string{"component": "gateway"})
}

func TestConnectionMetricsAccessors(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := New(reg, "dev", "c", "a")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	conn := m.Connection()
	conn.IncReconnects()
	conn.SetState("ready")

	assertCounterValue(t, reg, "kubexa_connection_reconnects_total", 1)
	assertGaugeValue(t, reg, "kubexa_connection_state", 1, map[string]string{"state": "ready"})
	assertGaugeValue(t, reg, "kubexa_connection_state", 0, map[string]string{"state": "idle"})
}

func TestServerMetricsEndpoint(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := New(reg, "dev", "c", "a"); err != nil {
		t.Fatalf("New() error = %v", err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()

	srv := NewServer(addr, reg, logger.New("metrics-test"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- srv.Run(ctx)
	}()

	waitForServer(t, addr)

	for _, path := range []string{"/metrics", "/metrics/json"} {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s error = %v", path, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read body %s error = %v", path, readErr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200, body = %s", path, resp.StatusCode, string(body))
		}
		if path == "/metrics" && !strings.Contains(string(body), "kubexa_agent_info") {
			t.Fatalf("GET /metrics body missing kubexa_agent_info")
		}
		if path == "/metrics/json" && !strings.Contains(string(body), `"name": "kubexa_agent_info"`) {
			t.Fatalf("GET /metrics/json body missing kubexa_agent_info")
		}
	}

	cancel()

	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Fatalf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down within timeout")
	}
}

func waitForServer(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s did not become ready", addr)
}

func assertCounterValue(t *testing.T, reg *prometheus.Registry, name string, want float64, labels ...map[string]string) {
	t.Helper()
	got := gatherMetricValue(t, reg, name, dto.MetricType_COUNTER, labels...)
	if got != want {
		t.Fatalf("counter %s = %v, want %v", name, got, want)
	}
}

func assertGaugeValue(t *testing.T, reg *prometheus.Registry, name string, want float64, labels map[string]string) {
	t.Helper()
	got := gatherMetricValue(t, reg, name, dto.MetricType_GAUGE, labels)
	if got != want {
		t.Fatalf("gauge %s = %v, want %v", name, got, want)
	}
}

func gatherMetricValue(t *testing.T, reg *prometheus.Registry, name string, typ dto.MetricType, labels ...map[string]string) float64 {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}

	var labelFilter map[string]string
	if len(labels) > 0 {
		labelFilter = labels[0]
	}

	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		if mf.GetType() != typ {
			t.Fatalf("metric %s type = %v, want %v", name, mf.GetType(), typ)
		}
		for _, metric := range mf.GetMetric() {
			if !labelsMatch(metric.GetLabel(), labelFilter) {
				continue
			}
			switch typ {
			case dto.MetricType_COUNTER:
				return metric.GetCounter().GetValue()
			case dto.MetricType_GAUGE:
				return metric.GetGauge().GetValue()
			default:
				t.Fatalf("unsupported metric type %v for %s", typ, name)
			}
		}
	}

	t.Fatalf("metric %s with labels %v not found", name, labelFilter)
	return 0
}

func labelsMatch(pairs []*dto.LabelPair, want map[string]string) bool {
	if len(want) == 0 {
		return len(pairs) == 0
	}
	if len(pairs) != len(want) {
		return false
	}
	for _, lp := range pairs {
		if want[lp.GetName()] != lp.GetValue() {
			return false
		}
	}
	return true
}
