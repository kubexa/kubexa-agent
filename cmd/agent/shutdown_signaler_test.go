package main

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/internal/metrics"
	"github.com/kubexa/kubexa-agent/internal/queue"
	"github.com/kubexa/kubexa-agent/internal/stream"
	"github.com/kubexa/kubexa-agent/pkg/config"
)

// TestStreamManagerSatisfiesShutdownSignaler pins the invariant serve()
// depends on but cannot check for itself at compile time: that the concrete
// value stream.New returns -- behind the narrow stream.Manager interface --
// still implements shutdownSignaler. serve() turns a failed assertion into a
// boot error, mirroring TestStreamManagerSatisfiesEffectiveConfigSetter,
// because nothing else would catch a silent regression here (a future
// decorator or second Manager implementation) before it reached a customer's
// cluster: a gateway Shutdown would arrive, log, and do nothing.
func TestStreamManagerSatisfiesShutdownSignaler(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.TenantToken = "tenant-secret"
	cfg.Agent.AgentID = "agent-1"
	cfg.Agent.ClusterID = "cluster-1"
	cfg.Gateway.Address = "127.0.0.1:0"

	reg := prometheus.NewRegistry()
	m, err := metrics.New(reg, "test", cfg.Agent.ClusterID, cfg.Agent.AgentID)
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}

	q, err := queue.New(&cfg.Buffer, logger.New("queue-test"), m.Queue())
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })

	streamMgr, err := stream.New(
		cfg,
		q,
		logger.New("stream-test"),
		m.Stream(),
		m.Connection(),
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("stream.New: %v", err)
	}

	if _, ok := streamMgr.(shutdownSignaler); !ok {
		t.Fatalf("stream.New's result (%T) no longer implements shutdownSignaler; "+
			"a gateway Shutdown would silently never restart the agent", streamMgr)
	}
}
