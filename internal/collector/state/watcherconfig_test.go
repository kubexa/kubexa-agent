package state_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// WatcherConfig.kinds is a closed enum of 25 values and cannot name a CRD,
// which is the case demand-driven watches exist to serve. The new repeated
// ResourceRef carries GVR strings alongside it; field 5 is the first free
// number and kinds stays for agents that predate this.
func TestWatcherConfigCarriesGVRs(t *testing.T) {
	cfg := &agentv1.WatcherConfig{
		Id:        "demand",
		Namespace: "",
		Kinds:     []agentv1.ResourceKind{agentv1.ResourceKind_RESOURCE_KIND_POD},
		Resources: []*agentv1.ResourceRef{
			{Group: "batch", Version: "v1", Resource: "cronjobs"},
			{Group: "", Version: "v1", Resource: "configmaps"},
			{Group: "example.com", Version: "v1alpha1", Resource: "widgets"},
		},
	}

	raw, err := proto.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got agentv1.WatcherConfig
	if err := proto.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(got.GetResources()) != 3 {
		t.Fatalf("resources = %d, want 3", len(got.GetResources()))
	}
	// The core group is the empty string, not "core" — a round-trip that
	// invented a value here would build the wrong GVR on the agent.
	if g := got.GetResources()[1].GetGroup(); g != "" {
		t.Fatalf("core group = %q, want the empty string", g)
	}
	if got.GetResources()[2].GetResource() != "widgets" {
		t.Fatalf("CRD resource = %q, want widgets", got.GetResources()[2].GetResource())
	}
	if len(got.GetKinds()) != 1 {
		t.Fatalf("kinds = %d, want the legacy field preserved", len(got.GetKinds()))
	}
}
