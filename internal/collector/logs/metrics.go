package logs

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	metricNamespace = "agent"
)

// metrics holds Prometheus instrumentation for the log collector.
type metrics struct {
	linesTotal       *prometheus.CounterVec
	droppedTotal     *prometheus.CounterVec
	activeStreams    prometheus.Gauge
	streamErrorsTotal          *prometheus.CounterVec
	bytesProcessedTotal        prometheus.Counter
	checkpointWritesTotal      prometheus.Counter
	checkpointErrorsTotal      *prometheus.CounterVec
}

func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	m := &metrics{
		linesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Name:      "log_collector_lines_total",
				Help:      "Total log lines processed by the log collector.",
			},
			[]string{"namespace", "pod", "level"},
		),
		droppedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Name:      "log_collector_dropped_total",
				Help:      "Total log lines or messages dropped by the log collector.",
			},
			[]string{"namespace", "pod", "reason"},
		),
		activeStreams: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricNamespace,
				Name:      "log_collector_active_streams",
				Help:      "Number of active pod log streams.",
			},
		),
		streamErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Name:      "log_collector_stream_errors_total",
				Help:      "Total pod log stream errors.",
			},
			[]string{"error_type"},
		),
		bytesProcessedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Name:      "log_collector_bytes_processed_total",
				Help:      "Total bytes read from pod log streams.",
			},
		),
		checkpointWritesTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Name:      "log_collector_checkpoint_writes_total",
				Help:      "Total persisted log stream checkpoints.",
			},
		),
		checkpointErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Name:      "log_collector_checkpoint_errors_total",
				Help:      "Total log stream checkpoint store errors.",
			},
			[]string{"op"},
		),
	}

	collectors := []prometheus.Collector{
		m.linesTotal,
		m.droppedTotal,
		m.activeStreams,
		m.streamErrorsTotal,
		m.bytesProcessedTotal,
		m.checkpointWritesTotal,
		m.checkpointErrorsTotal,
	}
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("register log collector metric: %w", err)
		}
	}
	return m, nil
}

func (m *metrics) incLines(namespace, pod, level string) {
	if m == nil {
		return
	}
	m.linesTotal.WithLabelValues(namespace, pod, level).Inc()
}

func (m *metrics) incDropped(namespace, pod, reason string) {
	if m == nil {
		return
	}
	m.droppedTotal.WithLabelValues(namespace, pod, reason).Inc()
}

func (m *metrics) setActiveStreams(n int) {
	if m == nil {
		return
	}
	m.activeStreams.Set(float64(n))
}

func (m *metrics) incStreamError(errorType string) {
	if m == nil {
		return
	}
	m.streamErrorsTotal.WithLabelValues(errorType).Inc()
}

func (m *metrics) addBytes(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.bytesProcessedTotal.Add(float64(n))
}

func (m *metrics) incCheckpointWrite() {
	if m == nil {
		return
	}
	m.checkpointWritesTotal.Inc()
}

func (m *metrics) incCheckpointError(op string) {
	if m == nil {
		return
	}
	m.checkpointErrorsTotal.WithLabelValues(op).Inc()
}
