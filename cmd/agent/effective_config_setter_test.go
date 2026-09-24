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

// TestStreamManagerSatisfiesEffectiveConfigSetter pins the invariant serve()
// depends on but cannot check for itself at compile time: that the concrete
// value stream.New returns -- behind the narrow stream.Manager interface --
// still implements effectiveConfigSetter. serve() turns a failed assertion
// into a boot error specifically because nothing else would ever catch a
// silent regression here (a future decorator or second Manager
// implementation); this test is what catches that drift in CI, against the
// real constructor, instead of on a customer's cluster at boot.
//
// It does not call streamMgr.Run -- constructing the manager is enough to
// exercise the assertion serve() performs. Building a full manager (rather
// than asserting the interface some other way) is deliberate: the risk named
// in the review this test was written for is stream.New itself changing what
// it returns, so the test must go through stream.New, not around it.
func TestStreamManagerSatisfiesEffectiveConfigSetter(t *testing.T) {
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

	if _, ok := streamMgr.(effectiveConfigSetter); !ok {
		t.Fatalf("stream.New's result (%T) no longer implements effectiveConfigSetter; "+
			"serve()'s live query policy swap would silently never be wired", streamMgr)
	}
}
