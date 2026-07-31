// Package query answers live, on-demand resource reads from the gateway.
//
// It is the pull counterpart to the state collector: nothing is started,
// nothing is remembered, and no response is cached. The agent stays stateless
// so its memory footprint does not grow with how much anyone looks at.
package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubexa/kubexa-agent/internal/collector/state"
	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/internal/query/policy"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

const (
	defaultTimeout  = 10 * time.Second
	minTimeout      = 1 * time.Second
	maxTimeout      = 30 * time.Second
	defaultMaxBytes = 8 << 20
	defaultLimit    = int32(500)
	// maxLimit caps the page size a requester may ask for. Without it, a
	// query carrying limit: 2000000 has the agent ask the API server for two
	// million objects and decode and buffer every one of them BEFORE maxBytes
	// is ever consulted -- the byte cap runs on the assembled result, far too
	// late to save a memory-limited pod in the customer's cluster.
	maxLimit = int32(5000)
)

// Options configures an Executor. Only Clients and Policy are required.
type Options struct {
	Clients      k8s.QueryClients
	Policy       *policy.Policy
	Logger       *logger.Logger
	Registerer   prometheus.Registerer
	MaxInFlight  int
	MaxQueued    int
	MaxBytes     int
	DefaultLimit int32
}

// Executor answers one ResourceQuery at a time against the Kubernetes API.
type Executor struct {
	clients  k8s.QueryClients
	policy   *policy.Policy
	log      *logger.Logger
	metrics  *recorders
	gate     *gate
	maxBytes int
	defLimit int32
}

// New builds an Executor.
func New(opts Options) (*Executor, error) {
	if opts.Policy == nil {
		return nil, errors.New("query: policy is required")
	}
	if opts.Clients.Dynamic == nil {
		return nil, errors.New("query: dynamic client is required")
	}
	if opts.Logger == nil {
		opts.Logger = logger.New("query")
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultMaxBytes
	}
	if opts.DefaultLimit <= 0 {
		opts.DefaultLimit = defaultLimit
	}
	// The configured default is capped by the same ceiling as a request's
	// limit; an operator cannot opt their own pod into the OOM maxLimit exists
	// to prevent.
	if opts.DefaultLimit > maxLimit {
		opts.DefaultLimit = maxLimit
	}
	return &Executor{
		clients:  opts.Clients,
		policy:   opts.Policy,
		log:      opts.Logger,
		metrics:  newRecorders(opts.Registerer),
		gate:     newGate(opts.MaxInFlight, opts.MaxQueued),
		maxBytes: opts.MaxBytes,
		defLimit: opts.DefaultLimit,
	}, nil
}

// Execute answers one query. It never returns an error: the wire contract is
// that the agent always replies, so a failure is reported inside the result.
func (e *Executor) Execute(ctx context.Context, q *agentv1.ResourceQuery) *agentv1.ResourceQueryResult {
	if q == nil {
		return &agentv1.ResourceQueryResult{
			Error: queryError(agentv1.QueryErrorCode_QUERY_ERROR_INTERNAL, "empty query"),
		}
	}
	res := e.execute(ctx, q)
	res.QueryId = q.GetQueryId()
	return res
}

func (e *Executor) execute(ctx context.Context, q *agentv1.ResourceQuery) *agentv1.ResourceQueryResult {
	ref := policy.Ref{
		Group:    q.GetRef().GetGroup(),
		Version:  q.GetRef().GetVersion(),
		Resource: q.GetRef().GetResource(),
	}
	verb := policy.VerbList
	if q.GetVerb() == agentv1.QueryVerb_QUERY_VERB_GET {
		verb = policy.VerbGet
	}
	view := "table"
	if q.GetView() != agentv1.QueryView_QUERY_VIEW_TABLE {
		view = "full"
	}

	// The policy gate runs before anything else touches the network. A refused
	// query must never reach the customer's API server; executor_test.go
	// asserts the API client recorded zero calls for a denied request.
	decision := e.policy.Decide(ref, verb, q.GetNamespace(), q.GetName())
	if !decision.Allowed {
		// unknownResource, not ref.Resource: nothing has validated the string
		// off the wire at this point. See metrics.go for why recording it
		// would be an unbounded, permanent allocation inside the customer's
		// cluster. Everything past this gate matched a rule the owner wrote,
		// so those paths keep the real resource.
		e.metrics.observe(string(verb), unknownResource, view, "policy_denied", 0, 0)
		// Logged at debug, not warn: a denial is the policy working, not a
		// fault. It is here so an operator debugging "why can't I see this
		// type" gets the reason without enabling anything exotic.
		e.log.Debug("live query denied by policy",
			logger.F("resource", ref.Resource),
			logger.F("namespace", q.GetNamespace()),
			logger.F("verb", string(verb)),
			logger.F("reason", decision.Reason),
		)
		return &agentv1.ResourceQueryResult{
			Error: queryError(agentv1.QueryErrorCode_QUERY_ERROR_POLICY_DENIED, decision.Reason),
		}
	}
	if verb == policy.VerbGet && q.GetName() == "" {
		e.metrics.observe(string(verb), ref.Resource, view, "internal", 0, 0)
		return &agentv1.ResourceQueryResult{
			Error: queryError(agentv1.QueryErrorCode_QUERY_ERROR_INTERNAL, "get requires a name"),
		}
	}

	release, ok := e.gate.acquire(ctx)
	if !ok {
		e.metrics.observe(string(verb), ref.Resource, view, "resource_exhausted", 0, 0)
		return &agentv1.ResourceQueryResult{
			Error: queryError(agentv1.QueryErrorCode_QUERY_ERROR_RESOURCE_EXHAUSTED,
				"too many concurrent queries for this agent; retry shortly"),
		}
	}
	defer release()

	e.metrics.enter()
	defer e.metrics.exit()

	ctx, cancel := context.WithTimeout(ctx, clampTimeout(q.GetTimeoutMs()))
	defer cancel()

	start := time.Now()
	var res *agentv1.ResourceQueryResult
	if verb == policy.VerbGet {
		res = e.get(ctx, ref, decision, q)
	} else {
		res = e.list(ctx, ref, decision, q)
	}
	outcome := "ok"
	if res.GetError() != nil {
		outcome = strings.ToLower(strings.TrimPrefix(
			res.GetError().GetCode().String(), "QUERY_ERROR_"))
	}
	e.metrics.observe(string(verb), ref.Resource, view, outcome,
		time.Since(start).Seconds(), len(res.GetPayload()))
	return res
}

func (e *Executor) list(
	ctx context.Context,
	ref policy.Ref,
	decision policy.Decision,
	q *agentv1.ResourceQuery,
) *agentv1.ResourceQueryResult {
	if q.GetView() == agentv1.QueryView_QUERY_VIEW_TABLE {
		return e.listTable(ctx, ref, decision, q)
	}
	opts := metav1.ListOptions{
		LabelSelector: andSelector(decision.LabelSelector, q.GetLabelSelector()),
		FieldSelector: andSelector(decision.FieldSelector, q.GetFieldSelector()),
		Limit:         int64(e.clampLimit(q.GetLimit())),
		Continue:      q.GetContinueToken(),
	}

	list, err := e.resource(ref, q.GetNamespace()).List(ctx, opts)
	if err != nil {
		return &agentv1.ResourceQueryResult{Error: mapAPIError(err)}
	}

	items := make([]json.RawMessage, 0, len(list.Items))
	total := 0
	truncated := false
	for i := range list.Items {
		obj := &list.Items[i]
		// A name pattern cannot be pushed to the API server (Kubernetes has no
		// name glob), so it is applied here, against the patterns the
		// AUTHORIZING rule carried out in the decision -- never by asking the
		// policy again per row, which could land on a different, more
		// permissive rule. Consequence of filtering here: paging happens
		// BEFORE this filter, so a page may return fewer rows than limit while
		// more pages remain. The consumer must treat continue_token as the
		// only end-of-pagination signal.
		if !policy.MatchesName(obj.GetName(), decision.NamePatterns) {
			continue
		}
		state.SanitizeUnstructured(obj, ref.Resource, decision.RedactSecrets)
		raw, err := json.Marshal(obj.Object)
		if err != nil {
			return &agentv1.ResourceQueryResult{
				Error: queryError(agentv1.QueryErrorCode_QUERY_ERROR_INTERNAL, err.Error()),
			}
		}
		if total+len(raw) > e.maxBytes {
			truncated = true
			break
		}
		total += len(raw)
		items = append(items, raw)
	}

	payload, err := json.Marshal(items)
	if err != nil {
		return &agentv1.ResourceQueryResult{
			Error: queryError(agentv1.QueryErrorCode_QUERY_ERROR_INTERNAL, err.Error()),
		}
	}
	// GetRemainingItemCount returns *int64, not int64: the API server omits the
	// field entirely unless it is serving a paged list, and nil means "unknown",
	// not zero. Dereferencing unconditionally would panic on the common case.
	var remaining int32
	if rc := list.GetRemainingItemCount(); rc != nil {
		remaining = int32(*rc)
	}
	return &agentv1.ResourceQueryResult{
		Payload:       payload,
		ContinueToken: list.GetContinue(),
		Remaining:     remaining,
		Truncated:     truncated,
	}
}

func (e *Executor) get(
	ctx context.Context,
	ref policy.Ref,
	decision policy.Decision,
	q *agentv1.ResourceQuery,
) *agentv1.ResourceQueryResult {
	obj, err := e.resource(ref, q.GetNamespace()).Get(ctx, q.GetName(), metav1.GetOptions{})
	if err != nil {
		return &agentv1.ResourceQueryResult{Error: mapAPIError(err)}
	}
	state.SanitizeUnstructured(obj, ref.Resource, decision.RedactSecrets)
	payload, err := json.Marshal(obj.Object)
	if err != nil {
		return &agentv1.ResourceQueryResult{
			Error: queryError(agentv1.QueryErrorCode_QUERY_ERROR_INTERNAL, err.Error()),
		}
	}
	if len(payload) > e.maxBytes {
		return &agentv1.ResourceQueryResult{
			Error: queryError(agentv1.QueryErrorCode_QUERY_ERROR_TOO_LARGE,
				fmt.Sprintf("object is %d bytes, over the %d byte limit", len(payload), e.maxBytes)),
		}
	}
	return &agentv1.ResourceQueryResult{Payload: payload}
}

// resource addresses the GVR, namespaced or not. An empty namespace yields the
// cluster-scoped/all-namespaces form, which is exactly what the policy already
// decided is permitted -- no RESTMapper lookup is needed to tell the two apart.
func (e *Executor) resource(ref policy.Ref, namespace string) dynamicResource {
	gvr := schema.GroupVersionResource{
		Group: ref.Group, Version: ref.Version, Resource: ref.Resource,
	}
	if namespace == "" {
		return e.clients.Dynamic.Resource(gvr)
	}
	return e.clients.Dynamic.Resource(gvr).Namespace(namespace)
}

// dynamicResource is the subset of dynamic.ResourceInterface this package uses.
type dynamicResource interface {
	List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error)
	Get(ctx context.Context, name string, opts metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error)
}

// clampTimeout keeps a caller-supplied deadline inside a band the agent can
// honour. Zero means "use the default" rather than "no timeout": an unbounded
// query would pin a slot in the concurrency gate indefinitely.
func clampTimeout(ms int32) time.Duration {
	if ms <= 0 {
		return defaultTimeout
	}
	d := time.Duration(ms) * time.Millisecond
	if d < minTimeout {
		return minTimeout
	}
	if d > maxTimeout {
		return maxTimeout
	}
	return d
}

// clampLimit resolves the page size actually asked of the API server: the
// configured default when the request set none, and never more than maxLimit.
// Both list paths go through it -- a cap only the FULL view honours is not a
// cap.
func (e *Executor) clampLimit(limit int32) int32 {
	if limit <= 0 {
		return e.defLimit
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}

// andSelector conjoins the policy's selector with the request's, so a
// requester can only ever narrow what the policy already permits.
func andSelector(policySel, requestSel string) string {
	p := strings.TrimSpace(policySel)
	r := strings.TrimSpace(requestSel)
	switch {
	case p == "":
		return r
	case r == "":
		return p
	default:
		return p + "," + r
	}
}

func mapAPIError(err error) *agentv1.QueryError {
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return queryError(agentv1.QueryErrorCode_QUERY_ERROR_RBAC_DENIED, err.Error())
	case apierrors.IsNotFound(err):
		return queryError(agentv1.QueryErrorCode_QUERY_ERROR_NOT_FOUND, err.Error())
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err):
		return queryError(agentv1.QueryErrorCode_QUERY_ERROR_TIMEOUT, err.Error())
	default:
		return queryError(agentv1.QueryErrorCode_QUERY_ERROR_INTERNAL, err.Error())
	}
}

func queryError(code agentv1.QueryErrorCode, msg string) *agentv1.QueryError {
	return &agentv1.QueryError{Code: code, Message: msg}
}
