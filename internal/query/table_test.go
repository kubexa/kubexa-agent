package query

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	return tableServerBody(t, capture, tableJSON)
}

// tableServerBody is tableServer with a caller-chosen response body, so a test
// can hand the executor the rows it needs to assert filtering and sanitization.
func tableServerBody(t *testing.T, capture *http.Request, body string) (*httptest.Server, *k8s.QueryClients) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*capture = *r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
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
	return tableExecutorFor(t, clients, allowPodsInStage)
}

func tableExecutorFor(t *testing.T, clients *k8s.QueryClients, cfgYAML string) *Executor {
	t.Helper()
	var cfg pkgconfig.Config
	if err := yaml.Unmarshal([]byte(cfgYAML), &cfg); err != nil {
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

// TestTableViewPreservesTheServerRendering pins what survives the decode and
// re-encode the name filter and sanitization require. Byte equality with the
// server's body is deliberately NOT asserted -- re-marshalling a metav1.Table
// writes back fields the server elided -- but the kind, the column
// definitions and the printed cells are the whole value of server-side
// printing and must come through untouched.
func TestTableViewPreservesTheServerRendering(t *testing.T) {
	var got http.Request
	_, clients := tableServer(t, &got)
	e := tableExecutor(t, clients)

	res := e.Execute(context.Background(), tableQuery())
	if res.GetError() != nil {
		t.Fatalf("error = %+v, want nil", res.GetError())
	}
	var table metav1.Table
	if err := json.Unmarshal(res.GetPayload(), &table); err != nil {
		t.Fatalf("payload is not a Table: %v (%s)", err, res.GetPayload())
	}
	if table.Kind != "Table" || table.APIVersion != "meta.k8s.io/v1" {
		t.Errorf("kind/apiVersion = %s/%s, want Table/meta.k8s.io/v1", table.Kind, table.APIVersion)
	}
	if len(table.ColumnDefinitions) != 1 || table.ColumnDefinitions[0].Name != "Name" {
		t.Errorf("column definitions = %+v, want the server's single Name column",
			table.ColumnDefinitions)
	}
	if len(table.Rows) != 1 || len(table.Rows[0].Cells) != 1 || table.Rows[0].Cells[0] != "be-1" {
		t.Errorf("rows = %+v, want the server's printed cells unchanged", table.Rows)
	}
}

// TestTableViewFailsClosedOnANonTableBody covers content negotiation being a
// request rather than a guarantee: an API server with no table converter
// answers with the ordinary list of whole objects. Forwarding that would skip
// both the name filter and sanitization, so it is refused.
func TestTableViewFailsClosedOnANonTableBody(t *testing.T) {
	var got http.Request
	_, clients := tableServerBody(t, &got,
		`{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"fe-1"}}]}`)
	e := tableExecutor(t, clients)

	res := e.Execute(context.Background(), tableQuery())
	if res.GetError().GetCode() != agentv1.QueryErrorCode_QUERY_ERROR_INTERNAL {
		t.Fatalf("code = %v, want INTERNAL for a body that is not a Table", res.GetError().GetCode())
	}
	if len(res.GetPayload()) != 0 {
		t.Errorf("unfiltered, unsanitized objects were forwarded: %s", res.GetPayload())
	}
}

// TestTableViewDropsUnverifiableRowsWhenNamesAreConstrained is the fail-closed
// half of the name filter: a row the agent cannot read a name from cannot be
// checked against the rule that authorized the query, so it must not be sent.
func TestTableViewDropsUnverifiableRowsWhenNamesAreConstrained(t *testing.T) {
	var got http.Request
	_, clients := tableServerBody(t, &got,
		`{"kind":"Table","apiVersion":"meta.k8s.io/v1",`+
			`"columnDefinitions":[{"name":"Name","type":"string"}],`+
			`"rows":[{"cells":["mystery"]}]}`)
	e := tableExecutorFor(t, clients, `
query:
  rules:
    - namespace: stage
      resources: [pods]
      names: ["be-*"]
`)

	res := e.Execute(context.Background(), tableQuery())
	if res.GetError() != nil {
		t.Fatalf("error = %+v, want nil", res.GetError())
	}
	var table metav1.Table
	if err := json.Unmarshal(res.GetPayload(), &table); err != nil {
		t.Fatalf("payload is not a Table: %v", err)
	}
	if len(table.Rows) != 0 {
		t.Errorf("rows = %+v, want none: a row with no object carries no name to check "+
			"against names: [\"be-*\"]", table.Rows)
	}
}

func TestTableViewClampsTheRequestedLimit(t *testing.T) {
	var got http.Request
	_, clients := tableServer(t, &got)
	e := tableExecutor(t, clients)

	q := tableQuery()
	q.Limit = 2_000_000
	e.Execute(context.Background(), q)

	if want := strconv.Itoa(int(maxLimit)); got.URL.Query().Get("limit") != want {
		t.Errorf("limit = %q, want it clamped to %q -- an unclamped page size has the "+
			"agent buffer the whole cluster before the byte cap is consulted",
			got.URL.Query().Get("limit"), want)
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

// twoRowTableJSON is what the API server returns for
// includeObject=Metadata: printed cells plus a PartialObjectMetadata per row.
const twoRowTableJSON = `{"kind":"Table","apiVersion":"meta.k8s.io/v1",` +
	`"columnDefinitions":[{"name":"Name","type":"string"}],` +
	`"rows":[` +
	`{"cells":["be-1"],"object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1",` +
	`"metadata":{"name":"be-1","namespace":"stage"}}},` +
	`{"cells":["fe-1"],"object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1",` +
	`"metadata":{"name":"fe-1","namespace":"stage"}}}` +
	`]}`

// TestTableViewFiltersRowsByNamePattern is the TABLE twin of
// TestListFiltersRowsByNamePattern. Decide deliberately passes name="" for a
// LIST and delegates the name patterns to the executor, so a view that skips
// that filter hands back objects the identical FULL query drops and a direct
// GET refuses -- the view must not decide what the policy permits.
func TestTableViewFiltersRowsByNamePattern(t *testing.T) {
	var got http.Request
	_, clients := tableServerBody(t, &got, twoRowTableJSON)
	e := tableExecutorFor(t, clients, `
query:
  rules:
    - namespace: stage
      resources: [pods]
      names: ["be-*"]
`)

	res := e.Execute(context.Background(), tableQuery())
	if res.GetError() != nil {
		t.Fatalf("error = %+v, want nil", res.GetError())
	}
	body := string(res.GetPayload())
	if !strings.Contains(body, "be-1") {
		t.Errorf("be-1 must survive the name pattern, got %s", body)
	}
	if strings.Contains(body, "fe-1") {
		t.Errorf("fe-1 is outside names: [\"be-*\"] and must be filtered out of the "+
			"table exactly as the full view filters it, got %s", body)
	}
}

// secretTableJSON carries the leak: a Secret applied with `kubectl apply` keeps
// a full copy of its manifest, base64 values and all, in the
// last-applied-configuration annotation, and PartialObjectMetadata copies
// annotations verbatim.
const secretTableJSON = `{"kind":"Table","apiVersion":"meta.k8s.io/v1",` +
	`"columnDefinitions":[{"name":"Name","type":"string"},{"name":"Type","type":"string"},` +
	`{"name":"Data","type":"string"},{"name":"Age","type":"string"}],` +
	`"rows":[{"cells":["db-creds","Opaque",1,"3d"],"object":{"kind":"PartialObjectMetadata",` +
	`"apiVersion":"meta.k8s.io/v1","metadata":{"name":"db-creds","namespace":"stage",` +
	`"managedFields":[{"manager":"kubectl-client-side-apply"}],"annotations":{` +
	`"kubectl.kubernetes.io/last-applied-configuration":` +
	`"{\"apiVersion\":\"v1\",\"kind\":\"Secret\",\"data\":{\"password\":\"aHVudGVyMg==\"}}"` +
	`}}}}]}`

// TestTableViewStripsLastAppliedConfigurationFromSecretRows guards the second
// copy of a Secret's payload. The stripping is unconditional in
// state.SanitizeUnstructured and must stay that way here: an owner who sets
// redact_secrets:true and still sees aHVudGVyMg== on screen has a setting that
// does nothing.
func TestTableViewStripsLastAppliedConfigurationFromSecretRows(t *testing.T) {
	var got http.Request
	_, clients := tableServerBody(t, &got, secretTableJSON)
	e := tableExecutorFor(t, clients, `
query:
  redact_secrets: true
  rules:
    - namespace: stage
      resources: [secrets]
`)

	q := tableQuery()
	q.Ref = &agentv1.ResourceRef{Group: "", Version: "v1", Resource: "secrets"}
	res := e.Execute(context.Background(), q)
	if res.GetError() != nil {
		t.Fatalf("error = %+v, want nil", res.GetError())
	}
	body := string(res.GetPayload())
	if strings.Contains(body, "aHVudGVyMg==") {
		t.Errorf("the Secret's base64 value left the cluster in a TABLE response, got %s", body)
	}
	if strings.Contains(body, "last-applied-configuration") {
		t.Errorf("last-applied-configuration must be stripped unconditionally, got %s", body)
	}
	if strings.Contains(body, "managedFields") {
		t.Errorf("managedFields must be stripped unconditionally, got %s", body)
	}
	if !strings.Contains(body, "db-creds") {
		t.Errorf("the row itself must survive sanitization, got %s", body)
	}
}

// TestTableViewPreservesLargeNumbersInCells pins the round trip through
// filterTable against a CRD printer column holding an integer too large for a
// float64 mantissa -- a nanosecond timestamp is ~1.7e18, well past 2^53.
// TableRow.Cells is []any, so decoding without UseNumber would land the value
// in a float64 and re-encode it altered, silently corrupting a printed cell
// that the verbatim path used to pass through untouched.
func TestTableViewPreservesLargeNumbersInCells(t *testing.T) {
	const nanos = "1753900000123456789"
	body := `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
		"columnDefinitions":[{"name":"Name","type":"string"},{"name":"Nanos","type":"integer"}],
		"rows":[{"cells":["be-1",` + nanos + `],
			"object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1",
				"metadata":{"name":"be-1","namespace":"stage"}}}]}`

	var got http.Request
	_, clients := tableServerBody(t, &got, body)
	e := tableExecutor(t, clients)

	res := e.Execute(context.Background(), tableQuery())
	if res.GetError() != nil {
		t.Fatalf("error = %+v, want nil", res.GetError())
	}
	if !strings.Contains(string(res.GetPayload()), nanos) {
		t.Errorf("cell value %s did not survive the round trip, got %s", nanos, res.GetPayload())
	}
}
