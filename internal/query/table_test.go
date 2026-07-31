package query

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/query/policy"
	pkgconfig "github.com/kubexa/kubexa-agent/pkg/config"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

const tableJSON = `{"kind":"Table","apiVersion":"meta.k8s.io/v1",` +
	`"columnDefinitions":[{"name":"Name","type":"string"}],` +
	`"rows":[{"cells":["be-1"]}]}`

// tableServer stands in for the API server and records what the agent asked
// for, so the test can assert the content negotiation rather than trusting it.
func tableServer(t *testing.T, capture *http.Request) (*httptest.Server, *k8s.QueryClients) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*capture = *r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tableJSON))
	}))
	t.Cleanup(srv.Close)

	restFor := func(gv schema.GroupVersion) (rest.Interface, error) {
		// GroupVersion lives on rest.Config's embedded ContentConfig, not on
		// Config itself -- a bare `GroupVersion: &gv` field in this literal
		// does not compile against client-go v0.36.1.
		cfg := &rest.Config{Host: srv.URL, ContentConfig: rest.ContentConfig{GroupVersion: &gv}}
		if gv.Group == "" {
			cfg.APIPath = "/api"
		} else {
			cfg.APIPath = "/apis"
		}
		cfg.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
		return rest.RESTClientFor(cfg)
	}
	return srv, &k8s.QueryClients{Dynamic: newFakeDynamic(), RESTFor: restFor}
}

func tableExecutor(t *testing.T, clients *k8s.QueryClients) *Executor {
	t.Helper()
	var cfg pkgconfig.Config
	if err := yaml.Unmarshal([]byte(allowPodsInStage), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p, err := policy.Compile(&cfg)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	e, err := New(Options{Clients: *clients, Policy: p})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func tableQuery() *agentv1.ResourceQuery {
	return &agentv1.ResourceQuery{
		QueryId:   "t1",
		Ref:       &agentv1.ResourceRef{Group: "", Version: "v1", Resource: "pods"},
		Verb:      agentv1.QueryVerb_QUERY_VERB_LIST,
		View:      agentv1.QueryView_QUERY_VIEW_TABLE,
		Namespace: "stage",
	}
}

func TestTableViewNegotiatesServerSidePrinting(t *testing.T) {
	var got http.Request
	_, clients := tableServer(t, &got)
	e := tableExecutor(t, clients)

	res := e.Execute(context.Background(), tableQuery())
	if res.GetError() != nil {
		t.Fatalf("error = %+v, want nil", res.GetError())
	}

	accept := got.Header.Get("Accept")
	if !strings.Contains(accept, "as=Table") {
		t.Errorf("Accept = %q, want it to request as=Table -- without this the API "+
			"server returns full objects and the whole payload saving is lost", accept)
	}
	if !strings.Contains(accept, "g=meta.k8s.io") || !strings.Contains(accept, "v=v1") {
		t.Errorf("Accept = %q, want the meta.k8s.io/v1 group-version", accept)
	}
}

func TestTableViewTargetsTheNamespacedPath(t *testing.T) {
	var got http.Request
	_, clients := tableServer(t, &got)
	e := tableExecutor(t, clients)

	e.Execute(context.Background(), tableQuery())

	if want := "/api/v1/namespaces/stage/pods"; got.URL.Path != want {
		t.Errorf("path = %q, want %q", got.URL.Path, want)
	}
}

func TestTableViewReturnsTheServerTableVerbatim(t *testing.T) {
	var got http.Request
	_, clients := tableServer(t, &got)
	e := tableExecutor(t, clients)

	res := e.Execute(context.Background(), tableQuery())
	if string(res.GetPayload()) != tableJSON {
		t.Errorf("payload = %s, want the server's Table unmodified", res.GetPayload())
	}
}

func TestTableViewIsStillPolicyGated(t *testing.T) {
	var got http.Request
	srv, clients := tableServer(t, &got)
	e := tableExecutor(t, clients)

	q := tableQuery()
	q.Namespace = "prod" // outside the policy
	res := e.Execute(context.Background(), q)

	if res.GetError().GetCode() != agentv1.QueryErrorCode_QUERY_ERROR_POLICY_DENIED {
		t.Fatalf("code = %v, want POLICY_DENIED", res.GetError().GetCode())
	}
	if got.URL != nil {
		t.Fatalf("a denied table query reached %s; it must not leave the agent", srv.URL)
	}
}
