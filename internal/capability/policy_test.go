package capability

import (
	"testing"
	"time"
)

type fakePolicy struct {
	list map[string]bool
	get  map[string]bool
}

func key(g, v, r string) string { return g + "/" + v + "/" + r }

func (f fakePolicy) AllowsAnyList(g, v, r string) bool { return f.list[key(g, v, r)] }
func (f fakePolicy) AllowsAnyGet(g, v, r string) bool  { return f.get[key(g, v, r)] }

func TestBuildCatalogCarriesPolicyVerdicts(t *testing.T) {
	caps := []Capability{
		{GVR: GVR{Group: "", Version: "v1", Resource: "pods", Kind: "Pod", Namespaced: true},
			CanList: true, CanWatch: true, PolicyList: true, PolicyGet: true},
		{GVR: GVR{Group: "", Version: "v1", Resource: "secrets", Kind: "Secret", Namespaced: true},
			CanList: true, CanWatch: true, PolicyList: true, PolicyGet: false},
	}
	cat := buildCatalog(caps, nil, "fp", time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC))

	if len(cat.GetEntries()) != 2 {
		t.Fatalf("got %d entries, want 2", len(cat.GetEntries()))
	}
	pods, secrets := cat.GetEntries()[0], cat.GetEntries()[1]
	if !pods.GetPolicyList() || !pods.GetPolicyGet() {
		t.Error("pods must report both policy verbs")
	}
	if !secrets.GetPolicyList() {
		t.Error("secrets must report policy_list")
	}
	if secrets.GetPolicyGet() {
		t.Error("secrets must report policy_get=false when only list is granted")
	}
	// RBAC and policy stay independent signals.
	if !secrets.GetCanList() {
		t.Error("can_list must be untouched by the policy fields")
	}
}

func TestApplyPolicyMarksEachCapability(t *testing.T) {
	caps := []Capability{
		{GVR: GVR{Group: "", Version: "v1", Resource: "pods"}},
		{GVR: GVR{Group: "apps", Version: "v1", Resource: "deployments"}},
	}
	p := fakePolicy{
		list: map[string]bool{key("", "v1", "pods"): true},
		get:  map[string]bool{},
	}

	applyPolicy(caps, p)

	if !caps[0].PolicyList {
		t.Error("pods must be marked list-allowed")
	}
	if caps[0].PolicyGet {
		t.Error("pods must not be marked get-allowed")
	}
	if caps[1].PolicyList || caps[1].PolicyGet {
		t.Error("deployments must be marked as denied on both verbs")
	}
}

func TestApplyPolicyWithNilSourceDeniesEverything(t *testing.T) {
	// A nil policy means live query is not configured. Reporting true would
	// tell the UI to offer types no query can ever answer.
	caps := []Capability{{GVR: GVR{Group: "", Version: "v1", Resource: "pods"}}}
	applyPolicy(caps, nil)
	if caps[0].PolicyList || caps[0].PolicyGet {
		t.Fatal("a nil policy source must leave both verdicts false")
	}
}
