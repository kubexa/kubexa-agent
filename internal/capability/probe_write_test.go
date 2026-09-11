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

// verbTally counts SelfSubjectAccessReview creates by resource+verb, so a
// test can assert a review was never issued rather than merely that its
// answer was false.
type verbTally struct {
	mu     sync.Mutex
	counts map[string]int
}

func (t *verbTally) inc(resource, verb string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counts == nil {
		t.counts = map[string]int{}
	}
	t.counts[resource+":"+verb]++
}

func (t *verbTally) get(resource, verb string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counts[resource+":"+verb]
}

// countingAuthzClient always allows, but tallies every review it receives by
// resource+verb.
func countingAuthzClient(tally *verbTally) *fake.Clientset {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			ssar := action.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
			ra := ssar.Spec.ResourceAttributes
			tally.inc(ra.Resource, ra.Verb)
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

	Probe(context.Background(), cs.AuthorizationV1(), gvrs(), 4, mp)

	for _, verb := range []string{"patch", "delete", "create"} {
		if n := tally.get("secrets", verb); n != 0 {
			t.Fatalf("secrets:%s issued %d SelfSubjectAccessReviews, want 0 -- "+
				"secrets is outside the mutate policy", verb, n)
		}
	}
	for _, verb := range []string{"patch", "delete", "create"} {
		if n := tally.get("deployments", verb); n != 1 {
			t.Fatalf("deployments:%s issued %d SelfSubjectAccessReviews, want 1 -- "+
				"deployments is named by the mutate policy", verb, n)
		}
	}

	// Sanity: prove the fake actually ran for BOTH entries, so the zero
	// counts asserted above cannot pass merely because nothing ran at all.
	for _, resource := range []string{"deployments", "secrets"} {
		for _, verb := range []string{"list", "watch"} {
			if tally.get(resource, verb) == 0 {
				t.Fatalf("%s:%s tally is zero; the fake was never exercised for this GVR", resource, verb)
			}
		}
	}
}

// An unprobed entry reports false, which is the same answer an agent
// without the feature gives -- not "unknown", and never "allowed".
func TestUnprobedEntriesReportFalse(t *testing.T) {
	cs := authzClient(map[string]bool{
		"deployments:list": true, "deployments:watch": true,
		"secrets:list": true, "secrets:watch": true,
	}, nil)
	mp := fakeMutatePolicy{named: map[string]bool{"apps/v1/deployments": true}}

	got := byResource(Probe(context.Background(), cs.AuthorizationV1(), gvrs(), 4, mp))

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
