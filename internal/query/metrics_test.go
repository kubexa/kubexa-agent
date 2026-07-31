package query

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNilRecordersAreSafe(t *testing.T) {
	var r *recorders
	r.enter()
	r.observe("list", "pods", "full", "ok", 0.1, 10)
	r.exit()
}

func TestPolicyDeniedIsCountedUnderItsOwnOutcome(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := newRecorders(reg)
	r.observe("list", "pods", "full", "policy_denied", 0, 0)

	got := testutil.ToFloat64(r.total.WithLabelValues("list", "pods", "full", "policy_denied"))
	if got != 1 {
		t.Fatalf("policy_denied count = %v, want 1", got)
	}
	if other := testutil.ToFloat64(r.total.WithLabelValues("list", "pods", "full", "ok")); other != 0 {
		t.Errorf("ok count = %v, want 0", other)
	}
}
