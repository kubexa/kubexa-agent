package capability

import (
	"context"
	"errors"
	"sync"
	"testing"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// allow decides the fake API server's answer per (resource, verb).
func authzClient(allow map[string]bool, failOn map[string]bool) *fake.Clientset {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			ssar := action.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
			ra := ssar.Spec.ResourceAttributes
			key := ra.Resource + ":" + ra.Verb
			if failOn[key] {
				return true, nil, errors.New("apiserver unavailable")
			}
			return true, &authv1.SelfSubjectAccessReview{
				Status: authv1.SubjectAccessReviewStatus{Allowed: allow[key]},
			}, nil
		})
	return cs
}

func gvrs() []GVR {
	return []GVR{
		{Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment", Namespaced: true},
		{Group: "", Version: "v1", Resource: "secrets", Kind: "Secret", Namespaced: true},
	}
}

func byResource(caps []Capability) map[string]Capability {
	m := make(map[string]Capability, len(caps))
	for _, c := range caps {
		m[c.Resource] = c
	}
	return m
}

// list and watch are asked separately and must stay separate: list-yes /
// watch-no is the polling-fallback case the UI depends on.
func TestProbeReportsListAndWatchIndependently(t *testing.T) {
	cs := authzClient(map[string]bool{
		"deployments:list": true, "deployments:watch": false,
		"secrets:list": false, "secrets:watch": false,
	}, nil)

	got := byResource(Probe(context.Background(), cs.AuthorizationV1(), gvrs(), 4, nil, nil, nil, ""))

	if d := got["deployments"]; !d.CanList || d.CanWatch {
		t.Fatalf("deployments = list %v / watch %v, want true/false", d.CanList, d.CanWatch)
	}
	if s := got["secrets"]; s.CanList || s.CanWatch {
		t.Fatalf("secrets = list %v / watch %v, want false/false", s.CanList, s.CanWatch)
	}
	for _, c := range got {
		if c.ProbeFailed {
			t.Fatalf("%s: ProbeFailed set on a successful probe", c.Resource)
		}
	}
}

// A failing SSAR is not a denial. Reporting it as can_list=false would hide
// the resource and send the operator hunting through their own RBAC.
func TestProbeMarksProbeFailedRatherThanDenied(t *testing.T) {
	cs := authzClient(
		map[string]bool{"secrets:list": true, "secrets:watch": true},
		map[string]bool{"deployments:list": true},
	)

	got := byResource(Probe(context.Background(), cs.AuthorizationV1(), gvrs(), 4, nil, nil, nil, ""))

	d := got["deployments"]
	if !d.ProbeFailed {
		t.Fatal("deployments: ProbeFailed = false, want true when the SSAR itself errors")
	}
	if d.CanList {
		t.Fatal("deployments: CanList = true, want false alongside ProbeFailed")
	}
	if s := got["secrets"]; s.ProbeFailed || !s.CanList || !s.CanWatch {
		t.Fatalf("secrets = %+v, want a clean allow", s)
	}
}

// The agent runs cluster-wide informers, so the question it must ask is
// "across all namespaces", i.e. an empty Namespace in the ResourceAttributes.
func TestProbeAsksClusterWide(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var seen []*authv1.ResourceAttributes
	cs.PrependReactor("create", "selfsubjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			ssar := action.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
			seen = append(seen, ssar.Spec.ResourceAttributes)
			return true, &authv1.SelfSubjectAccessReview{
				Status: authv1.SubjectAccessReviewStatus{Allowed: true},
			}, nil
		})

	Probe(context.Background(), cs.AuthorizationV1(), gvrs()[:1], 1, nil, nil, nil, "")

	if len(seen) != 2 {
		t.Fatalf("issued %d reviews, want 2 (list + watch)", len(seen))
	}
	for _, ra := range seen {
		if ra.Namespace != "" {
			t.Fatalf("Namespace = %q, want \"\" so the check covers every namespace", ra.Namespace)
		}
		if ra.Group != "apps" || ra.Resource != "deployments" {
			t.Fatalf("attributes = %+v, want the apps/deployments GVR", ra)
		}
	}
	_ = metav1.NamespaceAll
}

func TestProbeReturnsOneCapabilityPerGVR(t *testing.T) {
	cs := authzClient(map[string]bool{}, nil)
	if got := Probe(context.Background(), cs.AuthorizationV1(), gvrs(), 8, nil, nil, nil, ""); len(got) != 2 {
		t.Fatalf("got %d capabilities, want 2", len(got))
	}
}

// Watch is only worth asking about when list is allowed: without a first page
// there is nothing to watch, and the backend collapses that case to
// "unavailable" without consulting canWatch. Since the agent's RBAC is an
// operator-chosen allowlist, most GVRs in a cluster are denied, so skipping
// the second review there roughly halves the sweep. A ~400-review sweep
// against a rate-limited client is what produced client-side throttling
// warnings in production.
func TestProbeSkipsWatchWhenListIsDenied(t *testing.T) {
	var verbs []string
	var mu sync.Mutex
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			ssar := action.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
			mu.Lock()
			verbs = append(verbs, ssar.Spec.ResourceAttributes.Verb)
			mu.Unlock()
			return true, &authv1.SelfSubjectAccessReview{
				Status: authv1.SubjectAccessReviewStatus{Allowed: false},
			}, nil
		})

	got := Probe(context.Background(), cs.AuthorizationV1(), gvrs()[:1], 1, nil, nil, nil, "")

	if len(verbs) != 1 || verbs[0] != "list" {
		t.Fatalf("issued reviews for %v, want exactly [list] — watch must not be asked once list is denied", verbs)
	}
	if got[0].CanList || got[0].CanWatch || got[0].ProbeFailed {
		t.Fatalf("capability = %+v, want a plain deny with no probe failure", got[0])
	}
}

// A watch review that errors must not leave the resource looking merely
// poll-only: that would silently downgrade a watchable type on a transient
// API hiccup. Unknown is the honest state.
// recordingAuthzWithAllow lets a test configure the Allowed verdict per
// resource/subresource/verb key (matching verbTally's own convention in
// probe_write_test.go) while also recording every ResourceAttributes the
// fake receives, so a test can assert both what was asked and what came
// back.
func recordingAuthzWithAllow(allow map[string]bool, seen *[]*authv1.ResourceAttributes) *fake.Clientset {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			ssar := action.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
			ra := ssar.Spec.ResourceAttributes
			*seen = append(*seen, ra)
			key := ra.Resource + ":" + ra.Subresource + ":" + ra.Verb
			return true, &authv1.SelfSubjectAccessReview{
				Status: authv1.SubjectAccessReviewStatus{Allowed: allow[key]},
			}, nil
		})
	return cs
}

// fakeNodeExecPolicy answers AllowsAnyNode as configured, mirroring
// fakeExecPolicy's shape in probe_write_test.go for the same probe-gating
// pattern.
type fakeNodeExecPolicy struct {
	allow bool
}

func (f fakeNodeExecPolicy) AllowsAnyNode() bool { return f.allow }

// fakeNodeExecPolicyPtr is a pointer-receiver policy whose method is
// nil-receiver-safe, mirroring execpolicy.NodePolicy.AllowsAnyNode on this
// branch: a *fakeNodeExecPolicyPtr(nil) stored in the NodeExecPolicySource
// interface is a non-nil interface value, so probeExec's `nodeExecPolicy ==
// nil` guard does not catch it -- the guard against this case has to be the
// nil-safety of the method itself.
type fakeNodeExecPolicyPtr struct {
	allow bool
}

func (f *fakeNodeExecPolicyPtr) AllowsAnyNode() bool {
	if f == nil {
		return false
	}
	return f.allow
}

// TestProbeExecNodesUsesTheHelperNamespace pins probeExec's "nodes" branch:
// policy_exec comes from the node exec policy, can_exec is the AND of two
// SSARs -- helper-Pod creation and pods/exec -- both scoped to the helper
// namespace rather than metav1.NamespaceAll (unlike every other probe in
// this package, which asks cluster-wide).
func TestProbeExecNodesUsesTheHelperNamespace(t *testing.T) {
	nodesGVR := GVR{Group: "", Resource: "nodes"}

	t.Run("both allowed", func(t *testing.T) {
		var seen []*authv1.ResourceAttributes
		cs := recordingAuthzWithAllow(map[string]bool{
			"pods::create":     true,
			"pods:exec:create": true,
		}, &seen)

		c := Capability{GVR: nodesGVR}
		probeExec(context.Background(), cs.AuthorizationV1(), nodesGVR, nil, fakeNodeExecPolicy{allow: true}, "kubexa", &c)

		if !c.PolicyExec {
			t.Fatal("PolicyExec = false, want true when the node exec policy allows any node")
		}
		if !c.CanExec {
			t.Fatal("CanExec = false, want true when both SSARs allow")
		}
		if len(seen) != 2 {
			t.Fatalf("issued %d SelfSubjectAccessReviews, want 2", len(seen))
		}
		for _, ra := range seen {
			if ra.Namespace != "kubexa" {
				t.Fatalf("Namespace = %q, want the helper namespace %q", ra.Namespace, "kubexa")
			}
		}
		if seen[0].Resource != "pods" || seen[0].Subresource != "" || seen[0].Verb != "create" {
			t.Fatalf("first SSAR = %+v, want {Resource: pods, Subresource: \"\", Verb: create}", seen[0])
		}
		if seen[1].Resource != "pods" || seen[1].Subresource != "exec" || seen[1].Verb != "create" {
			t.Fatalf("second SSAR = %+v, want {Resource: pods, Subresource: exec, Verb: create}", seen[1])
		}
	})

	// CanExec is true only when BOTH SSARs allow: deny just the pods/exec
	// half and the AND must still come out false, not the pods:create half.
	t.Run("pods/exec denied", func(t *testing.T) {
		var seen []*authv1.ResourceAttributes
		cs := recordingAuthzWithAllow(map[string]bool{
			"pods::create":     true,
			"pods:exec:create": false,
		}, &seen)

		c := Capability{GVR: nodesGVR}
		probeExec(context.Background(), cs.AuthorizationV1(), nodesGVR, nil, fakeNodeExecPolicy{allow: true}, "kubexa", &c)

		if !c.PolicyExec {
			t.Fatal("PolicyExec = false, want true")
		}
		if c.CanExec {
			t.Fatal("CanExec = true, want false when the pods/exec SSAR is denied even though pods:create is allowed")
		}
	})

	t.Run("nil node exec policy", func(t *testing.T) {
		var seen []*authv1.ResourceAttributes
		cs := recordingAuthzWithAllow(map[string]bool{
			"pods::create":     true,
			"pods:exec:create": true,
		}, &seen)

		c := Capability{GVR: nodesGVR}
		probeExec(context.Background(), cs.AuthorizationV1(), nodesGVR, nil, nil, "kubexa", &c)

		if c.PolicyExec || c.CanExec {
			t.Fatalf("capability = %+v, want PolicyExec and CanExec both false when nodeExecPolicy is nil", c)
		}
		if len(seen) != 0 {
			t.Fatalf("issued %d SelfSubjectAccessReviews, want 0 when nodeExecPolicy is nil", len(seen))
		}
	})

	// The typed-nil trap: a nil *execpolicy.NodePolicy stored in the
	// NodeExecPolicySource interface is a non-nil interface value, so this
	// must NOT be mistaken for the "nil node exec policy" case above. It is
	// acceptable only because AllowsAnyNode is nil-receiver-safe and answers
	// false, which is what this asserts.
	t.Run("typed nil policy value is a non-nil interface but answers false safely", func(t *testing.T) {
		var seen []*authv1.ResourceAttributes
		cs := recordingAuthzWithAllow(map[string]bool{
			"pods::create":     true,
			"pods:exec:create": true,
		}, &seen)

		var typedNil *fakeNodeExecPolicyPtr
		c := Capability{GVR: nodesGVR}
		probeExec(context.Background(), cs.AuthorizationV1(), nodesGVR, nil, typedNil, "kubexa", &c)

		if c.PolicyExec || c.CanExec {
			t.Fatalf("capability = %+v, want PolicyExec and CanExec both false for a nil-receiver-safe node policy", c)
		}
		if len(seen) != 0 {
			t.Fatalf("issued %d SelfSubjectAccessReviews, want 0", len(seen))
		}
	})
}

func TestProbeMarksUnknownWhenOnlyTheWatchReviewFails(t *testing.T) {
	cs := authzClient(
		map[string]bool{"deployments:list": true},
		map[string]bool{"deployments:watch": true},
	)

	got := Probe(context.Background(), cs.AuthorizationV1(), gvrs()[:1], 1, nil, nil, nil, "")

	if !got[0].ProbeFailed {
		t.Fatalf("capability = %+v, want ProbeFailed after the watch review errored", got[0])
	}
	if got[0].CanList || got[0].CanWatch {
		t.Fatalf("capability = %+v, want no permission claims alongside ProbeFailed", got[0])
	}
}
