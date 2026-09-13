package exec_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// The field numbers are the wire contract with kubexa-backend, which imports
// this module by version. A renumbering compiles on both sides and breaks
// every console on the next upgrade, so pin them.
func TestConsoleFieldNumbersArePinned(t *testing.T) {
	gm := &agentv1.GatewayMessage{Payload: &agentv1.GatewayMessage_ExecOpen{
		ExecOpen: &agentv1.ExecOpen{SessionId: "s"},
	}}
	fd := gm.ProtoReflect().Descriptor().Fields().ByName("exec_open")
	if fd == nil || fd.Number() != 9 {
		t.Fatalf("GatewayMessage.exec_open must be field 9, got %v", fd)
	}
	ack := (&agentv1.ExecAttachAck{}).ProtoReflect().Descriptor().Fields()
	if ack.ByName("replay_from_seq").Number() != 3 {
		t.Fatal("ExecAttachAck.replay_from_seq must be field 3")
	}
	exit := (&agentv1.ExecExit{}).ProtoReflect().Descriptor().Fields()
	if exit.ByName("reason").Number() != 3 {
		t.Fatal("ExecExit.reason must be field 3")
	}
}

func TestExecClientMessageRoundTrip(t *testing.T) {
	in := &agentv1.ExecClientMessage{Payload: &agentv1.ExecClientMessage_Data{Data: &agentv1.ExecData{
		Channel: agentv1.ExecChannel_EXEC_CHANNEL_STDOUT, Chunk: []byte("hi"), Seq: 7,
	}}}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out agentv1.ExecClientMessage
	if err := proto.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.GetData().GetSeq() != 7 || string(out.GetData().GetChunk()) != "hi" {
		t.Fatalf("round trip lost data: %v", &out)
	}
}

func TestExecServiceHasExecSession(t *testing.T) {
	var found bool
	for _, m := range agentv1.AgentService_ServiceDesc.Streams {
		if m.StreamName == "ExecSession" && m.ClientStreams && m.ServerStreams {
			found = true
		}
	}
	if !found {
		t.Fatal("AgentService.ExecSession must be a bidirectional stream")
	}
}

// The backend maps the agent's exit reason by its enum NAME, so the name
// and value are wire contract: renumbering or renaming breaks a release.
func TestHelperRejectedReasonIsPinned(t *testing.T) {
	const want = 9
	if got := int32(agentv1.ExecExitReason_EXEC_EXIT_REASON_HELPER_REJECTED); got != want {
		t.Fatalf("HELPER_REJECTED = %d, want %d", got, want)
	}
	if s := agentv1.ExecExitReason_EXEC_EXIT_REASON_HELPER_REJECTED.String(); s != "EXEC_EXIT_REASON_HELPER_REJECTED" {
		t.Fatalf("name = %q", s)
	}
}
