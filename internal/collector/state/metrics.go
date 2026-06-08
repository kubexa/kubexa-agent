package state

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const metricNamespace = "agent"

// metrics holds Prometheus instrumentation for the state watcher.
type metrics struct {
	eventsTotal      *prometheus.CounterVec
	droppedTotal     *prometheus.CounterVec
	queueDepth       prometheus.Gauge
	cacheSynced      *prometheus.GaugeVec
	processingHist   prometheus.Histogram
}

func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	m := &metrics{
		eventsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Name:      "state_watcher_events_total",
				Help:      "Total Kubernetes state events processed by the state watcher.",
			},
			[]string{"resource_type", "event_type"},
		),
		droppedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Name:      "state_watcher_dropped_total",
				Help:      "Total Kubernetes state events dropped by the state watcher.",
			},
			[]string{"resource_type", "reason"},
		),
		queueDepth: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricNamespace,
				Name:      "state_watcher_queue_depth",
				Help:      "Current depth of the internal state watcher work queue.",
			},
		),
		cacheSynced: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: metricNamespace,
				Name:      "state_watcher_cache_synced",
				Help:      "Informer cache sync status per resource (1=synced, 0=not synced).",
			},
			[]string{"resource"},
		),
		processingHist: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Namespace: metricNamespace,
				Name:      "state_watcher_processing_duration_seconds",
				Help:      "Time spent processing a state event from the work queue.",
				Buckets:   prometheus.DefBuckets,
			},
		),
	}

	collectors := []prometheus.Collector{
		m.eventsTotal,
		m.droppedTotal,
		m.queueDepth,
		m.cacheSynced,
		m.processingHist,
	}
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("register state watcher metric: %w", err)
		}
	}
	return m, nil
}

func (m *metrics) incEvent(resourceType, eventType string) {
	if m == nil {
		return
	}
	m.eventsTotal.WithLabelValues(resourceType, eventType).Inc()
}

func (m *metrics) incDropped(resourceType, reason string) {
	if m == nil {
		return
	}
	m.droppedTotal.WithLabelValues(resourceType, reason).Inc()
}

func (m *metrics) setQueueDepth(depth int) {
	if m == nil {
		return
	}
	m.queueDepth.Set(float64(depth))
}

func (m *metrics) setCacheSynced(resource string, synced bool) {
	if m == nil {
		return
	}
	v := 0.0
	if synced {
		v = 1
	}
	m.cacheSynced.WithLabelValues(resource).Set(v)
}

func (m *metrics) observeProcessing(d time.Duration) {
	if m == nil {
		return
	}
	m.processingHist.Observe(d.Seconds())
}
