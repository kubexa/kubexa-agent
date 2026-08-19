package stream

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func reconnectCount(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	g, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range g {
		if mf.GetName() != "kubexa_connection_reconnects_total" {
			continue
		}
		for _, metric := range mf.GetMetric() {
			return metric.GetCounter().GetValue()
		}
		t.Fatal("connection_reconnects_total has no sample")
	}
	t.Fatal("connection_reconnects_total metric not found")
	return 0
}

// The counter is named "reconnects", so the first dial of a process is not one.
// Every later entry into StateConnecting is: the run loop only re-reaches the
// top after a session was lost, which is exactly what an operator wants counted.
func TestReconnectCounterCountsRedialsOnly(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	q := newTestQueue(t)
	_, lis := startBufGRPCServer(t, &mockGateway{})
	sm, reg := newTestManager(t, cfg, q, lis)

	sm.transition(StateConnecting, "dialing gateway", nil)
	if got := reconnectCount(t, reg); got != 0 {
		t.Fatalf("first dial counted as a reconnect: got %v, want 0", got)
	}

	sm.transition(StateTransientFailure, "session lost", nil)
	sm.transition(StateConnecting, "dialing gateway", nil)
	if got := reconnectCount(t, reg); got != 1 {
		t.Fatalf("redial after a lost session not counted: got %v, want 1", got)
	}

	sm.transition(StateTransientFailure, "session lost", nil)
	sm.transition(StateConnecting, "dialing gateway", nil)
	if got := reconnectCount(t, reg); got != 2 {
		t.Fatalf("second redial not counted: got %v, want 2", got)
	}
}

// A shutdown must not be reported as a reconnect, and the loop must not be able
// to leave shutdown: transition() refuses that edge, so the counter stays put.
func TestShutdownIsNotAReconnect(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	q := newTestQueue(t)
	_, lis := startBufGRPCServer(t, &mockGateway{})
	sm, reg := newTestManager(t, cfg, q, lis)

	sm.transition(StateShutdown, "context cancelled", nil)
	sm.transition(StateConnecting, "dialing gateway", nil)
	if got := reconnectCount(t, reg); got != 0 {
		t.Fatalf("a refused transition out of shutdown was counted: got %v, want 0", got)
	}
}
