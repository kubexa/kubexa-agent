package mutate

import "github.com/prometheus/client_golang/prometheus"

// unknownResource is the resource label recorded whenever ref.Resource has
// not been vouched for by the owner's own configuration.
//
// This is narrower than the read path's reasoning (see internal/query's
// unknownResource comment), because the mutate policy supports no wildcard
// rule at all -- config.ValidateMutateRules refuses every "*" form, bare or
// partial, so compiledRule.matchesResource only ever matches a ref by exact
// equality against a Ref the operator typed. That makes an ALLOWED decision's
// ref.Resource safe to use as a label unconditionally: the set of values it
// can take is bounded by the owner's own rule set, the same finite family
// every mutation metric already carries elsewhere.
//
// A DENIED decision is a different story: ref.Resource never matched any
// rule, so it is still exactly what arrived on the wire -- attacker/gateway
// chosen, bounded only by "is a valid DNS-1123 label" (policy.Decide's
// validateRef). Feeding that into a CounterVec/HistogramVec would let anyone
// who can reach the stream mint a metric child per bogus string, and
// Prometheus collectors never evict children. So the denied path (and any
// other path that returns before a rule match is established) uses this
// placeholder instead, exactly as the read path does for its own
// wire-controlled failure cases.
const unknownResource = "other"

// recorders holds the mutate path's Prometheus instruments. All are
// optional: a nil recorders is valid and every method is a no-op, so tests
// and the dev path need no registry.
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
			Name: "kubexa_agent_mutation_total",
			Help: "Cluster mutations by verb, resource and outcome.",
		}, []string{"verb", "resource", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kubexa_agent_mutation_duration_seconds",
			Help:    "Cluster mutation latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"verb", "resource"}),
		bytes: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kubexa_agent_mutation_response_bytes",
			Help:    "Cluster mutation response size.",
			Buckets: prometheus.ExponentialBuckets(1024, 4, 8),
		}, []string{"verb", "resource"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kubexa_agent_mutation_inflight",
			Help: "Cluster mutations currently executing.",
		}),
	}
	reg.MustRegister(r.total, r.duration, r.bytes, r.inflight)
	return r
}

func (r *recorders) observe(verb, resource, outcome string, seconds float64, size int) {
	if r == nil {
		return
	}
	r.total.WithLabelValues(verb, resource, outcome).Inc()
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
