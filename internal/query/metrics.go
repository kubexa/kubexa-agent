package query

import "github.com/prometheus/client_golang/prometheus"

// unknownResource is the resource label recorded whenever ref.Resource is a
// string the requester chose rather than one the cluster owner or the API
// server vouched for. Two paths qualify.
//
// First, a query that never got past the policy gate. ref.Resource arrives on
// the wire and is attacker-chosen until a policy rule matches it. Feeding it
// unvalidated into a CounterVec and two HistogramVecs lets anyone who can
// reach the stream mint a metric child per bogus string, and Prometheus
// collectors never evict children -- the agent's RSS would climb until the pod
// is OOM-killed, inside the customer's cluster.
//
// Second -- and this is what a rule of resources: ["*"] added -- a query the
// policy ALLOWED but that failed. Matching a wildcard rule proves nothing
// about the string: "aaa1" is a perfectly good DNS-1123 label, so the
// syntactic validation in policy.Decide lets an unbounded family of them
// through, and a loop over them would mint a child each. A wildcard query that
// SUCCEEDED is different: the API server confirmed that resource exists, so
// the label set is bounded by the cluster's own GVR count. A failure names
// nothing real, so it is recorded here instead.
//
// A ref that matched a rule naming it explicitly always keeps its real
// resource: that cardinality is bounded by the owner's own rule set.
const unknownResource = "other"

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
