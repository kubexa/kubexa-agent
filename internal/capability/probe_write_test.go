package capability

import (
	"context"
	"sync"
	"testing"

	authv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// verbTally counts SelfSubjectAccessReview creates by resource+subresource+
// verb, so a test can assert a review was never issued rather than merely
// that its answer was false. subresource is "" for a review on the
// top-level resource itself (list/watch/patch/delete/create), matching
// allowed()'s own "" convention -- so a caller checking a plain verb, like
// the pre-existing patch/delete/create assertions below, passes "" and gets
// exactly the same key it always has. A caller checking a SUBRESOURCE verb
// (pods/exec's "create") must pass "exec" explicitly, which is what makes
// this tally able to tell "pods/exec create" apart from a plain "pods
// create" -- two materially different RBAC permissions that would otherwise
// collide on the same resource+verb key.
type verbTally struct {
	mu     sync.Mutex
	counts map[string]int
}

func (t *verbTally) inc(resource, subresource, verb string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counts == nil {
		t.counts = map[string]int{}
	}
	t.counts[resource+":"+subresource+":"+verb]++
}

func (t *verbTally) get(resource, subresource, verb string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counts[resource+":"+subresource+":"+verb]
}

// countingAuthzClient always allows, but tallies every review it receives by
// resource+subresource+verb.
func countingAuthzClient(tally *verbTally) *fake.Clientset {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			ssar := action.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
			ra := ssar.Spec.ResourceAttributes
			tally.inc(ra.Resource, ra.Subresource, ra.Verb)
			return true, &authv1.SelfSubjectAccessReview{
				Status: authv1.SubjectAccessReviewStatus{Allowed: true},
			}, nil
		})
	return cs
}

// fakeMutatePolicy names a fixed set of GVRs as patch-granted; everything
// else is outside the policy.
type fakeMutatePolicy struct {
	named map[string]bool // "group/version/resource" -> named
}

func (f fakeMutatePolicy) AllowsAnyWrite(group, version, resource string) (patch, del, create, scale bool) {
	if f.named[group+"/"+version+"/"+resource] {
		return true, false, false, false
	}
	return false, false, false, false
}

// Probing four extra verbs on every GVR in the cluster would multiply
// catalog cost. Only GVRs the mutate policy names are probed, which is a
// finite set precisely because mutate rules forbid a wildcard.
func TestWriteVerbsAreProbedOnlyForPolicyNamedResources(t *testing.T) {
	tally := &verbTally{}
	cs := countingAuthzClient(tally)
	mp := fakeMutatePolicy{named: map[string]bool{"apps/v1/deployments": true}}

	Probe(context.Background(), cs.AuthorizationV1(), gvrs(), 4, mp, nil, nil, "")

	for _, verb := range []string{"patch", "delete", "create"} {
		if n := tally.get("secrets", "", verb); n != 0 {
			t.Fatalf("secrets:%s issued %d SelfSubjectAccessReviews, want 0 -- "+
				"secrets is outside the mutate policy", verb, n)
		}
	}
	for _, verb := range []string{"patch", "delete", "create"} {
		if n := tally.get("deployments", "", verb); n != 1 {
			t.Fatalf("deployments:%s issued %d SelfSubjectAccessReviews, want 1 -- "+
				"deployments is named by the mutate policy", verb, n)
		}
	}

	// Sanity: prove the fake actually ran for BOTH entries, so the zero
	// counts asserted above cannot pass merely because nothing ran at all.
	for _, resource := range []string{"deployments", "secrets"} {
		for _, verb := range []string{"list", "watch"} {
			if tally.get(resource, "", verb) == 0 {
				t.Fatalf("%s:%s tally is zero; the fake was never exercised for this GVR", resource, verb)
			}
		}
	}
}

// fakeExecPolicy answers AllowsAnyPod as configured, mirroring
// fakeMutatePolicy's shape for the same probe-gating pattern: a primitive
// bool the probe consults before spending an SSAR.
type fakeExecPolicy struct {
	allow bool
}

func (f fakeExecPolicy) AllowsAnyPod() bool { return f.allow }

// podAndDeploymentGVRs is a two-entry catalog for the exec test: a core-group
// pods entry (the only one exec is ever reported for) and a deployments entry
// (the contrast case -- Phase B does not touch it).
func podAndDeploymentGVRs() []GVR {
	return []GVR{
		{Group: "", Version: "v1", Resource: "pods", Kind: "Pod", Namespaced: true},
		{Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment", Namespaced: true},
	}
}

// TestCatalogReportsExecForPodsOnly pins the third independent policy
// source: exec's can_exec/policy_exec, mirroring the CanPatch/PolicyPatch
// pattern above but gated on the core-group "pods" resource specifically,
// never on any other GVR. The core-group "nodes" resource is the one other
// entry that reports exec, through the separate node policy (nodeExecPolicy
// argument, nil here) -- see TestProbeExecNodesUsesTheHelperNamespace.
func TestCatalogReportsExecForPodsOnly(t *testing.T) {
	tally := &verbTally{}
	cs := countingAuthzClient(tally)
	ep := fakeExecPolicy{allow: true}

	got := byResource(Probe(context.Background(), cs.AuthorizationV1(), podAndDeploymentGVRs(), 4, nil, ep, nil, ""))

	pods := got["pods"]
	if !pods.CanExec || !pods.PolicyExec {
		t.Fatalf("pods = %+v, want CanExec and PolicyExec both true", pods)
	}
	if n := tally.get("pods", "exec", "create"); n != 1 {
		t.Fatalf("pods/exec:create issued %d SelfSubjectAccessReviews, want exactly 1", n)
	}
	// The subresource is load-bearing: pods/exec:create is a materially
	// different RBAC permission from a plain pods:create, and a probeExec
	// regression to allowed(ctx, authz, g, "", "create") -- asking about
	// ordinary pod creation instead of the exec subresource -- must fail
	// this test rather than pass it by coincidence.
	if n := tally.get("pods", "", "create"); n != 0 {
		t.Fatalf("pods:create (no subresource) issued %d SelfSubjectAccessReviews, want 0 -- "+
			"the exec probe must ask about the exec SUBRESOURCE, not plain pod creation", n)
	}

	deployments := got["deployments"]
	if deployments.CanExec || deployments.PolicyExec {
		t.Fatalf("deployments = %+v, want CanExec and PolicyExec both false -- exec is pods-only", deployments)
	}

	t.Run("nil exec policy", func(t *testing.T) {
		tally2 := &verbTally{}
		cs2 := countingAuthzClient(tally2)

		got2 := byResource(Probe(context.Background(), cs2.AuthorizationV1(), podAndDeploymentGVRs(), 4, nil, nil, nil, ""))

		pods2 := got2["pods"]
		if pods2.CanExec || pods2.PolicyExec {
			t.Fatalf("pods = %+v, want CanExec and PolicyExec both false when ExecPolicy is nil", pods2)
		}
		if n := tally2.get("pods", "exec", "create"); n != 0 {
			t.Fatalf("pods/exec:create issued %d SelfSubjectAccessReviews, want 0 when ExecPolicy is nil", n)
		}
		if n := tally2.get("pods", "", "create"); n != 0 {
			t.Fatalf("pods:create (no subresource) issued %d SelfSubjectAccessReviews, want 0 when ExecPolicy is nil", n)
		}
	})
}

// An unprobed entry reports false, which is the same answer an agent
// without the feature gives -- not "unknown", and never "allowed".
func TestUnprobedEntriesReportFalse(t *testing.T) {
	cs := authzClient(map[string]bool{
		"deployments:list": true, "deployments:watch": true,
		"secrets:list": true, "secrets:watch": true,
	}, nil)
	mp := fakeMutatePolicy{named: map[string]bool{"apps/v1/deployments": true}}

	got := byResource(Probe(context.Background(), cs.AuthorizationV1(), gvrs(), 4, mp, nil, nil, ""))

	s := got["secrets"]
	if s.CanPatch || s.CanDelete || s.CanCreate {
		t.Fatalf("secrets = %+v, want can_patch/can_delete/can_create all false -- "+
			"it is outside the mutate policy and must never be probed", s)
	}
	if s.PolicyPatch || s.PolicyDelete || s.PolicyCreate || s.PolicyScale {
		t.Fatalf("secrets = %+v, want policy_patch/delete/create/scale all false", s)
	}

	// A named entry is the contrast case: it must actually get probed, so
	// the assertions above are not vacuously true for every entry.
	d := got["deployments"]
	if !d.PolicyPatch {
		t.Fatalf("deployments = %+v, want policy_patch true -- it is named by the mutate policy", d)
	}
}
