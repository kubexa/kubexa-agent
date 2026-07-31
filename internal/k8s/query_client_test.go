package k8s

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

func TestNewQueryClientsRequiresConfig(t *testing.T) {
	if _, err := NewQueryClients(nil, 0, 0); err == nil {
		t.Fatal("NewQueryClients(nil) = nil error, want an error")
	}
}

func TestRESTForBuildsCoreAndGroupedClients(t *testing.T) {
	restCfg := restConfigForTest(t)

	qc, err := newQueryClientsFromRest(restCfg)
	if err != nil {
		t.Fatalf("newQueryClientsFromRest: %v", err)
	}

	// Core group: APIPath must be /api, not /apis. Getting this backwards
	// produces 404s only at request time, on every core resource.
	core, err := qc.RESTFor(schema.GroupVersion{Group: "", Version: "v1"})
	if err != nil {
		t.Fatalf("RESTFor(core): %v", err)
	}
	if got := core.Get().URL().Path; got != "/api/v1" {
		t.Errorf("core path = %q, want /api/v1", got)
	}

	grouped, err := qc.RESTFor(schema.GroupVersion{Group: "apps", Version: "v1"})
	if err != nil {
		t.Fatalf("RESTFor(apps/v1): %v", err)
	}
	if got := grouped.Get().URL().Path; got != "/apis/apps/v1" {
		t.Errorf("apps path = %q, want /apis/apps/v1", got)
	}
}

func TestRESTForCachesPerGroupVersion(t *testing.T) {
	qc, err := newQueryClientsFromRest(restConfigForTest(t))
	if err != nil {
		t.Fatalf("newQueryClientsFromRest: %v", err)
	}
	gv := schema.GroupVersion{Group: "apps", Version: "v1"}
	a, err := qc.RESTFor(gv)
	if err != nil {
		t.Fatalf("first RESTFor: %v", err)
	}
	b, err := qc.RESTFor(gv)
	if err != nil {
		t.Fatalf("second RESTFor: %v", err)
	}
	if a != b {
		t.Error("RESTFor must return the same client for the same GroupVersion")
	}
}

func restConfigForTest(t *testing.T) *rest.Config {
	t.Helper()
	return &rest.Config{Host: "https://127.0.0.1:6443"}
}
