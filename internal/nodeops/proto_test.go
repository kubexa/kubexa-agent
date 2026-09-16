package nodeops_test

import (
	"testing"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	"google.golang.org/protobuf/proto"
)

// The two gateway payloads and the one agent payload ride the existing
// oneofs on new field numbers. An old gateway must still parse a message
// carrying them; this fails loudly if one is retyped or renumbered.
func TestNodeJobRidesTheExistingOneofs(t *testing.T) {
	gw := &agentv1.GatewayMessage{
		MessageId: "m1",
		Payload: &agentv1.GatewayMessage_NodeJob{NodeJob: &agentv1.NodeJobRequest{
			JobId: "j1",
			Node:  "worker-1",
			Verb:  agentv1.NodeJobVerb_NODE_JOB_VERB_DRAIN,
			Drain: &agentv1.DrainOptions{Force: true, GracePeriodSeconds: -1, TimeoutSec: 300, DryRun: true},
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
	if back.GetNodeJob().GetNode() != "worker-1" || !back.GetNodeJob().GetDrain().GetDryRun() {
		t.Fatalf("node job did not round-trip: %+v", back.GetNodeJob())
	}

	cancel := &agentv1.GatewayMessage{Payload: &agentv1.GatewayMessage_NodeJobCancel{NodeJobCancel: &agentv1.NodeJobCancel{JobId: "j1"}}}
	raw, _ = proto.Marshal(cancel)
	var backCancel agentv1.GatewayMessage
	if err := proto.Unmarshal(raw, &backCancel); err != nil || backCancel.GetNodeJobCancel().GetJobId() != "j1" {
		t.Fatalf("cancel did not round-trip: %v %+v", err, backCancel.GetNodeJobCancel())
	}

	ev := &agentv1.AgentMessage{Payload: &agentv1.AgentMessage_NodeJobEvent{NodeJobEvent: &agentv1.NodeJobEvent{
		JobId: "j1", Phase: agentv1.NodeJobPhase_NODE_JOB_PHASE_RUNNING, PodsTotal: 3,
		Pods: []*agentv1.NodeJobPod{{Namespace: "kube-system", Name: "coredns-1", State: agentv1.NodeJobPodState_NODE_JOB_POD_STATE_BLOCKED, Reason: "pdb"}},
	}}}
	raw, _ = proto.Marshal(ev)
	var backEv agentv1.AgentMessage
	if err := proto.Unmarshal(raw, &backEv); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if got := backEv.GetNodeJobEvent().GetPods()[0].GetReason(); got != "pdb" {
		t.Fatalf("pod reason = %q, want pdb", got)
	}
}

// node_ops must default to false so an agent built before this branch,
// whose handshake omits it, reads as "do not dispatch".
func TestNodeOpsCapabilityDefaultsFalse(t *testing.T) {
	var caps agentv1.AgentCapabilities
	if caps.GetNodeOps() {
		t.Fatal("node_ops defaulted to true")
	}
}

// Enum zero values are UNSPECIFIED, never a real verb or a real phase: an
// old binary that does not know a value must not read it as one.
func TestNodeJobEnumsZeroIsUnspecified(t *testing.T) {
	if agentv1.NodeJobVerb(0).String() != "NODE_JOB_VERB_UNSPECIFIED" {
		t.Fatalf("verb zero = %s", agentv1.NodeJobVerb(0))
	}
	if agentv1.NodeJobPhase(0).String() != "NODE_JOB_PHASE_UNSPECIFIED" {
		t.Fatalf("phase zero = %s", agentv1.NodeJobPhase(0))
	}
	if agentv1.NodeJobPodState(0).String() != "NODE_JOB_POD_STATE_UNSPECIFIED" {
		t.Fatalf("pod state zero = %s", agentv1.NodeJobPodState(0))
	}
	if agentv1.NodeJobErrorCode(0).String() != "NODE_JOB_ERROR_UNSPECIFIED" {
		t.Fatalf("error zero = %s", agentv1.NodeJobErrorCode(0))
	}
}
