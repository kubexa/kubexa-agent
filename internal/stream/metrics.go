package stream

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	metricNamespace = "kubexa"
	metricSubsystem = "grpc"
)

// grpcMetrics holds Prometheus instrumentation for outbound gRPC calls.
type grpcMetrics struct {
	connectionState       prometheus.Gauge
	requestsTotal           *prometheus.CounterVec
	requestDuration         *prometheus.HistogramVec
	streamActive            prometheus.Gauge
	streamMessagesSent      prometheus.Counter
	streamMessagesReceived  prometheus.Counter
	streamErrorsTotal       *prometheus.CounterVec
}

// newGRPCMetrics constructs and registers gRPC metrics on reg.
func newGRPCMetrics(reg prometheus.Registerer) (*grpcMetrics, error) {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	m := &grpcMetrics{
		connectionState: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "connection_state",
			Help:      "Current gRPC connection state (see ConnState enum ordering).",
		}),
		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: metricSubsystem,
				Name:      "requests_total",
				Help:      "Total unary gRPC requests by method and status.",
			},
			[]string{"method", "status"},
		),
		requestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace:   metricNamespace,
				Subsystem:   metricSubsystem,
				Name:        "request_duration_seconds",
				Help:        "Unary gRPC request duration in seconds.",
				Buckets:     prometheus.DefBuckets,
			},
			[]string{"method"},
		),
		streamActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "stream_active",
			Help:      "1 when a bidirectional stream is active, 0 otherwise.",
		}),
		streamMessagesSent: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "stream_messages_sent_total",
			Help:      "Total messages sent on the bidirectional stream.",
		}),
		streamMessagesReceived: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "stream_messages_received_total",
			Help:      "Total messages received on the bidirectional stream.",
		}),
		streamErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: metricSubsystem,
				Name:      "stream_errors_total",
				Help:      "Total bidirectional stream errors by type.",
			},
			[]string{"error_type"},
		),
	}

	collectors := []prometheus.Collector{
		m.connectionState,
		m.requestsTotal,
		m.requestDuration,
		m.streamActive,
		m.streamMessagesSent,
		m.streamMessagesReceived,
		m.streamErrorsTotal,
	}
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("register metric: %w", err)
		}
	}
	return m, nil
}

func (m *grpcMetrics) setConnectionState(s ConnState) {
	if m == nil {
		return
	}
	m.connectionState.Set(stateGaugeValue(s))
}

func (m *grpcMetrics) setStreamActive(active bool) {
	if m == nil {
		return
	}
	if active {
		m.streamActive.Set(1)
	} else {
		m.streamActive.Set(0)
	}
}
