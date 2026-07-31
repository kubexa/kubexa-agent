package query

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
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

// TestDeniedQueryDoesNotMintAMetricChildPerBogusResource guards the label
// cardinality of the one path that sees an unvalidated resource string. Every
// distinct label value is a permanent allocation in the collector, so a caller
// able to choose it can grow the agent's RSS without bound inside a customer's
// cluster.
func TestDeniedQueryDoesNotMintAMetricChildPerBogusResource(t *testing.T) {
	reg := prometheus.NewRegistry()
	dyn := newFakeDynamic()
	e := newExecutor(t, allowPodsInStage, dyn)
	e.metrics = newRecorders(reg)

	for _, junk := range []string{"aaaa", "bbbb", "cccc"} {
		q := listQuery("stage")
		q.Ref = &agentv1.ResourceRef{Group: "", Version: "v1", Resource: junk}
		res := e.Execute(context.Background(), q)
		if res.GetError().GetCode() != agentv1.QueryErrorCode_QUERY_ERROR_POLICY_DENIED {
			t.Fatalf("code = %v, want POLICY_DENIED for %q", res.GetError().GetCode(), junk)
		}
	}

	if got := testutil.CollectAndCount(e.metrics.total); got != 1 {
		t.Errorf("kubexa_agent_query_total has %d children after 3 bogus resources, want 1: "+
			"the denied path must not label the series with an unvalidated resource", got)
	}
	got := testutil.ToFloat64(
		e.metrics.total.WithLabelValues("list", unknownResource, "full", "policy_denied"))
	if got != 3 {
		t.Errorf("%q denial count = %v, want 3", unknownResource, got)
	}
}
