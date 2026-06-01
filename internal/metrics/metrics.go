// Package metrics defines and registers all Prometheus metrics for the kubexa-agent.
// Other packages receive a *Metrics instance via constructor injection and must not
// register metrics directly.
package metrics

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

const metricNamespace = "kubexa"

var (
	grpcRequestDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5}
	k8sRequestDurationBuckets  = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}
	connectionStates           = []string{
		"idle",
		"connecting",
		"handshaking",
		"ready",
		"transient_failure",
		"shutdown",
	}
)

// Metrics holds all agent-wide Prometheus metrics and exposes typed sub-recorders.
type Metrics struct {
	agentInfo *prometheus.GaugeVec

	queue      *QueueMetrics
	stream     *StreamMetrics
	k8s        *K8sMetrics
	collector  *CollectorMetrics
	health     *HealthMetrics
	connection *ConnectionMetrics
}

// QueueMetrics records buffer queue instrumentation.
type QueueMetrics struct {
	depth         *prometheus.GaugeVec
	enqueuedTotal prometheus.Counter
	dequeuedTotal prometheus.Counter
	droppedTotal  prometheus.Counter
	ackTotal      prometheus.Counter
	nackTotal     prometheus.Counter
	diskBytes     prometheus.Gauge
}

// StreamMetrics records gRPC stream and unary call instrumentation.
type StreamMetrics struct {
	streamActive           prometheus.Gauge
	streamMessagesSent     prometheus.Counter
	streamMessagesReceived prometheus.Counter
	streamErrorsTotal      *prometheus.CounterVec
	requestsTotal          *prometheus.CounterVec
	requestDuration        *prometheus.HistogramVec
}

// K8sMetrics records Kubernetes API client instrumentation.
type K8sMetrics struct {
	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	errorsTotal     *prometheus.CounterVec
}

// CollectorMetrics records data collection instrumentation.
type CollectorMetrics struct {
	logsCollectedTotal  *prometheus.CounterVec
	logsBytesTotal      *prometheus.CounterVec
	stateEventsTotal    *prometheus.CounterVec
	metricScrapesTotal  *prometheus.CounterVec
	activeStreams       *prometheus.GaugeVec
}

// HealthMetrics records component health status.
type HealthMetrics struct {
	status *prometheus.GaugeVec
}

// ConnectionMetrics records gateway connection lifecycle instrumentation.
type ConnectionMetrics struct {
	reconnectsTotal prometheus.Counter
	state           *prometheus.GaugeVec
}

// New constructs Metrics, registers all collectors on reg, and sets agent identity.
func New(reg prometheus.Registerer, version, clusterID, agentID string) (*Metrics, error) {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	agentInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Name:      "agent_info",
			Help:      "Agent identity and version. Value is always 1 when the agent is running.",
		},
		[]string{"version", "cluster_id", "agent_id"},
	)

	queue := &QueueMetrics{
		depth: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: metricNamespace,
				Subsystem: "queue",
				Name:      "depth",
				Help:      "Current number of items in the queue by storage tier.",
			},
			[]string{"tier"},
		),
		enqueuedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: "queue",
				Name:      "enqueued_total",
				Help:      "Total number of items enqueued.",
			},
		),
		dequeuedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: "queue",
				Name:      "dequeued_total",
				Help:      "Total number of items dequeued.",
			},
		),
		droppedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: "queue",
				Name:      "dropped_total",
				Help:      "Total number of items dropped due to capacity limits.",
			},
		),
		ackTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: "queue",
				Name:      "ack_total",
				Help:      "Total number of acknowledged (delivered) items.",
			},
		),
		nackTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: "queue",
				Name:      "nack_total",
				Help:      "Total number of negative-acknowledged (requeued) items.",
			},
		),
		diskBytes: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricNamespace,
				Subsystem: "queue",
				Name:      "disk_bytes",
				Help:      "Total bytes used by on-disk spill segments.",
			},
		),
	}

	stream := &StreamMetrics{
		streamActive: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricNamespace,
				Subsystem: "grpc",
				Name:      "stream_active",
				Help:      "1 when a bidirectional stream is active, 0 otherwise.",
			},
		),
		streamMessagesSent: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: "grpc",
				Name:      "stream_messages_sent_total",
				Help:      "Total messages sent on the bidirectional stream.",
			},
		),
		streamMessagesReceived: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: "grpc",
				Name:      "stream_messages_received_total",
				Help:      "Total messages received on the bidirectional stream.",
			},
		),
		streamErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: "grpc",
				Name:      "stream_errors_total",
				Help:      "Total bidirectional stream errors by type.",
			},
			[]string{"error_type"},
		),
		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: "grpc",
				Name:      "requests_total",
				Help:      "Total unary gRPC requests by method and status.",
			},
			[]string{"method", "status"},
		),
		requestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: metricNamespace,
				Subsystem: "grpc",
				Name:      "request_duration_seconds",
				Help:      "Unary gRPC request duration in seconds.",
				Buckets:   grpcRequestDurationBuckets,
			},
			[]string{"method"},
		),
	}

	k8s := &K8sMetrics{
		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: "k8s",
				Name:      "api_requests_total",
				Help:      "Total number of Kubernetes API requests.",
			},
			[]string{"method", "resource", "status"},
		),
		requestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: metricNamespace,
				Subsystem: "k8s",
				Name:      "api_request_duration_seconds",
				Help:      "Kubernetes API request duration in seconds.",
				Buckets:   k8sRequestDurationBuckets,
			},
			[]string{"method", "resource"},
		),
		errorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: "k8s",
				Name:      "api_errors_total",
				Help:      "Total number of Kubernetes API errors.",
			},
			[]string{"method", "resource", "error_type"},
		),
	}

	collector := &CollectorMetrics{
		logsCollectedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: "collector",
				Name:      "logs_collected_total",
				Help:      "Total log records collected by namespace.",
			},
			[]string{"namespace"},
		),
		logsBytesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: "collector",
				Name:      "logs_bytes_total",
				Help:      "Total log bytes collected by namespace.",
			},
			[]string{"namespace"},
		),
		stateEventsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: "collector",
				Name:      "state_events_total",
				Help:      "Total Kubernetes state events collected.",
			},
			[]string{"kind", "event_type"},
		),
		metricScrapesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: "collector",
				Name:      "metric_scrapes_total",
				Help:      "Total metric scrape attempts by endpoint and status.",
			},
			[]string{"endpoint", "status"},
		),
		activeStreams: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: metricNamespace,
				Subsystem: "collector",
				Name:      "active_streams",
				Help:      "Number of active log streams by namespace.",
			},
			[]string{"namespace"},
		),
	}

	health := &HealthMetrics{
		status: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: metricNamespace,
				Subsystem: "health",
				Name:      "status",
				Help:      "Component health status. 1 = healthy, 0 = unhealthy.",
			},
			[]string{"component"},
		),
	}

	connection := &ConnectionMetrics{
		reconnectsTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: "connection",
				Name:      "reconnects_total",
				Help:      "Total number of gateway reconnect attempts.",
			},
		),
		state: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: metricNamespace,
				Subsystem: "connection",
				Name:      "state",
				Help:      "Gateway connection state. 1 for the current state, 0 for all others.",
			},
			[]string{"state"},
		),
	}

	m := &Metrics{
		agentInfo:  agentInfo,
		queue:      queue,
		stream:     stream,
		k8s:        k8s,
		collector:  collector,
		health:     health,
		connection: connection,
	}

	if err := registerAll(reg,
		agentInfo,
		queue.depth,
		queue.enqueuedTotal,
		queue.dequeuedTotal,
		queue.droppedTotal,
		queue.ackTotal,
		queue.nackTotal,
		queue.diskBytes,
		stream.streamActive,
		stream.streamMessagesSent,
		stream.streamMessagesReceived,
		stream.streamErrorsTotal,
		stream.requestsTotal,
		stream.requestDuration,
		k8s.requestsTotal,
		k8s.requestDuration,
		k8s.errorsTotal,
		collector.logsCollectedTotal,
		collector.logsBytesTotal,
		collector.stateEventsTotal,
		collector.metricScrapesTotal,
		collector.activeStreams,
		health.status,
		connection.reconnectsTotal,
		connection.state,
	); err != nil {
		return nil, err
	}

	agentInfo.WithLabelValues(version, clusterID, agentID).Set(1)

	for _, state := range connectionStates {
		value := float64(0)
		if state == "idle" {
			value = 1
		}
		connection.state.WithLabelValues(state).Set(value)
	}

	return m, nil
}

// Queue returns queue-specific metric recorders.
func (m *Metrics) Queue() *QueueMetrics {
	if m == nil {
		return nil
	}
	return m.queue
}

// Stream returns stream-specific metric recorders.
func (m *Metrics) Stream() *StreamMetrics {
	if m == nil {
		return nil
	}
	return m.stream
}

// K8s returns Kubernetes client metric recorders.
func (m *Metrics) K8s() *K8sMetrics {
	if m == nil {
		return nil
	}
	return m.k8s
}

// Collector returns collector metric recorders.
func (m *Metrics) Collector() *CollectorMetrics {
	if m == nil {
		return nil
	}
	return m.collector
}

// Health returns health metric recorders.
func (m *Metrics) Health() *HealthMetrics {
	if m == nil {
		return nil
	}
	return m.health
}

// Connection returns connection metric recorders.
func (m *Metrics) Connection() *ConnectionMetrics {
	if m == nil {
		return nil
	}
	return m.connection
}

// IncEnqueued increments the enqueued counter.
func (q *QueueMetrics) IncEnqueued() {
	if q == nil {
		return
	}
	q.enqueuedTotal.Inc()
}

// IncDequeued increments the dequeued counter.
func (q *QueueMetrics) IncDequeued() {
	if q == nil {
		return
	}
	q.dequeuedTotal.Inc()
}

// IncDropped increments the dropped counter.
func (q *QueueMetrics) IncDropped() {
	if q == nil {
		return
	}
	q.droppedTotal.Inc()
}

// IncAck increments the ack counter.
func (q *QueueMetrics) IncAck() {
	if q == nil {
		return
	}
	q.ackTotal.Inc()
}

// IncNack increments the nack counter by count.
func (q *QueueMetrics) IncNack(count int) {
	if q == nil || count <= 0 {
		return
	}
	q.nackTotal.Add(float64(count))
}

// SetDepth sets the queue depth for the given storage tier.
func (q *QueueMetrics) SetDepth(tier string, depth float64) {
	if q == nil {
		return
	}
	q.depth.WithLabelValues(tier).Set(depth)
}

// SetDiskBytes sets the total on-disk queue bytes.
func (q *QueueMetrics) SetDiskBytes(bytes float64) {
	if q == nil {
		return
	}
	q.diskBytes.Set(bytes)
}

// AddDiskBytes adjusts the on-disk queue bytes gauge by delta.
func (q *QueueMetrics) AddDiskBytes(delta float64) {
	if q == nil {
		return
	}
	q.diskBytes.Add(delta)
}

// SetStreamActive sets whether a bidirectional stream is active.
func (s *StreamMetrics) SetStreamActive(active bool) {
	if s == nil {
		return
	}
	if active {
		s.streamActive.Set(1)
		return
	}
	s.streamActive.Set(0)
}

// IncMessagesSent increments the stream messages sent counter.
func (s *StreamMetrics) IncMessagesSent() {
	if s == nil {
		return
	}
	s.streamMessagesSent.Inc()
}

// IncMessagesReceived increments the stream messages received counter.
func (s *StreamMetrics) IncMessagesReceived() {
	if s == nil {
		return
	}
	s.streamMessagesReceived.Inc()
}

// IncStreamError increments stream errors for the given error type.
func (s *StreamMetrics) IncStreamError(errorType string) {
	if s == nil {
		return
	}
	s.streamErrorsTotal.WithLabelValues(errorType).Inc()
}

// IncRequest increments unary gRPC requests for method and status.
func (s *StreamMetrics) IncRequest(method, status string) {
	if s == nil {
		return
	}
	s.requestsTotal.WithLabelValues(method, status).Inc()
}

// ObserveRequestDuration records unary gRPC request duration in seconds.
func (s *StreamMetrics) ObserveRequestDuration(method string, seconds float64) {
	if s == nil {
		return
	}
	s.requestDuration.WithLabelValues(method).Observe(seconds)
}

// ObserveRequest records a successful Kubernetes API request.
func (k *K8sMetrics) ObserveRequest(method, resource, status string, seconds float64) {
	if k == nil {
		return
	}
	k.requestsTotal.WithLabelValues(method, resource, status).Inc()
	k.requestDuration.WithLabelValues(method, resource).Observe(seconds)
}

// IncError increments Kubernetes API errors for the given error type.
func (k *K8sMetrics) IncError(method, resource, errorType string) {
	if k == nil {
		return
	}
	k.errorsTotal.WithLabelValues(method, resource, errorType).Inc()
}

// IncLogsCollected increments collected log records for namespace.
func (c *CollectorMetrics) IncLogsCollected(namespace string) {
	if c == nil {
		return
	}
	c.logsCollectedTotal.WithLabelValues(namespace).Inc()
}

// AddLogsBytes adds collected log bytes for namespace.
func (c *CollectorMetrics) AddLogsBytes(namespace string, bytes float64) {
	if c == nil {
		return
	}
	c.logsBytesTotal.WithLabelValues(namespace).Add(bytes)
}

// IncStateEvent increments state events for kind and event type.
func (c *CollectorMetrics) IncStateEvent(kind, eventType string) {
	if c == nil {
		return
	}
	c.stateEventsTotal.WithLabelValues(kind, eventType).Inc()
}

// IncMetricScrape increments metric scrape attempts for endpoint and status.
func (c *CollectorMetrics) IncMetricScrape(endpoint, status string) {
	if c == nil {
		return
	}
	c.metricScrapesTotal.WithLabelValues(endpoint, status).Inc()
}

// SetActiveStreams sets the number of active log streams for namespace.
func (c *CollectorMetrics) SetActiveStreams(namespace string, count float64) {
	if c == nil {
		return
	}
	c.activeStreams.WithLabelValues(namespace).Set(count)
}

// SetHealthy marks component as healthy (1).
func (h *HealthMetrics) SetHealthy(component string) {
	if h == nil {
		return
	}
	h.status.WithLabelValues(component).Set(1)
}

// SetUnhealthy marks component as unhealthy (0).
func (h *HealthMetrics) SetUnhealthy(component string) {
	if h == nil {
		return
	}
	h.status.WithLabelValues(component).Set(0)
}

// IncReconnects increments the gateway reconnect counter.
func (c *ConnectionMetrics) IncReconnects() {
	if c == nil {
		return
	}
	c.reconnectsTotal.Inc()
}

// SetState sets the current gateway connection state gauge to 1 and all others to 0.
func (c *ConnectionMetrics) SetState(state string) {
	if c == nil {
		return
	}
	for _, s := range connectionStates {
		value := float64(0)
		if s == state {
			value = 1
		}
		c.state.WithLabelValues(s).Set(value)
	}
}

func registerAll(reg prometheus.Registerer, collectors ...prometheus.Collector) error {
	for _, collector := range collectors {
		if err := reg.Register(collector); err != nil {
			return fmt.Errorf("register metric: %w", err)
		}
	}
	return nil
}
