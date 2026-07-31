package query

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubexa/kubexa-agent/internal/collector/state"
	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/internal/query/policy"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// tableAccept asks the API server to render the response as a Table -- the
// same columns kubectl prints, including the additionalPrinterColumns a CRD
// defines. This is why the agent ships no per-type column registry.
const tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io"

// tableKind is the only kind this path will serve. See filterTable for why a
// body of any other kind is refused rather than passed through.
const tableKind = "Table"

// listTable serves the TABLE view.
//
// It bypasses the dynamic client because content negotiation is an Accept
// header and dynamic.Interface exposes no way to set one.
//
// The server's Table is decoded, filtered, sanitized and re-encoded rather
// than forwarded as received. Two independent reasons, both load-bearing:
//
//   - Names. Decide passes name="" for a LIST, so a rule's `names` patterns
//     are never applied at decision time; the executor owns that filter (see
//     executor.go's list). A pass-through TABLE therefore returns rows that
//     the identical FULL query drops and that a direct GET refuses -- the
//     view would decide what the policy permits.
//   - Secrets. includeObject=Metadata gives every row a PartialObjectMetadata,
//     whose annotations are copied from the object verbatim. For a Secret
//     created or updated with `kubectl apply`, the
//     kubectl.kubernetes.io/last-applied-configuration annotation holds a
//     second, complete copy of the manifest -- every base64 value included.
//     That is exactly why state.SanitizeUnstructured strips it
//     unconditionally, independent of redact_secrets, and why it has to run
//     here too. Without it, a TABLE list of Secrets published their values
//     even with redact_secrets: true.
//
// What a Table can and cannot carry, precisely. A row holds printed cells and,
// with includeObject=Metadata, object metadata -- never spec, status, or
// a Secret's data/stringData. Kubernetes' built-in Secret columns are
// NAME/TYPE/DATA/AGE, where DATA is a key count, so no built-in cell carries a
// Secret value. The residual is the cluster owner's own definitions: a CRD's
// additionalPrinterColumns can aim a printed cell at any field its author
// chose, so cells reflect CRDs that already exist in their cluster and show
// exactly what `kubectl get` shows them. Cells are consequently left alone;
// the row's metadata object is not.
//
// Pagination is unchanged by the re-encode. continue_token and remaining stay
// empty on this path and the consumer reads metadata.continue out of the body,
// as the proto documents; re-marshalling a metav1.Table preserves the body's
// own ListMeta. Row filtering does make the body's remainingItemCount an upper
// bound rather than an exact count -- the same caveat the FULL path already
// carries, for the same reason: paging happens on the API server, before the
// name filter runs here.
func (e *Executor) listTable(
	ctx context.Context,
	ref policy.Ref,
	decision policy.Decision,
	q *agentv1.ResourceQuery,
) *agentv1.ResourceQueryResult {
	if e.clients.RESTFor == nil {
		return &agentv1.ResourceQueryResult{
			Error: queryError(agentv1.QueryErrorCode_QUERY_ERROR_INTERNAL,
				"table view requires a REST client"),
		}
	}
	gv := schema.GroupVersion{Group: ref.Group, Version: ref.Version}
	client, err := e.clients.RESTFor(gv)
	if err != nil {
		return &agentv1.ResourceQueryResult{
			Error: queryError(agentv1.QueryErrorCode_QUERY_ERROR_INTERNAL, err.Error()),
		}
	}

	limit := e.clampLimit(q.GetLimit())

	req := client.Get().
		Resource(ref.Resource).
		SetHeader("Accept", tableAccept).
		Param("limit", strconv.FormatInt(int64(limit), 10)).
		Param("includeObject", "Metadata")
	if ns := q.GetNamespace(); ns != "" {
		req = req.Namespace(ns)
	}
	if sel := andSelector(decision.LabelSelector, q.GetLabelSelector()); sel != "" {
		req = req.Param("labelSelector", sel)
	}
	if sel := andSelector(decision.FieldSelector, q.GetFieldSelector()); sel != "" {
		req = req.Param("fieldSelector", sel)
	}
	if tok := q.GetContinueToken(); tok != "" {
		req = req.Param("continue", tok)
	}

	raw, err := req.Do(ctx).Raw()
	if err != nil {
		return &agentv1.ResourceQueryResult{Error: mapAPIError(err)}
	}
	// Checked against the wire body first so an oversized response is rejected
	// before it is decoded into a second, larger in-memory copy.
	if len(raw) > e.maxBytes {
		return tableTooLarge()
	}

	payload, qerr := e.filterTable(raw, ref, decision)
	if qerr != nil {
		return &agentv1.ResourceQueryResult{Error: qerr}
	}
	// The re-encoded body is capped too, not just the raw one: re-marshalling
	// a metav1.Table writes back fields the server elided, so the payload the
	// gateway receives can be larger than the payload the API server sent.
	if len(payload) > e.maxBytes {
		return tableTooLarge()
	}
	return &agentv1.ResourceQueryResult{Payload: payload}
}

// filterTable applies the policy's name patterns and the standard
// sanitization to every row of a server-rendered Table.
func (e *Executor) filterTable(
	raw []byte,
	ref policy.Ref,
	decision policy.Decision,
) ([]byte, *agentv1.QueryError) {
	var table metav1.Table
	if err := json.Unmarshal(raw, &table); err != nil {
		return nil, queryError(agentv1.QueryErrorCode_QUERY_ERROR_INTERNAL,
			"the API server's table response could not be decoded: "+err.Error())
	}
	// Fail closed on a body that is not a Table. Content negotiation is a
	// request, not a guarantee: an API server with no table converter for the
	// resource (an aggregated one, typically) answers with the ordinary list
	// serialization instead. Those are whole objects, and forwarding them from
	// here would skip both the name filter and sanitization -- precisely the
	// hole this function exists to close.
	if table.Kind != tableKind {
		return nil, queryError(agentv1.QueryErrorCode_QUERY_ERROR_INTERNAL, fmt.Sprintf(
			"the API server answered with kind %q instead of a Table; this resource does "+
				"not support server-side printing, so request the full view instead",
			table.Kind))
	}

	kept := make([]metav1.TableRow, 0, len(table.Rows))
	for i := range table.Rows {
		row := table.Rows[i]
		body := bytes.TrimSpace(row.Object.Raw)

		// A row with no object at all carries nothing to sanitize, but also no
		// name to check. Emit it only when the authorizing rule constrains
		// nothing by name: a row whose name cannot be verified against the
		// patterns that permitted the query must never reach the gateway.
		if len(body) == 0 {
			if len(decision.NamePatterns) == 0 {
				kept = append(kept, row)
			}
			continue
		}

		var fields map[string]any
		if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
			// Undecodable: neither verifiable nor sanitizable. Dropped
			// unconditionally -- the alternative is forwarding bytes whose
			// contents the agent could not inspect.
			e.log.Debug("dropping a table row whose object did not decode",
				logger.F("resource", ref.Resource))
			continue
		}
		obj := &unstructured.Unstructured{Object: fields}
		// Filtered against the patterns the AUTHORIZING rule carried out in
		// the decision, never by re-asking the policy per row: a second rule
		// selection could land on a more permissive rule. See
		// policy.MatchesName.
		if !policy.MatchesName(obj.GetName(), decision.NamePatterns) {
			continue
		}
		state.SanitizeUnstructured(obj, ref.Resource, decision.RedactSecrets)
		enc, err := json.Marshal(obj.Object)
		if err != nil {
			return nil, queryError(agentv1.QueryErrorCode_QUERY_ERROR_INTERNAL, err.Error())
		}
		row.Object.Raw = enc
		kept = append(kept, row)
	}
	table.Rows = kept

	out, err := json.Marshal(&table)
	if err != nil {
		return nil, queryError(agentv1.QueryErrorCode_QUERY_ERROR_INTERNAL, err.Error())
	}
	return out, nil
}

func tableTooLarge() *agentv1.ResourceQueryResult {
	return &agentv1.ResourceQueryResult{
		Error: queryError(agentv1.QueryErrorCode_QUERY_ERROR_TOO_LARGE,
			"table response exceeds the size limit; request a smaller page"),
	}
}
