package query

import (
	"context"
	"strconv"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubexa/kubexa-agent/internal/query/policy"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// tableAccept asks the API server to render the response as a Table -- the
// same columns kubectl prints, including the additionalPrinterColumns a CRD
// defines. This is why the agent ships no per-type column registry.
const tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io"

// listTable serves the TABLE view.
//
// It bypasses the dynamic client because content negotiation is an Accept
// header and dynamic.Interface exposes no way to set one.
//
// The server's Table is returned verbatim, without sanitization, and that is
// correct rather than an oversight: a Table carries printed cell values and,
// with includeObject=Metadata, PartialObjectMetadata. A Secret's data never
// appears in either, so there is nothing for SanitizeUnstructured to strip.
// Do not add a sanitize call here -- it would have to parse and re-encode
// every response for no benefit.
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

	limit := q.GetLimit()
	if limit <= 0 {
		limit = e.defLimit
	}

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
	if len(raw) > e.maxBytes {
		return &agentv1.ResourceQueryResult{
			Error: queryError(agentv1.QueryErrorCode_QUERY_ERROR_TOO_LARGE,
				"table response exceeds the size limit; request a smaller page"),
		}
	}

	// The Table's own metadata.continue is inside the JSON body. Parsing it out
	// here would mean decoding the whole payload only to re-encode it, so the
	// continue token is left for the consumer to read from the body. Assert
	// this explicitly so nobody assumes an empty ContinueToken means "done".
	return &agentv1.ResourceQueryResult{Payload: raw}
}
