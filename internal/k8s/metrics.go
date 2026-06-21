package k8s

// MetricsRecorder records Kubernetes API Prometheus metrics.
type MetricsRecorder interface {
	ObserveRequest(method, resource, status string, seconds float64)
	IncError(method, resource, errorType string)
}

type apiMetrics struct {
	rec MetricsRecorder
}

func newAPIMetrics(rec MetricsRecorder) *apiMetrics {
	if rec == nil {
		return nil
	}
	return &apiMetrics{rec: rec}
}

func (m *apiMetrics) observe(method, resource, status string, durationSeconds float64) {
	if m == nil || m.rec == nil {
		return
	}
	m.rec.ObserveRequest(method, resource, status, durationSeconds)
}

func (m *apiMetrics) observeError(method, resource, errorType string) {
	if m == nil || m.rec == nil {
		return
	}
	m.rec.IncError(method, resource, errorType)
}
