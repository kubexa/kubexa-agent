package mutate_test

import (
	"testing"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	"google.golang.org/protobuf/proto"
)

// A gateway that speaks only the old schema must still parse a message
// carrying the new fields, and an old agent's message must still parse here.
// Field numbers are the contract; this test fails loudly if one is retyped.
func TestMutationRidesTheExistingOneofs(t *testing.T) {
	gw := &agentv1.GatewayMessage{
		MessageId: "m1",
		Payload: &agentv1.GatewayMessage_Mutation{Mutation: &agentv1.MutationRequest{
			MutationId: "q1",
			Verb:       agentv1.MutationVerb_MUTATION_VERB_DELETE,
			Ref:        &agentv1.ResourceRef{Version: "v1", Resource: "pods"},
			Namespace:  "dev",
			Name:       "web-0",
		}},
	}
	raw, err := proto.Marshal(gw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back agentv1.GatewayMessage
	if err := proto.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.GetMutation().GetName() != "web-0" {
		t.Fatalf("name = %q, want web-0", back.GetMutation().GetName())
	}
}

// Capability bools must default to false so an agent built before this
// branch, whose handshake omits them, reads as "do not dispatch".
func TestCapabilityBoolsDefaultFalse(t *testing.T) {
	var caps agentv1.AgentCapabilities
	if caps.GetMutate() || caps.GetExecPod() || caps.GetExecNode() {
		t.Fatal("new capability bools must default to false")
	}
}
