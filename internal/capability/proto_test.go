package capability_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// The catalog rides the existing AgentMessage oneof. Field 10 is the first
// free number: 3-9 are handshake, logs, state, metrics, heartbeat,
// kube_metrics and prometheus_metrics.
func TestResourceCatalogRoundTripsThroughAgentMessage(t *testing.T) {
	msg := &agentv1.AgentMessage{
		MessageId: "m1",
		Payload: &agentv1.AgentMessage_Catalog{
			Catalog: &agentv1.ResourceCatalog{
				Fingerprint:  "sha256:abc",
				CollectedAt:  1_785_406_076_406,
				FailedGroups: []string{"metrics.k8s.io/v1beta1"},
				Entries: []*agentv1.ResourceCapability{{
					Group:       "apps",
					Version:     "v1",
					Resource:    "deployments",
					Kind:        "Deployment",
					Namespaced:  true,
					CanList:     true,
					CanWatch:    false,
					ProbeFailed: false,
				}},
			},
		},
	}

	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got agentv1.AgentMessage
	if err := proto.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	cat := got.GetCatalog()
	if cat == nil {
		t.Fatal("GetCatalog() = nil, want the catalog payload")
	}
	if cat.GetFingerprint() != "sha256:abc" {
		t.Fatalf("fingerprint = %q, want %q", cat.GetFingerprint(), "sha256:abc")
	}
	if len(cat.GetEntries()) != 1 {
		t.Fatalf("entries = %d, want 1", len(cat.GetEntries()))
	}
	e := cat.GetEntries()[0]
	// can_watch false while can_list is true is the polling-fallback case and
	// must survive the wire as two distinct signals, not one.
	if !e.GetCanList() || e.GetCanWatch() {
		t.Fatalf("canList/canWatch = %v/%v, want true/false", e.GetCanList(), e.GetCanWatch())
	}
	if len(cat.GetFailedGroups()) != 1 {
		t.Fatalf("failedGroups = %v, want one entry", cat.GetFailedGroups())
	}
}
