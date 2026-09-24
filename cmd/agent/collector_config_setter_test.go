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

// TestStreamManagerSatisfiesCollectorConfigSetter is the third of the
// structural assertions serve() makes and cannot check at compile time --
// see TestStreamManagerSatisfiesEffectiveConfigSetter for the full reasoning
// this mirrors.
//
// What is lost if it silently stopped holding: the heartbeat would report
// every revision as applied and restart_required as false forever, because
// the manager would never learn which config the collectors were actually
// built from. That is precisely the "control that changes nothing, reported
// as working" this task exists to close, so serve() treats a failed
// assertion as a boot error and this test catches the drift in CI.
func TestStreamManagerSatisfiesCollectorConfigSetter(t *testing.T) {
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

	if _, ok := streamMgr.(collectorConfigSetter); !ok {
		t.Fatalf("stream.New's result (%T) no longer implements collectorConfigSetter; "+
			"the heartbeat could never report restart_required truthfully", streamMgr)
	}
	// serve() waits on the same value for the first managed set before it
	// builds the collectors; a Manager that stopped answering this would
	// leave the collectors on the file config again.
	if _, ok := streamMgr.(effectiveConfigSource); !ok {
		t.Fatalf("stream.New's result (%T) no longer implements effectiveConfigSource; "+
			"the collectors would be built from the file config again", streamMgr)
	}
}
