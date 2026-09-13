package capability

import (
	"context"
	"sync"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authzv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
)

// Capability is one GVR plus what the agent may actually do with it.
type Capability struct {
	GVR
	CanList     bool
	CanWatch    bool
	ProbeFailed bool
	// PolicyList and PolicyGet come from the agent's own configuration, not
	// from the API server. RBAC may permit what the cluster owner refuses.
	PolicyList bool
	PolicyGet  bool

	// CanPatch/CanDelete/CanCreate are SelfSubjectAccessReview verdicts, like
	// CanList/CanWatch above. They are probed only for a GVR the mutate
	// policy names for some verb -- see probeWrites -- so an entry outside
	// the policy leaves these at their zero value, false.
	//
	// There is deliberately no CanScale: scale RBAC is a patch on the
	// deployments/scale SUBRESOURCE, which allowed() cannot express (it sets
	// no Subresource), so scale is reported as policy only.
	CanPatch  bool
	CanDelete bool
	CanCreate bool

	// PolicyPatch/PolicyDelete/PolicyCreate/PolicyScale come from the
	// agent's own mutate configuration, mirroring PolicyList/PolicyGet
	// above. PolicyScale has no RBAC counterpart -- see CanScale's absence.
	PolicyPatch  bool
	PolicyDelete bool
	PolicyCreate bool
	PolicyScale  bool

	// CanExec is a SelfSubjectAccessReview verdict for the pods/exec
	// subresource's "create" verb, like CanPatch/CanDelete/CanCreate above.
	// It is probed only for the core-group "pods" entry, and only when the
	// exec policy allows a console on at least one pod -- see probeExec --
	// so every other entry (including "nodes", which is Phase C) leaves this
	// at its zero value, false.
	CanExec bool
	// PolicyExec comes from the agent's own exec.pod configuration,
	// mirroring PolicyPatch above. It is set only for the "pods" entry, for
	// the same reason CanExec is.
	PolicyExec bool
}

// MutatePolicySource reports the agent's configured mutate policy per
// resource. It is a separate interface from PolicySource (reporter.go)
// rather than an extra method on it: a nil PolicySource and a nil
// MutatePolicySource are independent facts -- live query and mutation are
// configured (or not) independently -- and primitive parameters keep this
// package independent of internal/mutate/policy, for the same reason
// PolicySource's doc comment gives for internal/query/policy.
type MutatePolicySource interface {
	AllowsAnyWrite(group, version, resource string) (patch, delete, create, scale bool)
}

// ExecPolicySource reports whether the agent's pod console configuration
// permits a session on any pod at all. It is a THIRD independent policy
// source alongside PolicySource and MutatePolicySource above, for the same
// reason MutatePolicySource's doc comment gives: live query, mutation, and
// exec are each configured (or not) independently, and a primitive-only
// method keeps this package independent of internal/exec/policy.
type ExecPolicySource interface {
	AllowsAnyPod() bool
}

// NodeExecPolicySource reports whether the agent's node console
// configuration permits a session on any node at all. It is a FOURTH
// independent policy source alongside PolicySource, MutatePolicySource and
// ExecPolicySource above, for the same reason: live query, mutation, the pod
// console, and the node console are each configured (or not) independently,
// and a primitive-only method keeps this package independent of
// internal/exec/policy.
type NodeExecPolicySource interface {
	AllowsAnyNode() bool
}

const defaultProbeWorkers = 8

// Probe asks the API server, for each GVR, whether this agent may list and
// watch it across all namespaces.
//
// The answer comes from SelfSubjectAccessReview rather than from parsing our
// own RBAC: the API server is the authority, and a derived answer that is
// wrong produces exactly the silent UI lie this feature exists to prevent.
//
// A review that errors sets ProbeFailed instead of denying. The two mistakes
// are not symmetric — a wrong "allowed" surfaces as an error the user can
// report, while a wrong "denied" makes the resource vanish.
// mutatePolicy is nil when the mutate section is disabled, in which case no
// write verb is ever probed -- see probeWrites. execPolicy is nil the same
// way when exec.pod is disabled, in which case no exec verb is ever probed
// -- see probeExec. nodeExecPolicy is nil the same way again when exec.node
// is disabled; helperNamespace scopes the "nodes" entry's SSARs to where its
// helper Pods are actually created, rather than metav1.NamespaceAll.
func Probe(
	ctx context.Context,
	authz authzv1client.AuthorizationV1Interface,
	gvrs []GVR,
	workers int,
	mutatePolicy MutatePolicySource,
	execPolicy ExecPolicySource,
	nodeExecPolicy NodeExecPolicySource,
	helperNamespace string,
) []Capability {
	if workers <= 0 {
		workers = defaultProbeWorkers
	}

	out := make([]Capability, len(gvrs))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for i, g := range gvrs {
		wg.Add(1)
		go func(i int, g GVR) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			c := Capability{GVR: g}

			// Write permission is an independent fact from list/watch: an
			// entry can be write-capable whatever list/watch turn out to be,
			// so it is probed unconditionally here rather than nested under
			// the list/watch branches below. The two branches that follow
			// only clear CanList/CanWatch/ProbeFailed on their own failure
			// paths -- they must not also erase what was just probed here.
			probeWrites(ctx, authz, g, mutatePolicy, &c)

			// Exec is an independent fact from list/watch too, for the same
			// reason probeWrites is called here rather than nested below.
			probeExec(ctx, authz, g, execPolicy, nodeExecPolicy, helperNamespace, &c)

			canList, listErr := allowed(ctx, authz, g, "", "list")
			if listErr != nil {
				c.ProbeFailed = true
				c.CanList = false
				c.CanWatch = false
				out[i] = c
				return
			}
			c.CanList = canList

			// Only ask about watch when list is allowed. Watch without list is
			// unusable anyway — there is no first page to render — and the
			// backend collapses that case to "unavailable" without consulting
			// canWatch. Since the agent's RBAC is an operator-chosen allowlist,
			// most of a cluster's GVRs are denied, so skipping the second
			// review there roughly halves a sweep that would otherwise issue
			// two API calls for every resource type in the cluster.
			if !canList {
				out[i] = c
				return
			}

			canWatch, watchErr := allowed(ctx, authz, g, "", "watch")
			if watchErr != nil {
				// The list answer is real, but reporting it alongside a
				// defaulted canWatch=false would silently downgrade a
				// watchable type to polling. Unknown is the honest state.
				// The write fields probeWrites already set on c are deliberately
				// left standing, not wiped along with list/watch: they came from a
				// separate SelfSubjectAccessReview this error says nothing about,
				// and catalog.proto's probe_failed comment scopes its meaning to
				// can_list/can_watch only -- not to the entry as a whole.
				c.ProbeFailed = true
				c.CanList = false
				c.CanWatch = false
				out[i] = c
				return
			}
			c.CanWatch = canWatch
			out[i] = c
		}(i, g)
	}
	wg.Wait()
	return out
}

// probeWrites fills c's write can_*/policy_* fields for one GVR.
//
// policy_patch/delete/create/scale come straight from the mutate policy.
// can_patch/delete/create are only probed -- issuing a
// SelfSubjectAccessReview per verb -- when the policy names this GVR for at
// least one write verb. Probing every discovered GVR for four extra verbs
// would multiply the sweep's API cost across a cluster's full resource list;
// gating on the policy keeps the extra cost bounded by the (necessarily
// finite, since mutate rules forbid a wildcard) set of resources an operator
// actually named for mutation. An entry the policy does not name is left
// with can_patch/delete/create at their zero value, false -- the same answer
// an agent without this feature gives, never "unknown" and never "allowed".
//
// can_scale does not exist on the wire: scale RBAC is a patch on the
// deployments/scale SUBRESOURCE, which allowed() cannot express, so scale is
// reported as policy only.
//
// probeWrites is called unconditionally, before the list/watch checks below
// it in Probe, and is NOT gated on canList: write permission is an
// independent RBAC fact from read permission, so a GVR this agent cannot
// list may still be one it can patch, and probing writes must not be skipped
// just because list/watch skipped their own second call.
func probeWrites(
	ctx context.Context,
	authz authzv1client.AuthorizationV1Interface,
	g GVR,
	mutatePolicy MutatePolicySource,
	c *Capability,
) {
	if mutatePolicy == nil {
		return
	}
	patch, del, create, scale := mutatePolicy.AllowsAnyWrite(g.Group, g.Version, g.Resource)
	c.PolicyPatch = patch
	c.PolicyDelete = del
	c.PolicyCreate = create
	c.PolicyScale = scale

	if !patch && !del && !create && !scale {
		return
	}

	// A failed SSAR here is left as an unprobed "false" rather than folded
	// into ProbeFailed: ProbeFailed's contract (see its doc comment) is
	// specifically that can_list/can_watch carry no information, and
	// conflating a write-probe error into that same flag would make a
	// transient hiccup on "create" silently blank an otherwise-good
	// list/watch answer for the same entry.
	if v, err := allowed(ctx, authz, g, "", "patch"); err == nil {
		c.CanPatch = v
	}
	if v, err := allowed(ctx, authz, g, "", "delete"); err == nil {
		c.CanDelete = v
	}
	if v, err := allowed(ctx, authz, g, "", "create"); err == nil {
		c.CanCreate = v
	}
}

// probeExec fills c's exec can_exec/policy_exec fields. Exec is scoped to
// the core-group "pods" entry only -- "nodes" is Phase C, and every other
// GVR is left with CanExec/PolicyExec at their zero value, false, the same
// answer an agent without this feature gives.
//
// policy_exec comes straight from the exec policy. can_exec is only probed
// -- issuing one SelfSubjectAccessReview for the pods/exec subresource's
// "create" verb -- when the policy allows a console on at least one pod,
// mirroring probeWrites's own cost-bounding gate: an unconditional probe on
// every GVR would spend an SSAR nothing needs, but here the set this can
// ever apply to is exactly one entry, "pods", so the gate also keeps the
// probe from firing when exec.pod is configured off.
//
// execPolicy is nil when exec.pod is disabled, in which case this entirely
// skips, the same way probeWrites skips on a nil mutatePolicy.
//
// The core-group "nodes" entry is Phase C's node console: it is handled
// first, as its own early-returning branch, because it answers a completely
// different question (a helper Pod in helperNamespace, not the node itself)
// with its own policy source, nodeExecPolicy. nodeExecPolicy nil means
// exec.node is disabled, mirroring execPolicy nil above; a non-nil interface
// wrapping a nil policy pointer is the acceptable typed-nil described on
// NodePolicy.AllowsAnyNode -- it is nil-receiver-safe, so it simply answers
// false here, same as an explicit "not configured".
func probeExec(
	ctx context.Context,
	authz authzv1client.AuthorizationV1Interface,
	g GVR,
	execPolicy ExecPolicySource,
	nodeExecPolicy NodeExecPolicySource,
	helperNamespace string,
	c *Capability,
) {
	if g.Group == "" && g.Resource == "nodes" {
		if nodeExecPolicy == nil {
			return
		}
		c.PolicyExec = nodeExecPolicy.AllowsAnyNode()
		if !c.PolicyExec {
			return
		}
		// Two verdicts ANDed: the helper Pod must be creatable in its
		// namespace AND pods/exec there must be allowed. Either missing
		// makes the console fail after a click; false here hides it first.
		create, err1 := allowedIn(ctx, authz, helperNamespace, GVR{Resource: "pods"}, "", "create")
		execOK, err2 := allowedIn(ctx, authz, helperNamespace, GVR{Resource: "pods"}, "exec", "create")
		if err1 == nil && err2 == nil {
			c.CanExec = create && execOK
		}
		return
	}
	if g.Group != "" || g.Resource != "pods" {
		return
	}
	if execPolicy == nil {
		return
	}
	c.PolicyExec = execPolicy.AllowsAnyPod()
	if !c.PolicyExec {
		return
	}

	// A failed SSAR here is left as an unprobed "false", for the same reason
	// probeWrites's own comment gives for its write verbs.
	if v, err := allowed(ctx, authz, g, "exec", "create"); err == nil {
		c.CanExec = v
	}
}

// subresource is empty for every call site except probeExec's: an empty
// Subresource on ResourceAttributes means the top-level resource itself,
// exactly matching every pre-existing call's behaviour. allowed asks
// cluster-wide, matching how the agent reads (cluster-wide informers, not
// per-namespace); allowedIn is the namespaced variant probeExec's "nodes"
// branch uses to scope the SSAR to the helper namespace instead.
func allowed(
	ctx context.Context,
	authz authzv1client.AuthorizationV1Interface,
	g GVR,
	subresource string,
	verb string,
) (bool, error) {
	return allowedIn(ctx, authz, metav1.NamespaceAll, g, subresource, verb)
}

func allowedIn(
	ctx context.Context,
	authz authzv1client.AuthorizationV1Interface,
	namespace string,
	g GVR,
	subresource string,
	verb string,
) (bool, error) {
	review := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Namespace:   namespace,
				Group:       g.Group,
				Version:     g.Version,
				Resource:    g.Resource,
				Subresource: subresource,
				Verb:        verb,
			},
		},
	}
	res, err := authz.SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return res.Status.Allowed, nil
}
