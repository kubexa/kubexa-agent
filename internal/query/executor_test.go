package query

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/query/policy"
	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

var podsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}

func pod(ns, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":          name,
			"namespace":     ns,
			"managedFields": []any{map[string]any{"manager": "kubectl"}},
		},
		"status": map[string]any{"phase": "Running"},
	}}
}

func newFakeDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		podsGVR: "PodList",
		{Group: "", Version: "v1", Resource: "secrets"}: "SecretList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, objs...)
}

func newExecutor(t *testing.T, cfgYAML string, dyn dynamic.Interface) *Executor {
	t.Helper()
	var cfg pkgconfig.Config
	if err := yaml.Unmarshal([]byte(cfgYAML), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	p, err := policy.Compile(&cfg)
	if err != nil {
		t.Fatalf("compile policy: %v", err)
	}
	e, err := New(Options{
		Clients: k8s.QueryClients{Dynamic: dyn},
		Policy:  p,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

const allowPodsInStage = `
query:
  rules:
    - namespace: stage
      resources: [pods]
`

func listQuery(ns string) *agentv1.ResourceQuery {
	return &agentv1.ResourceQuery{
		QueryId:   "q1",
		Ref:       &agentv1.ResourceRef{Group: "", Version: "v1", Resource: "pods"},
		Verb:      agentv1.QueryVerb_QUERY_VERB_LIST,
		View:      agentv1.QueryView_QUERY_VIEW_FULL,
		Namespace: ns,
	}
}

// TestPolicyDenialNeverReachesTheAPIServer is the load-bearing security test.
// The design's central promise is that a refused query does not touch the
// customer's control plane. Asserting the error code alone would still pass if
// the executor called the API server and then discarded the result, so this
// asserts the fake client recorded zero actions.
func TestPolicyDenialNeverReachesTheAPIServer(t *testing.T) {
	dyn := newFakeDynamic(pod("prod", "p1"))
	e := newExecutor(t, allowPodsInStage, dyn)

	res := e.Execute(context.Background(), listQuery("prod"))

	if res.GetError().GetCode() != agentv1.QueryErrorCode_QUERY_ERROR_POLICY_DENIED {
		t.Fatalf("code = %v, want POLICY_DENIED", res.GetError().GetCode())
	}
	if got := dyn.Actions(); len(got) != 0 {
		t.Fatalf("policy-denied query issued %d API calls, want 0: %+v", len(got), got)
	}
	if res.GetQueryId() != "q1" {
		t.Errorf("query_id = %q, want it echoed back as q1", res.GetQueryId())
	}
	if len(res.GetPayload()) != 0 {
		t.Error("a denied query must carry no payload")
	}
}

func TestListReturnsAllowedObjects(t *testing.T) {
	dyn := newFakeDynamic(pod("stage", "be-1"), pod("stage", "be-2"))
	e := newExecutor(t, allowPodsInStage, dyn)

	res := e.Execute(context.Background(), listQuery("stage"))
	if res.GetError() != nil {
		t.Fatalf("error = %+v, want nil", res.GetError())
	}
	var items []map[string]any
	if err := json.Unmarshal(res.GetPayload(), &items); err != nil {
		t.Fatalf("payload is not a JSON array: %v (%s)", err, res.GetPayload())
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
}

func TestListStripsManagedFields(t *testing.T) {
	dyn := newFakeDynamic(pod("stage", "be-1"))
	e := newExecutor(t, allowPodsInStage, dyn)

	res := e.Execute(context.Background(), listQuery("stage"))
	if strings.Contains(string(res.GetPayload()), "managedFields") {
		t.Fatalf("managedFields must always be stripped, got %s", res.GetPayload())
	}
}

func TestListFiltersRowsByNamePattern(t *testing.T) {
	dyn := newFakeDynamic(pod("stage", "be-1"), pod("stage", "fe-1"))
	e := newExecutor(t, `
query:
  rules:
    - namespace: stage
      resources: [pods]
      names: ["be-*"]
`, dyn)

	res := e.Execute(context.Background(), listQuery("stage"))
	body := string(res.GetPayload())
	if !strings.Contains(body, "be-1") {
		t.Error("be-1 must be present")
	}
	if strings.Contains(body, "fe-1") {
		t.Error("fe-1 must be filtered out by the name pattern")
	}
}

func TestGetDeniedWhenVerbsAreListOnly(t *testing.T) {
	dyn := newFakeDynamic(pod("stage", "be-1"))
	e := newExecutor(t, `
query:
  rules:
    - namespace: stage
      resources: [pods]
      verbs: [list]
`, dyn)

	res := e.Execute(context.Background(), &agentv1.ResourceQuery{
		QueryId:   "q2",
		Ref:       &agentv1.ResourceRef{Group: "", Version: "v1", Resource: "pods"},
		Verb:      agentv1.QueryVerb_QUERY_VERB_GET,
		Namespace: "stage",
		Name:      "be-1",
	})
	if res.GetError().GetCode() != agentv1.QueryErrorCode_QUERY_ERROR_POLICY_DENIED {
		t.Fatalf("code = %v, want POLICY_DENIED", res.GetError().GetCode())
	}
	if len(dyn.Actions()) != 0 {
		t.Fatal("a denied GET must not reach the API server")
	}
}

func TestGetReturnsTheObject(t *testing.T) {
	dyn := newFakeDynamic(pod("stage", "be-1"))
	e := newExecutor(t, allowPodsInStage, dyn)

	res := e.Execute(context.Background(), &agentv1.ResourceQuery{
		QueryId:   "q3",
		Ref:       &agentv1.ResourceRef{Group: "", Version: "v1", Resource: "pods"},
		Verb:      agentv1.QueryVerb_QUERY_VERB_GET,
		Namespace: "stage",
		Name:      "be-1",
	})
	if res.GetError() != nil {
		t.Fatalf("error = %+v, want nil", res.GetError())
	}
	var obj map[string]any
	if err := json.Unmarshal(res.GetPayload(), &obj); err != nil {
		t.Fatalf("payload is not a JSON object: %v", err)
	}
	meta, _ := obj["metadata"].(map[string]any)
	if meta["name"] != "be-1" {
		t.Errorf("name = %v, want be-1", meta["name"])
	}
}

func TestGetMissingObjectMapsToNotFound(t *testing.T) {
	dyn := newFakeDynamic()
	e := newExecutor(t, allowPodsInStage, dyn)

	res := e.Execute(context.Background(), &agentv1.ResourceQuery{
		QueryId:   "q4",
		Ref:       &agentv1.ResourceRef{Group: "", Version: "v1", Resource: "pods"},
		Verb:      agentv1.QueryVerb_QUERY_VERB_GET,
		Namespace: "stage",
		Name:      "nope",
	})
	if res.GetError().GetCode() != agentv1.QueryErrorCode_QUERY_ERROR_NOT_FOUND {
		t.Fatalf("code = %v, want NOT_FOUND", res.GetError().GetCode())
	}
}

func TestForbiddenMapsToRBACDeniedNotPolicyDenied(t *testing.T) {
	dyn := newFakeDynamic()
	dyn.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrorsForbidden()
	})
	e := newExecutor(t, allowPodsInStage, dyn)

	res := e.Execute(context.Background(), listQuery("stage"))
	if got := res.GetError().GetCode(); got != agentv1.QueryErrorCode_QUERY_ERROR_RBAC_DENIED {
		t.Fatalf("code = %v, want RBAC_DENIED -- an API-server 403 is a ClusterRole problem, "+
			"not a config problem, and the operator is sent to the wrong file if these merge", got)
	}
}

func TestGetRequiresAName(t *testing.T) {
	dyn := newFakeDynamic()
	e := newExecutor(t, allowPodsInStage, dyn)
	res := e.Execute(context.Background(), &agentv1.ResourceQuery{
		QueryId:   "q5",
		Ref:       &agentv1.ResourceRef{Group: "", Version: "v1", Resource: "pods"},
		Verb:      agentv1.QueryVerb_QUERY_VERB_GET,
		Namespace: "stage",
	})
	if res.GetError().GetCode() != agentv1.QueryErrorCode_QUERY_ERROR_INTERNAL {
		t.Fatalf("code = %v, want INTERNAL for a GET with no name", res.GetError().GetCode())
	}
	if len(dyn.Actions()) != 0 {
		t.Fatal("a malformed GET must not reach the API server")
	}
}

func TestPolicySelectorIsANDedWithTheRequest(t *testing.T) {
	dyn := newFakeDynamic()
	var seen string
	dyn.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		seen = a.(k8stesting.ListActionImpl).ListRestrictions.Labels.String()
		return true, &unstructured.UnstructuredList{Object: map[string]any{
			"apiVersion": "v1", "kind": "PodList",
		}}, nil
	})
	e := newExecutor(t, `
query:
  rules:
    - namespace: stage
      resources: [pods]
      label_selector: tier=backend
`, dyn)

	q := listQuery("stage")
	q.LabelSelector = "app=be"
	e.Execute(context.Background(), q)

	if !strings.Contains(seen, "tier=backend") {
		t.Errorf("selector %q must include the policy's tier=backend", seen)
	}
	if !strings.Contains(seen, "app=be") {
		t.Errorf("selector %q must include the request's app=be", seen)
	}
}

func TestTimeoutIsClamped(t *testing.T) {
	tests := []struct {
		name string
		ms   int32
		want time.Duration
	}{
		{"zero uses the default", 0, 10 * time.Second},
		{"below the floor clamps up", 100, time.Second},
		{"above the ceiling clamps down", 120000, 30 * time.Second},
		{"in range is honoured", 5000, 5 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampTimeout(tc.ms); got != tc.want {
				t.Errorf("clampTimeout(%d) = %v, want %v", tc.ms, got, tc.want)
			}
		})
	}
}

// TestListClampsTheRequestedLimit uses a real dynamic client against an
// httptest server rather than the fake one: k8stesting.ListRestrictions
// records only the selectors, so the fake client cannot observe the page size
// -- and the page size is the entire point of this test.
func TestListClampsTheRequestedLimit(t *testing.T) {
	tests := []struct {
		name string
		ask  int32
		want string
	}{
		{"unset falls back to the default", 0, strconv.Itoa(int(defaultLimit))},
		{"a sane page size is honoured", 50, "50"},
		{"an absurd page size is clamped", 2_000_000, strconv.Itoa(int(maxLimit))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got http.Request
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = *r.Clone(r.Context())
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PodList","items":[]}`))
			}))
			t.Cleanup(srv.Close)
			dyn, err := dynamic.NewForConfig(&rest.Config{Host: srv.URL})
			if err != nil {
				t.Fatalf("dynamic client: %v", err)
			}
			e := newExecutor(t, allowPodsInStage, dyn)

			q := listQuery("stage")
			q.Limit = tc.ask
			if res := e.Execute(context.Background(), q); res.GetError() != nil {
				t.Fatalf("error = %+v, want nil", res.GetError())
			}

			if seen := got.URL.Query().Get("limit"); seen != tc.want {
				t.Errorf("limit = %q, want %q -- an unclamped page size has the agent "+
					"decode and buffer the whole cluster before maxBytes is consulted",
					seen, tc.want)
			}
		})
	}
}

func apierrorsForbidden() error {
	return apierrors.NewForbidden(
		schema.GroupResource{Resource: "pods"}, "",
		errors.New("forbidden by RBAC"))
}
