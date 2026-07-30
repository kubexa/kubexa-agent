package capability

import (
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

type fakeDiscovery struct {
	discovery.DiscoveryInterface
	lists []*metav1.APIResourceList
	err   error
}

func (f *fakeDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return f.lists, f.err
}

func lists() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: []string{"list", "watch", "get"}},
				{Name: "pods/log", Kind: "Pod", Namespaced: true, Verbs: []string{"get"}},
				{Name: "bindings", Kind: "Binding", Namespaced: true, Verbs: []string{"create"}},
				{Name: "nodes", Kind: "Node", Namespaced: false, Verbs: []string{"list", "watch"}},
			},
		},
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", Kind: "Deployment", Namespaced: true, Verbs: []string{"list", "watch"}},
			},
		},
	}
}

// Subresources and resources the API server says cannot be listed are dropped
// before any SSAR is spent on them: the server has already answered.
func TestDiscoverDropsSubresourcesAndNonListable(t *testing.T) {
	got, failed, err := Discover(&fakeDiscovery{lists: lists()})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failedGroups = %v, want none", failed)
	}

	want := map[string]bool{"/v1/pods": true, "/v1/nodes": true, "apps/v1/deployments": true}
	if len(got) != len(want) {
		t.Fatalf("got %d resources (%v), want %d", len(got), got, len(want))
	}
	for _, g := range got {
		key := g.Group + "/" + g.Version + "/" + g.Resource
		if !want[key] {
			t.Fatalf("unexpected resource %q", key)
		}
	}
}

// A broken APIService must degrade the catalog, never erase it. This is the
// single most important behaviour in this file: one unhealthy operator would
// otherwise blank the whole type list.
func TestDiscoverReturnsPartialResultOnGroupFailure(t *testing.T) {
	failure := &discovery.ErrGroupDiscoveryFailed{
		Groups: map[schema.GroupVersion]error{
			{Group: "metrics.k8s.io", Version: "v1beta1"}: errors.New("service unavailable"),
		},
	}
	got, failed, err := Discover(&fakeDiscovery{lists: lists(), err: failure})
	if err != nil {
		t.Fatalf("Discover returned error %v, want partial success", err)
	}
	if len(got) == 0 {
		t.Fatal("got no resources, want the partial list")
	}
	if len(failed) != 1 || failed[0] != "metrics.k8s.io/v1beta1" {
		t.Fatalf("failedGroups = %v, want [metrics.k8s.io/v1beta1]", failed)
	}
}

func TestDiscoverPropagatesNonGroupErrors(t *testing.T) {
	if _, _, err := Discover(&fakeDiscovery{err: errors.New("connection refused")}); err == nil {
		t.Fatal("err = nil, want the transport error propagated")
	}
}

// Map iteration order must never look like a cluster change, or the SSAR
// sweep would re-run on every refresh for no reason.
func TestFingerprintIsOrderIndependent(t *testing.T) {
	a := []GVR{
		{Group: "apps", Version: "v1", Resource: "deployments"},
		{Group: "", Version: "v1", Resource: "pods"},
	}
	b := []GVR{a[1], a[0]}

	if Fingerprint(a) != Fingerprint(b) {
		t.Fatalf("Fingerprint differs by order: %q vs %q", Fingerprint(a), Fingerprint(b))
	}
}

func TestFingerprintChangesWithMembership(t *testing.T) {
	a := []GVR{{Group: "", Version: "v1", Resource: "pods"}}
	b := append(append([]GVR{}, a...), GVR{Group: "", Version: "v1", Resource: "nodes"})

	if Fingerprint(a) == Fingerprint(b) {
		t.Fatal("Fingerprint unchanged after adding a resource")
	}
}
