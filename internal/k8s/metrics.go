package k8s

import (
	"github.com/prometheus/client_golang/prometheus"
)

const (
	metricNamespace = "kubexa"
	metricSubsystem = "k8s"
)

// apiMetrics holds Prometheus instrumentation for Kubernetes API calls.
type apiMetrics struct {
	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	errorsTotal     *prometheus.CounterVec
}

// newAPIMetrics constructs and registers Kubernetes API metrics on reg.
func newAPIMetrics(reg prometheus.Registerer) (*apiMetrics, error) {
	m := &apiMetrics{
		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: metricSubsystem,
				Name:      "api_requests_total",
				Help:      "Total number of Kubernetes API requests.",
			},
			[]string{"method", "resource", "status"},
		),
		requestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: metricNamespace,
				Subsystem: metricSubsystem,
				Name:      "api_request_duration_seconds",
				Help:      "Kubernetes API request duration in seconds.",
				Buckets:   prometheus.DefBuckets,
			},
			[]string{"method", "resource"},
		),
		errorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: metricSubsystem,
				Name:      "api_errors_total",
				Help:      "Total number of Kubernetes API errors.",
			},
			[]string{"method", "resource", "error_type"},
		),
	}

	collectors := []prometheus.Collector{
		m.requestsTotal,
		m.requestDuration,
		m.errorsTotal,
	}
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *apiMetrics) observe(method, resource, status string, durationSeconds float64) {
	if m == nil {
		return
	}
	m.requestsTotal.WithLabelValues(method, resource, status).Inc()
	m.requestDuration.WithLabelValues(method, resource).Observe(durationSeconds)
}

func (m *apiMetrics) observeError(method, resource, errorType string) {
	if m == nil {
		return
	}
	m.errorsTotal.WithLabelValues(method, resource, errorType).Inc()
}
