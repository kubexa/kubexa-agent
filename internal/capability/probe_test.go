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

	got := byResource(Probe(context.Background(), cs.AuthorizationV1(), gvrs(), 4))

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

	got := byResource(Probe(context.Background(), cs.AuthorizationV1(), gvrs(), 4))

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

	Probe(context.Background(), cs.AuthorizationV1(), gvrs()[:1], 1)

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
	if got := Probe(context.Background(), cs.AuthorizationV1(), gvrs(), 8); len(got) != 2 {
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

	got := Probe(context.Background(), cs.AuthorizationV1(), gvrs()[:1], 1)

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
func TestProbeMarksUnknownWhenOnlyTheWatchReviewFails(t *testing.T) {
	cs := authzClient(
		map[string]bool{"deployments:list": true},
		map[string]bool{"deployments:watch": true},
	)

	got := Probe(context.Background(), cs.AuthorizationV1(), gvrs()[:1], 1)

	if !got[0].ProbeFailed {
		t.Fatalf("capability = %+v, want ProbeFailed after the watch review errored", got[0])
	}
	if got[0].CanList || got[0].CanWatch {
		t.Fatalf("capability = %+v, want no permission claims alongside ProbeFailed", got[0])
	}
}
