package stream

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/kubexa/kubexa-agent/internal/logger"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// recordingExecResponder records every ExecOpen the dispatcher hands it.
type recordingExecResponder struct {
	opened chan *agentv1.ExecOpen
}

func (r *recordingExecResponder) Open(_ context.Context, open *agentv1.ExecOpen) {
	r.opened <- open
}

// TestExecOpenReachesTheResponder pins the dispatch: a GatewayMessage_ExecOpen
// on the Connect stream reaches ExecResponder.Open with the same message.
// The responder returns at once by contract (the console runs on its own
// goroutines), so it is called inline, unlike the query and mutation
// responders.
func TestExecOpenReachesTheResponder(t *testing.T) {
	t.Parallel()

	rec := &recordingExecResponder{opened: make(chan *agentv1.ExecOpen, 1)}
	m := &streamManager{
		log:           logger.New("stream-exec-test"),
		sendCh:        make(chan *agentv1.AgentMessage, 4),
		execResponder: rec,
	}
	m.ready.Store(true)

	want := &agentv1.ExecOpen{SessionId: "s-1", Tty: true}
	m.handleGatewayMessage(context.Background(), &agentv1.GatewayMessage{
		Payload: &agentv1.GatewayMessage_ExecOpen{ExecOpen: want},
	})

	select {
	case got := <-rec.opened:
		if got != want {
			t.Fatalf("responder got %v, want the exact ExecOpen dispatched (%v)", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the exec responder was never called; exec_open was not dispatched")
	}
}

// TestExecOpenWithNilResponderIsIgnored mirrors TestMutationDroppedWhenResponderNil:
// an exec_open arriving at an agent that does not answer consoles
// (execResponder nil, exec.pod.enabled false) is dropped without a panic.
func TestExecOpenWithNilResponderIsIgnored(t *testing.T) {
	t.Parallel()

	m := &streamManager{
		log:    logger.New("stream-exec-test"),
		sendCh: make(chan *agentv1.AgentMessage, 4),
	}
	m.ready.Store(true)

	// Must not panic.
	m.handleGatewayMessage(context.Background(), &agentv1.GatewayMessage{
		Payload: &agentv1.GatewayMessage_ExecOpen{ExecOpen: &agentv1.ExecOpen{SessionId: "s-1"}},
	})
}

// TestHandshakeAdvertisesExecPodFromConfig asserts both directions the way
// TestHandshakeAdvertisesMutateFromConfig does: the gateway sends exec_open
// only to an agent whose handshake said exec_pod, so a disabled agent must
// advertise false as deliberately as an enabled one advertises true.
func TestHandshakeAdvertisesExecPodFromConfig(t *testing.T) {
	t.Parallel()

	t.Run("enabled", func(t *testing.T) {
		t.Parallel()
		if got := handshakeExecPodCap(t, true); !got {
			t.Fatal("Caps.ExecPod = false, want true when exec.pod.enabled is true")
		}
	})

	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		if got := handshakeExecPodCap(t, false); got {
			t.Fatal("Caps.ExecPod = true, want false when exec.pod.enabled is false")
		}
	})
}

// nodeReadyResponder wraps an ExecResponder and additionally answers
// NodeConsoleReady, so the handshake can be tested against a responder that
// implements NodeConsoleReporter and one that does not.
type nodeReadyResponder struct {
	ExecResponder
	ready bool
}

func (n nodeReadyResponder) NodeConsoleReady() bool { return n.ready }

// TestHandshakeAdvertisesExecNodeFromTheResponder pins that ExecNode in the
// handshake is read from the ExecResponder via NodeConsoleReporter, not from
// config: a responder that answers NodeConsoleReady(true) advertises
// ExecNode true, and a plain responder without that method advertises false.
func TestHandshakeAdvertisesExecNodeFromTheResponder(t *testing.T) {
	t.Parallel()

	t.Run("responder ready", func(t *testing.T) {
		t.Parallel()
		if got := handshakeExecNodeCap(t, nodeReadyResponder{ready: true}); !got {
			t.Fatal("Caps.ExecNode = false, want true when the responder's NodeConsoleReady() is true")
		}
	})

	t.Run("plain responder", func(t *testing.T) {
		t.Parallel()
		if got := handshakeExecNodeCap(t, &recordingExecResponder{opened: make(chan *agentv1.ExecOpen, 1)}); got {
			t.Fatal("Caps.ExecNode = true, want false when the responder does not implement NodeConsoleReporter")
		}
	})
}

// handshakeExecNodeCap builds a manager exactly as handshakeExecPodCap does,
// but with the given exec responder wired in directly, and returns the
// Caps.ExecNode value the agent advertised.
func handshakeExecNodeCap(t *testing.T, responder ExecResponder) bool {
	t.Helper()

	got := make(chan *agentv1.AgentCapabilities, 1)
	srv := &mockGateway{
		onConnect: func(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
			msg, err := stream.Recv()
			if err != nil {
				return err
			}
			got <- msg.GetHandshake().GetCaps()
			return stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Handshake{
					Handshake: &agentv1.HandshakeResponse{Accepted: true, SessionId: "sess-exec-node-caps"},
				},
			})
		},
	}
	_, lis := startBufGRPCServer(t, srv)

	cfg := testConfig()
	sm, _ := newTestManager(t, cfg, newTestQueue(t), lis)
	sm.execResponder = responder

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = sm.Run(ctx) }()

	select {
	case caps := <-got:
		return caps.GetExecNode()
	case <-time.After(3 * time.Second):
		t.Fatal("handshake was never observed by the fake gateway")
		return false
	}
}

// handshakeExecPodCap builds a manager with exec.pod.enabled set to enabled,
// runs it against a fake gateway that captures the handshake, and returns
// the Caps.ExecPod value the agent advertised. ExecNode is asserted false
// alongside: this manager's execResponder is nil (sm.execResponder is never
// set here), so nodeConsoleReady has no NodeConsoleReporter to read.
func handshakeExecPodCap(t *testing.T, enabled bool) bool {
	t.Helper()

	got := make(chan *agentv1.AgentCapabilities, 1)
	srv := &mockGateway{
		onConnect: func(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
			msg, err := stream.Recv()
			if err != nil {
				return err
			}
			got <- msg.GetHandshake().GetCaps()
			return stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Handshake{
					Handshake: &agentv1.HandshakeResponse{Accepted: true, SessionId: "sess-exec-caps"},
				},
			})
		},
	}
	_, lis := startBufGRPCServer(t, srv)

	cfg := testConfig()
	cfg.Exec.Pod.Enabled = &enabled
	sm, _ := newTestManager(t, cfg, newTestQueue(t), lis)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = sm.Run(ctx) }()

	select {
	case caps := <-got:
		if caps.GetExecNode() {
			t.Fatal("Caps.ExecNode = true, want false with no responder")
		}
		return caps.GetExecPod()
	case <-time.After(3 * time.Second):
		t.Fatal("handshake was never observed by the fake gateway")
		return false
	}
}
