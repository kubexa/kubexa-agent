package stream

import (
	"context"
	"testing"
	"time"

	"github.com/kubexa/kubexa-agent/internal/logger"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	"google.golang.org/grpc"
)

// blockingMutationResponder blocks in Execute until release is closed.
// entered is closed as the first statement of Execute so callers can
// observe that the mutation was actually dispatched (as opposed to a switch
// case that was silently dropped and never called Execute at all) -- the
// same shape blockingResponder uses for the query path in
// manager_query_test.go.
type blockingMutationResponder struct {
	release chan struct{}
	entered chan struct{}
}

func (b *blockingMutationResponder) Execute(_ context.Context, m *agentv1.MutationRequest) *agentv1.MutationResult {
	close(b.entered)
	<-b.release
	return &agentv1.MutationResult{MutationId: m.GetMutationId()}
}

// TestMutationIsAnsweredWithoutBlockingTheRecvLoop proves the recv loop
// stays live while a mutation is in flight, not merely that a result
// eventually arrives -- a switch case that ran the executor inline would
// also eventually produce a result, just after stalling every other
// gateway message (acks, backpressure, shutdown) for as long as the write
// took.
//
// The fake gateway sends a slow mutation and, immediately behind it, a
// backpressure signal. If handleMutation blocked the recv loop, the
// backpressure would never be observed until the mutation's responder was
// released; asserting it arrives WHILE the responder is still blocked is
// the proof. Only after that is the responder released and the mutation
// result checked, proving both halves: the loop stayed live, and the
// mutation still completes and is sent back.
func TestMutationIsAnsweredWithoutBlockingTheRecvLoop(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	entered := make(chan struct{})
	agentMsgs := make(chan *agentv1.AgentMessage, 8)

	srv := &mockGateway{
		onConnect: func(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
			if _, err := stream.Recv(); err != nil {
				return err
			}
			if err := stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Handshake{
					Handshake: &agentv1.HandshakeResponse{Accepted: true, SessionId: "sess-mut"},
				},
			}); err != nil {
				return err
			}
			if err := stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Mutation{
					Mutation: &agentv1.MutationRequest{MutationId: "slow"},
				},
			}); err != nil {
				return err
			}
			// Sent right behind the slow mutation, on the same stream: the
			// recv loop must read and act on this without waiting for the
			// mutation's goroutine to finish.
			if err := stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Backpressure{
					Backpressure: &agentv1.BackpressureSignal{Throttle: true, DelayMs: 50},
				},
			}); err != nil {
				return err
			}
			for {
				msg, err := stream.Recv()
				if err != nil {
					return nil
				}
				agentMsgs <- msg
			}
		},
	}
	_, lis := startBufGRPCServer(t, srv)

	cfg := testConfig()
	sm, _ := newTestManager(t, cfg, newTestQueue(t), lis)
	sm.mutationResponder = &blockingMutationResponder{release: release, entered: entered}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = sm.Run(ctx) }()

	waitFor(t, 3*time.Second, func() bool { return sm.Connected() })

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the mutation responder was never entered; the mutation was not dispatched")
	}

	// The load-bearing assertion: the backpressure message that arrived
	// right behind the still-blocked mutation was processed anyway.
	waitFor(t, 2*time.Second, func() bool { return sm.IsThrottled() })

	close(release)

	waitFor(t, 2*time.Second, func() bool {
		select {
		case msg := <-agentMsgs:
			r := msg.GetMutationResult()
			return r != nil && r.GetMutationId() == "slow"
		default:
			return false
		}
	})

	cancel()
}

// TestHandshakeAdvertisesMutateFromConfig asserts both directions: an old
// gateway and a disabled agent must be indistinguishable to the dispatcher,
// because both mean "do not send mutations" -- so false is exercised just
// as deliberately as true, not left as an assumed default.
func TestHandshakeAdvertisesMutateFromConfig(t *testing.T) {
	t.Parallel()

	t.Run("enabled", func(t *testing.T) {
		t.Parallel()
		if got := handshakeMutateCap(t, true); !got {
			t.Fatal("Caps.Mutate = false, want true when mutate.enabled is true")
		}
	})

	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		if got := handshakeMutateCap(t, false); got {
			t.Fatal("Caps.Mutate = true, want false when mutate.enabled is false")
		}
	})
}

// handshakeMutateCap builds a manager with mutate.enabled set to enabled,
// runs it against a fake gateway that captures the handshake request, and
// returns the Caps.Mutate value the agent advertised.
func handshakeMutateCap(t *testing.T, enabled bool) bool {
	t.Helper()

	got := make(chan bool, 1)
	srv := &mockGateway{
		onConnect: func(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
			msg, err := stream.Recv()
			if err != nil {
				return err
			}
			got <- msg.GetHandshake().GetCaps().GetMutate()
			return stream.Send(&agentv1.GatewayMessage{
				Payload: &agentv1.GatewayMessage_Handshake{
					Handshake: &agentv1.HandshakeResponse{Accepted: true, SessionId: "sess-caps"},
				},
			})
		},
	}
	_, lis := startBufGRPCServer(t, srv)

	cfg := testConfig()
	cfg.Mutate.Enabled = &enabled
	sm, _ := newTestManager(t, cfg, newTestQueue(t), lis)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = sm.Run(ctx) }()

	select {
	case v := <-got:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("handshake was never observed by the fake gateway")
		return false
	}
}

// TestMutationDroppedWhenResponderNil mirrors TestNilResponderIsIgnored in
// manager_query_test.go: a GatewayMessage_Mutation arriving when this agent
// does not run mutations (mutationResponder nil, e.g. mutate.enabled false)
// must be dropped without panicking.
func TestMutationDroppedWhenResponderNil(t *testing.T) {
	t.Parallel()

	m := &streamManager{
		log:    logger.New("stream-mutation-test"),
		sendCh: make(chan *agentv1.AgentMessage, 4),
	}
	m.ready.Store(true)

	// Must not panic.
	m.handleGatewayMessage(context.Background(), &agentv1.GatewayMessage{
		Payload: &agentv1.GatewayMessage_Mutation{
			Mutation: &agentv1.MutationRequest{MutationId: "m1"},
		},
	})
}
