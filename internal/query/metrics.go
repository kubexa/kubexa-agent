package query

import "github.com/prometheus/client_golang/prometheus"

// recorders holds the query path's Prometheus instruments. All are optional:
// a nil recorders is valid and every method is a no-op, so tests and the
// dev path need no registry.
type recorders struct {
	total    *prometheus.CounterVec
	duration *prometheus.HistogramVec
	bytes    *prometheus.HistogramVec
	inflight prometheus.Gauge
}

func newRecorders(reg prometheus.Registerer) *recorders {
	if reg == nil {
		return nil
	}
	r := &recorders{
		total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kubexa_agent_query_total",
			Help: "Live resource queries by verb, resource, view and outcome.",
		}, []string{"verb", "resource", "view", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kubexa_agent_query_duration_seconds",
			Help:    "Live resource query latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"verb", "resource"}),
		bytes: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kubexa_agent_query_response_bytes",
			Help:    "Live resource query response size.",
			Buckets: prometheus.ExponentialBuckets(1024, 4, 8),
		}, []string{"verb", "resource"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kubexa_agent_query_inflight",
			Help: "Live resource queries currently executing.",
		}),
	}
	reg.MustRegister(r.total, r.duration, r.bytes, r.inflight)
	return r
}

func (r *recorders) observe(verb, resource, view, outcome string, seconds float64, size int) {
	if r == nil {
		return
	}
	r.total.WithLabelValues(verb, resource, view, outcome).Inc()
	r.duration.WithLabelValues(verb, resource).Observe(seconds)
	r.bytes.WithLabelValues(verb, resource).Observe(float64(size))
}

func (r *recorders) enter() {
	if r != nil {
		r.inflight.Inc()
	}
}

func (r *recorders) exit() {
	if r != nil {
		r.inflight.Dec()
	}
}
