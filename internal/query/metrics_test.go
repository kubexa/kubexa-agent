package query

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

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

// A wildcard rule (resources: ["*"]) reopened the cardinality hole the test
// above closes: these queries are ALLOWED, so they take the success path's
// label. Nothing about matching a wildcard vouches for the string -- "aaa1" is
// a valid DNS-1123 label, so policy.Decide's syntactic validation passes an
// unbounded family of them -- and every distinct label value is a permanent
// allocation in a collector that never evicts children.
func TestWildcardAllowedFailureDoesNotMintAMetricChildPerResource(t *testing.T) {
	junk := []string{"aaa1", "aaa2", "aaa3"}
	listKinds := map[schema.GroupVersionResource]string{podsGVR: "PodList"}
	for _, name := range junk {
		listKinds[schema.GroupVersionResource{Version: "v1", Resource: name}] = "JunkList"
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	// The API server's answer for a resource that does not exist.
	dyn.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Resource == "pods" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewNotFound(
			schema.GroupResource{Resource: a.GetResource().Resource}, "")
	})

	reg := prometheus.NewRegistry()
	e := newExecutor(t, `
query:
  rules:
    - id: everything
      resources: ["*"]
`, dyn)
	e.metrics = newRecorders(reg)

	for _, name := range junk {
		q := listQuery("stage")
		q.Ref = &agentv1.ResourceRef{Version: "v1", Resource: name}
		if got := e.Execute(context.Background(), q).GetError().GetCode(); got != agentv1.QueryErrorCode_QUERY_ERROR_NOT_FOUND {
			t.Fatalf("code for %q = %v, want NOT_FOUND", name, got)
		}
	}
	if got := testutil.CollectAndCount(e.metrics.total); got != 1 {
		t.Errorf("kubexa_agent_query_total has %d children after 3 nonexistent resources, want 1: "+
			"a wildcard-allowed FAILURE names nothing the API server confirmed exists", got)
	}
	if got := testutil.ToFloat64(
		e.metrics.total.WithLabelValues("list", unknownResource, "full", "not_found")); got != 3 {
		t.Errorf("%q failure count = %v, want 3", unknownResource, got)
	}

	// A wildcard query that SUCCEEDED names a resource the API server
	// confirmed exists, so its cardinality is bounded by the cluster's own GVR
	// count -- it keeps its real label, which is what makes the metric useful.
	res := e.Execute(context.Background(), listQuery("stage"))
	if res.GetError() != nil {
		t.Fatalf("pods list failed: %v", res.GetError())
	}
	if got := testutil.ToFloat64(
		e.metrics.total.WithLabelValues("list", "pods", "full", "ok")); got != 1 {
		t.Errorf("pods ok count = %v, want 1: a successful wildcard query must keep its "+
			"real resource label", got)
	}
}
