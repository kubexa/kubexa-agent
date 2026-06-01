package queue

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	metricNamespace = "kubexa"
	metricSubsystem = "queue"
)

// queueMetrics holds Prometheus instrumentation for the buffer queue.
type queueMetrics struct {
	depth          *prometheus.GaugeVec
	enqueuedTotal  prometheus.Counter
	dequeuedTotal  prometheus.Counter
	droppedTotal   prometheus.Counter
	ackTotal       prometheus.Counter
	nackTotal      prometheus.Counter
	diskBytes      prometheus.Gauge
}

// newQueueMetrics constructs and registers queue metrics on reg.
func newQueueMetrics(reg prometheus.Registerer) (*queueMetrics, error) {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	m := &queueMetrics{
		depth: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: metricNamespace,
				Subsystem: metricSubsystem,
				Name:      "depth",
				Help:      "Current number of items in the queue by storage tier.",
			},
			[]string{"tier"},
		),
		enqueuedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: metricSubsystem,
				Name:      "enqueued_total",
				Help:      "Total number of items enqueued.",
			},
		),
		dequeuedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: metricSubsystem,
				Name:      "dequeued_total",
				Help:      "Total number of items dequeued.",
			},
		),
		droppedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: metricSubsystem,
				Name:      "dropped_total",
				Help:      "Total number of items dropped due to capacity limits.",
			},
		),
		ackTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: metricSubsystem,
				Name:      "ack_total",
				Help:      "Total number of acknowledged (delivered) items.",
			},
		),
		nackTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: metricSubsystem,
				Name:      "nack_total",
				Help:      "Total number of negative-acknowledged (requeued) items.",
			},
		),
		diskBytes: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricNamespace,
				Subsystem: metricSubsystem,
				Name:      "disk_bytes",
				Help:      "Total bytes used by on-disk spill segments.",
			},
		),
	}

	collectors := []prometheus.Collector{
		m.depth,
		m.enqueuedTotal,
		m.dequeuedTotal,
		m.droppedTotal,
		m.ackTotal,
		m.nackTotal,
		m.diskBytes,
	}
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("register queue metric: %w", err)
		}
	}
	return m, nil
}

func (m *queueMetrics) setDepth(tier string, v int64) {
	if m == nil {
		return
	}
	m.depth.WithLabelValues(tier).Set(float64(v))
}

func (m *queueMetrics) addDiskBytes(delta int64) {
	if m == nil {
		return
	}
	m.diskBytes.Add(float64(delta))
}

func (m *queueMetrics) setDiskBytes(v int64) {
	if m == nil {
		return
	}
	m.diskBytes.Set(float64(v))
}
