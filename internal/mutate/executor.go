package mutate

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
	"k8s.io/apimachinery/pkg/types"

	"github.com/kubexa/kubexa-agent/internal/collector/state"
	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/internal/mutate/policy"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

const (
	defaultTimeout = 10 * time.Second
	minTimeout     = 1 * time.Second
	// maxTimeout is wider than the query path's 30s ceiling: a write, once
	// admitted by policy, deserves the API server's full round trip -- a
	// slow admission webhook or a strategic merge on a large object should
	// not be cut off at the same bound that protects a casual list refresh.
	maxTimeout = 60 * time.Second

	defaultMaxBytes = 8 << 20

	// restartedAtAnnotation is kubectl's own rollout-restart annotation key.
	// Using it (rather than a Kubexa-specific key) means both tools agree
	// about what a restart is: `kubectl rollout status` and any dashboard
	// reading this annotation see the same event Kubexa produced.
	restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"
)

// restartableResources is the closed set of apps/v1 kinds a RESTART may
// target. A Pod cannot be restarted -- there is no template to re-roll -- so
// saying "restart" and doing something else (e.g. a delete) would silently
// redefine the word for a kind it was never asked about.
var restartableResources = map[string]bool{
	"deployments":  true,
	"statefulsets": true,
	"daemonsets":   true,
}

// Options configures an Executor. Only Clients and Policy are required.
type Options struct {
	Clients     k8s.QueryClients
	Policy      *policy.Policy
	Logger      *logger.Logger
	Registerer  prometheus.Registerer
	MaxInFlight int
	MaxQueued   int
	MaxBytes    int
	// RedactSecrets gates whether a Secret's data/stringData are stripped
	// from a result payload. The caller passes cfg.QueryRedactSecrets(): the
	// mutate path reuses the query path's setting rather than inventing a
	// second one, since both answer the same question -- does a secret value
	// leave this agent.
	RedactSecrets bool
}

// Executor applies one MutationRequest at a time against the Kubernetes API.
type Executor struct {
	clients       k8s.QueryClients
	policy        *policy.Policy
	log           *logger.Logger
	metrics       *recorders
	gate          *gate
	maxBytes      int
	redactSecrets bool
}

// New builds an Executor.
func New(opts Options) (*Executor, error) {
	if opts.Policy == nil {
		return nil, errors.New("mutate: policy is required")
	}
	if opts.Clients.Dynamic == nil {
		return nil, errors.New("mutate: dynamic client is required")
	}
	if opts.Logger == nil {
		opts.Logger = logger.New("mutate")
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultMaxBytes
	}
	return &Executor{
		clients:       opts.Clients,
		policy:        opts.Policy,
		log:           opts.Logger,
		metrics:       newRecorders(opts.Registerer),
		gate:          newGate(opts.MaxInFlight, opts.MaxQueued),
		maxBytes:      opts.MaxBytes,
		redactSecrets: opts.RedactSecrets,
	}, nil
}

// Execute applies one mutation. It never returns an error: the wire contract
// is that the agent always replies, so a failure is reported inside the
// result. MutationId is copied from the request on every path -- including
// when req itself is nil, where the proto getter's nil-safe zero value ("")
// is exactly the right answer.
func (e *Executor) Execute(ctx context.Context, req *agentv1.MutationRequest) *agentv1.MutationResult {
	res := e.execute(ctx, req)
	res.MutationId = req.GetMutationId()
	return res
}

func (e *Executor) execute(ctx context.Context, req *agentv1.MutationRequest) *agentv1.MutationResult {
	if req == nil {
		return invalidResult("empty mutation request")
	}

	ref := policy.Ref{
		Group:    req.GetRef().GetGroup(),
		Version:  req.GetRef().GetVersion(),
		Resource: req.GetRef().GetResource(),
	}
	verb, ok := mutationVerb(req.GetVerb())
	if !ok {
		e.metrics.observe("unspecified", unknownResource, "invalid", 0, 0)
		return invalidResult("mutation verb is required")
	}

	// CREATE is decoded before the policy check, not after: the object's
	// name lives in the body, not on the wire (req.Name is typically empty
	// for a create), and Decide's name-pattern matching only runs when it is
	// given a non-empty name. Deciding on the empty wire name first would
	// silently bypass a rule's `names:` filter for every create -- the same
	// class of "matched more than the owner intended" bug the query and
	// config packages already guard against for wildcards. Decoding is pure
	// parsing, not a network call, so doing it before the gate costs nothing
	// a well-formed request would not pay anyway.
	name := req.GetName()
	var createObj *unstructured.Unstructured
	if verb == policy.VerbCreate {
		obj, verr := decodeUnstructured(req.GetPayload())
		if verr != nil {
			e.metrics.observe(string(verb), unknownResource, "invalid", 0, 0)
			return invalidResult(fmt.Sprintf("decode create payload: %v", verr))
		}
		if obj.GetName() == "" {
			e.metrics.observe(string(verb), unknownResource, "invalid", 0, 0)
			return invalidResult("create payload metadata.name must not be empty")
		}
		// A body whose namespace disagrees with the request namespace is
		// REFUSED, never silently resolved to one of them -- the request
		// namespace is what policy.Decide is about to authorize.
		if bodyNS := obj.GetNamespace(); bodyNS != "" && bodyNS != req.GetNamespace() {
			e.metrics.observe(string(verb), unknownResource, "invalid", 0, 0)
			return invalidResult(fmt.Sprintf(
				"create payload metadata.namespace %q disagrees with the request namespace %q",
				bodyNS, req.GetNamespace()))
		}
		obj.SetNamespace(req.GetNamespace())
		name = obj.GetName()
		createObj = obj
	}

	// The policy gate runs before anything else touches the network. A
	// refused mutation must never reach the customer's API server.
	decision := e.policy.Decide(ref, verb, req.GetNamespace(), name)
	if !decision.Allowed {
		e.metrics.observe(string(verb), unknownResource, "policy_denied", 0, 0)
		// Logged at debug, not warn: a denial is the policy working, not a
		// fault -- it gives an operator debugging "why can't I write this"
		// the reason without enabling anything exotic.
		e.log.Debug("mutation denied by policy",
			logger.F("resource", ref.Resource),
			logger.F("namespace", req.GetNamespace()),
			logger.F("name", name),
			logger.F("verb", string(verb)),
			logger.F("reason", decision.Reason),
		)
		// A policy refusal carries POLICY_DENIED rather than RBAC_DENIED so
		// the operator is sent to the agent config, not to the ClusterRole.
		return &agentv1.MutationResult{
			Error: mutationError(agentv1.MutationErrorCode_MUTATION_ERROR_POLICY_DENIED, decision.Reason),
		}
	}

	// Verb-shaped preconditions that must be rejected before any API call --
	// there is no blind-write path, and no dispatch worth doing for a kind
	// RESTART can never mean anything on.
	if verb == policy.VerbPatch && req.GetResourceVersion() == "" {
		e.metrics.observe(string(verb), ref.Resource, "invalid", 0, 0)
		return invalidResult("patch requires resource_version; there is no blind-write path")
	}
	if verb == policy.VerbRestart && !isRestartable(ref) {
		e.metrics.observe(string(verb), ref.Resource, "invalid", 0, 0)
		return invalidResult(
			"restart is only supported for apps/v1 deployments, statefulsets, and daemonsets")
	}

	release, ok := e.gate.acquire(ctx)
	if !ok {
		e.metrics.observe(string(verb), ref.Resource, "resource_exhausted", 0, 0)
		return &agentv1.MutationResult{
			Error: mutationError(agentv1.MutationErrorCode_MUTATION_ERROR_INTERNAL,
				"too many concurrent mutations for this agent; retry shortly"),
		}
	}
	defer release()

	e.metrics.enter()
	defer e.metrics.exit()

	ctx, cancel := context.WithTimeout(ctx, clampTimeout(req.GetTimeoutMs()))
	defer cancel()

	start := time.Now()
	var res *agentv1.MutationResult
	switch verb {
	case policy.VerbPatch:
		res = e.patch(ctx, ref, req)
	case policy.VerbDelete:
		res = e.delete(ctx, ref, req)
	case policy.VerbRestart:
		res = e.restart(ctx, ref, req)
	case policy.VerbScale:
		res = e.scale(ctx, ref, req)
	case policy.VerbCreate:
		res = e.create(ctx, ref, req, createObj)
	}
	outcome := "ok"
	if res.GetError() != nil {
		outcome = strings.ToLower(strings.TrimPrefix(res.GetError().GetCode().String(), "MUTATION_ERROR_"))
	}
	e.metrics.observe(string(verb), ref.Resource, outcome, time.Since(start).Seconds(), len(res.GetPayload()))
	return res
}

// patch dispatches on patch_type. "replace" is a whole-object Update with
// the caller's resourceVersion injected so the API server -- never this
// code -- decides whether the write is stale. "merge"/"strategic" inject the
// same precondition into the patch body itself, for the same reason.
func (e *Executor) patch(ctx context.Context, ref policy.Ref, req *agentv1.MutationRequest) *agentv1.MutationResult {
	switch req.GetPatchType() {
	case "replace":
		return e.patchReplace(ctx, ref, req)
	case "merge":
		return e.patchWithPrecondition(ctx, ref, req, types.MergePatchType)
	case "strategic":
		return e.patchWithPrecondition(ctx, ref, req, types.StrategicMergePatchType)
	default:
		return invalidResult(fmt.Sprintf(
			"unsupported patch_type %q; want one of replace, merge, strategic", req.GetPatchType()))
	}
}

func (e *Executor) patchReplace(ctx context.Context, ref policy.Ref, req *agentv1.MutationRequest) *agentv1.MutationResult {
	obj, err := decodeUnstructured(req.GetPayload())
	if err != nil {
		return invalidResult(fmt.Sprintf("decode patch payload: %v", err))
	}
	if verr := enforceTarget(obj, req.GetNamespace(), req.GetName()); verr != nil {
		return invalidResult(verr.Error())
	}
	// The precondition the API server enforces, not us: we never compute a
	// conflict ourselves, we only ask for one.
	obj.SetResourceVersion(req.GetResourceVersion())

	opts := metav1.UpdateOptions{DryRun: dryRunOpts(req.GetDryRun())}
	updated, err := e.resource(ref, req.GetNamespace()).Update(ctx, obj, opts)
	if err != nil {
		return &agentv1.MutationResult{Error: mapAPIError(err)}
	}
	return e.payloadResult(ref, updated)
}

func (e *Executor) patchWithPrecondition(
	ctx context.Context,
	ref policy.Ref,
	req *agentv1.MutationRequest,
	pt types.PatchType,
) *agentv1.MutationResult {
	body, err := injectResourceVersion(req.GetPayload(), req.GetResourceVersion())
	if err != nil {
		return invalidResult(fmt.Sprintf("decode patch payload: %v", err))
	}
	opts := metav1.PatchOptions{DryRun: dryRunOpts(req.GetDryRun())}
	updated, err := e.resource(ref, req.GetNamespace()).Patch(ctx, req.GetName(), pt, body, opts)
	if err != nil {
		return &agentv1.MutationResult{Error: mapAPIError(err)}
	}
	return e.payloadResult(ref, updated)
}

func (e *Executor) delete(ctx context.Context, ref policy.Ref, req *agentv1.MutationRequest) *agentv1.MutationResult {
	opts := metav1.DeleteOptions{DryRun: dryRunOpts(req.GetDryRun())}

	if uid, rv := req.GetUid(), req.GetResourceVersion(); uid != "" || rv != "" {
		p := &metav1.Preconditions{}
		if uid != "" {
			u := types.UID(uid)
			p.UID = &u
		}
		if rv != "" {
			p.ResourceVersion = &rv
		}
		opts.Preconditions = p
	}
	if do := req.GetDeleteOptions(); do != nil {
		if do.GetPropagationPolicy() != "" {
			pp := metav1.DeletionPropagation(do.GetPropagationPolicy())
			opts.PropagationPolicy = &pp
		}
		// -1 means unset on the wire; any other value, including 0 (delete
		// immediately), is an explicit choice the caller made.
		if gp := do.GetGracePeriodSeconds(); gp != -1 {
			opts.GracePeriodSeconds = &gp
		}
	}

	if err := e.resource(ref, req.GetNamespace()).Delete(ctx, req.GetName(), opts); err != nil {
		return &agentv1.MutationResult{Error: mapAPIError(err)}
	}
	// Empty payload for DELETE: there is no resulting object.
	return &agentv1.MutationResult{}
}

func (e *Executor) restart(ctx context.Context, ref policy.Ref, req *agentv1.MutationRequest) *agentv1.MutationResult {
	opts := metav1.PatchOptions{DryRun: dryRunOpts(req.GetDryRun())}
	updated, err := e.resource(ref, req.GetNamespace()).
		Patch(ctx, req.GetName(), types.StrategicMergePatchType, restartPatchBody(), opts)
	if err != nil {
		return &agentv1.MutationResult{Error: mapAPIError(err)}
	}
	return e.payloadResult(ref, updated)
}

func (e *Executor) scale(ctx context.Context, ref policy.Ref, req *agentv1.MutationRequest) *agentv1.MutationResult {
	obj, err := decodeUnstructured(req.GetPayload())
	if err != nil {
		return invalidResult(fmt.Sprintf("decode scale payload: %v", err))
	}
	if verr := enforceTarget(obj, req.GetNamespace(), req.GetName()); verr != nil {
		return invalidResult(verr.Error())
	}
	opts := metav1.UpdateOptions{DryRun: dryRunOpts(req.GetDryRun())}
	updated, err := e.resource(ref, req.GetNamespace()).Update(ctx, obj, opts, "scale")
	if err != nil {
		return &agentv1.MutationResult{Error: mapAPIError(err)}
	}
	return e.payloadResult(ref, updated)
}

// create's body was already decoded and validated in execute (its name and
// namespace are what decided the policy check), so this only performs the
// write.
func (e *Executor) create(
	ctx context.Context,
	ref policy.Ref,
	req *agentv1.MutationRequest,
	obj *unstructured.Unstructured,
) *agentv1.MutationResult {
	opts := metav1.CreateOptions{DryRun: dryRunOpts(req.GetDryRun())}
	created, err := e.resource(ref, req.GetNamespace()).Create(ctx, obj, opts)
	if err != nil {
		return &agentv1.MutationResult{Error: mapAPIError(err)}
	}
	return e.payloadResult(ref, created)
}

// enforceTarget guards against a payload silently retargeting a different
// object than the one policy.Decide just authorized. dynamic.Interface's
// Update takes the object's OWN metadata.name to build the request URL --
// there is no separate name parameter the way Patch and Delete have one --
// so trusting whatever name the payload carries would let a mismatched body
// write an object the policy never evaluated. This mirrors the CREATE rule
// from the design: refuse on disagreement, never silently prefer one value
// over the other, then pin the object to the request's target explicitly.
func enforceTarget(obj *unstructured.Unstructured, namespace, name string) error {
	if got := obj.GetName(); got != "" && got != name {
		return fmt.Errorf("payload metadata.name %q disagrees with the requested name %q", got, name)
	}
	if got := obj.GetNamespace(); got != "" && got != namespace {
		return fmt.Errorf("payload metadata.namespace %q disagrees with the requested namespace %q",
			got, namespace)
	}
	obj.SetName(name)
	if namespace != "" {
		obj.SetNamespace(namespace)
	}
	return nil
}

// payloadResult marshals the API server's response as the result payload,
// redacting Secret values first when configured to. This is the object the
// API server returned -- not the caller's request body -- so a Secret patch
// or restart response carries real values unless redaction is on.
func (e *Executor) payloadResult(ref policy.Ref, obj *unstructured.Unstructured) *agentv1.MutationResult {
	state.SanitizeUnstructured(obj, ref.Resource, e.redactSecrets)
	payload, err := json.Marshal(obj.Object)
	if err != nil {
		return &agentv1.MutationResult{
			Error: mutationError(agentv1.MutationErrorCode_MUTATION_ERROR_INTERNAL, err.Error()),
		}
	}
	if len(payload) > e.maxBytes {
		return &agentv1.MutationResult{
			Error: mutationError(agentv1.MutationErrorCode_MUTATION_ERROR_TOO_LARGE,
				fmt.Sprintf("object is %d bytes, over the %d byte limit", len(payload), e.maxBytes)),
		}
	}
	return &agentv1.MutationResult{Payload: payload}
}

// resource addresses the GVR, namespaced or not. An empty namespace yields
// the cluster-scoped form, which is exactly what the policy already decided
// is permitted.
func (e *Executor) resource(ref policy.Ref, namespace string) dynamicResource {
	gvr := schema.GroupVersionResource{Group: ref.Group, Version: ref.Version, Resource: ref.Resource}
	if namespace == "" {
		return e.clients.Dynamic.Resource(gvr)
	}
	return e.clients.Dynamic.Resource(gvr).Namespace(namespace)
}

// dynamicResource is the subset of dynamic.ResourceInterface this package
// uses.
type dynamicResource interface {
	Create(ctx context.Context, obj *unstructured.Unstructured, options metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error)
	Update(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions, subresources ...string) (*unstructured.Unstructured, error)
	Delete(ctx context.Context, name string, options metav1.DeleteOptions, subresources ...string) error
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, options metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error)
}

// mutationVerb maps the wire verb to the policy's closed verb set. An
// unspecified or out-of-range value maps to (_, false): the request never
// reaches Decide, since there is nothing meaningful to ask permission for.
func mutationVerb(v agentv1.MutationVerb) (policy.Verb, bool) {
	switch v {
	case agentv1.MutationVerb_MUTATION_VERB_PATCH:
		return policy.VerbPatch, true
	case agentv1.MutationVerb_MUTATION_VERB_DELETE:
		return policy.VerbDelete, true
	case agentv1.MutationVerb_MUTATION_VERB_RESTART:
		return policy.VerbRestart, true
	case agentv1.MutationVerb_MUTATION_VERB_SCALE:
		return policy.VerbScale, true
	case agentv1.MutationVerb_MUTATION_VERB_CREATE:
		return policy.VerbCreate, true
	default:
		return "", false
	}
}

// isRestartable reports whether ref names one of the three apps/v1 kinds a
// rollout restart is defined for.
func isRestartable(ref policy.Ref) bool {
	return ref.Group == "apps" && ref.Version == "v1" && restartableResources[ref.Resource]
}

// restartPatchBody builds the strategic-merge patch a rollout restart sends:
// one annotation, timestamped now, nested under the pod template so the
// controller sees its template change and rolls every pod.
func restartPatchBody() []byte {
	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]any{
						restartedAtAnnotation: time.Now().UTC().Format(time.RFC3339),
					},
				},
			},
		},
	}
	// A literal map of strings and maps always marshals; the error return is
	// unreachable.
	b, _ := json.Marshal(patch)
	return b
}

// decodeUnstructured parses a request payload as a generic Kubernetes
// object.
func decodeUnstructured(payload []byte) (*unstructured.Unstructured, error) {
	var obj unstructured.Unstructured
	if err := json.Unmarshal(payload, &obj.Object); err != nil {
		return nil, err
	}
	return &obj, nil
}

// injectResourceVersion sets metadata.resourceVersion on a merge/strategic
// patch body, so the API server can refuse a stale write. It never decides
// staleness itself -- it only asks the API server to.
func injectResourceVersion(payload []byte, resourceVersion string) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, err
	}
	metadata, _ := body["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["resourceVersion"] = resourceVersion
	body["metadata"] = metadata
	return json.Marshal(body)
}

// dryRunOpts translates the wire's dry_run bool into the option every write
// call accepts. There is no "dryrun" pseudo-verb: reaching this point at all
// already required the policy to permit the REAL verb.
func dryRunOpts(dryRun bool) []string {
	if dryRun {
		return []string{metav1.DryRunAll}
	}
	return nil
}

// clampTimeout keeps a caller-supplied deadline inside a band the agent can
// honour. Zero means "use the default" rather than "no timeout": an
// unbounded mutation would pin a slot in the concurrency gate indefinitely.
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

// mapAPIError translates a Kubernetes API error into the wire's closed error
// code set.
func mapAPIError(err error) *agentv1.MutationError {
	switch {
	case apierrors.IsConflict(err):
		return mutationError(agentv1.MutationErrorCode_MUTATION_ERROR_CONFLICT, err.Error())
	case apierrors.IsNotFound(err):
		return mutationError(agentv1.MutationErrorCode_MUTATION_ERROR_NOT_FOUND, err.Error())
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return mutationError(agentv1.MutationErrorCode_MUTATION_ERROR_RBAC_DENIED, err.Error())
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return mutationError(agentv1.MutationErrorCode_MUTATION_ERROR_INVALID, err.Error())
	case apierrors.IsRequestEntityTooLargeError(err):
		return mutationError(agentv1.MutationErrorCode_MUTATION_ERROR_TOO_LARGE, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return mutationError(agentv1.MutationErrorCode_MUTATION_ERROR_TIMEOUT, err.Error())
	default:
		return mutationError(agentv1.MutationErrorCode_MUTATION_ERROR_INTERNAL, err.Error())
	}
}

func mutationError(code agentv1.MutationErrorCode, msg string) *agentv1.MutationError {
	return &agentv1.MutationError{Code: code, Message: msg}
}

func invalidResult(msg string) *agentv1.MutationResult {
	return &agentv1.MutationResult{
		Error: mutationError(agentv1.MutationErrorCode_MUTATION_ERROR_INVALID, msg),
	}
}
