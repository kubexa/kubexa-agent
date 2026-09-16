package stream

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// fakeNodeJobs records every Start and Cancel call the dispatcher hands it,
// and lets the test drive events back through the emit closure the manager
// gave it -- mirroring blockingMutationResponder's "entered" pattern from
// manager_mutation_test.go, but for a responder that answers with a STREAM
// of events instead of one return value.
type fakeNodeJobs struct {
	mu       sync.Mutex
	started  []*agentv1.NodeJobRequest
	cancels  []string
	emitters []func(*agentv1.NodeJobEvent)
}

func (f *fakeNodeJobs) Start(req *agentv1.NodeJobRequest, emit func(*agentv1.NodeJobEvent)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, req)
	f.emitters = append(f.emitters, emit)
}

func (f *fakeNodeJobs) Cancel(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, id)
	return true
}

// nodeJobGateway is a fake gateway that, unlike mockGateway's onConnect
// scripts, lets the test push GatewayMessages at arbitrary times after the
// handshake (via push) and inspect every AgentMessage the manager sent back
// (via sent) and the HandshakeRequest it opened with (via handshake).
type nodeJobGateway struct {
	agentv1.UnimplementedAgentServiceServer

	pushCh chan *agentv1.GatewayMessage

	mu   sync.Mutex
	hs   *agentv1.HandshakeRequest
	msgs []*agentv1.AgentMessage
}

func newNodeJobGateway() *nodeJobGateway {
	return &nodeJobGateway{pushCh: make(chan *agentv1.GatewayMessage, 8)}
}

func (g *nodeJobGateway) Connect(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.hs = first.GetHandshake()
	g.mu.Unlock()
	if err := stream.Send(&agentv1.GatewayMessage{
		Payload: &agentv1.GatewayMessage_Handshake{
			Handshake: &agentv1.HandshakeResponse{Accepted: true, SessionId: "sess-nodejob"},
		},
	}); err != nil {
		return err
	}

	recvErr := make(chan error, 1)
	go func() {
		for {
			m, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			g.mu.Lock()
			g.msgs = append(g.msgs, m)
			g.mu.Unlock()
		}
	}()

	for {
		select {
		case gm := <-g.pushCh:
			if err := stream.Send(gm); err != nil {
				return err
			}
		case err := <-recvErr:
			return err
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

func (g *nodeJobGateway) push(msg *agentv1.GatewayMessage) {
	g.pushCh <- msg
}

func (g *nodeJobGateway) sent() []*agentv1.AgentMessage {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*agentv1.AgentMessage, len(g.msgs))
	copy(out, g.msgs)
	return out
}

// handshake returns the HandshakeRequest this gateway observed, waiting for
// it to arrive.
func (g *nodeJobGateway) handshake(t *testing.T) *agentv1.HandshakeRequest {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		hs := g.hs
		g.mu.Unlock()
		if hs != nil {
			return hs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("handshake was never observed by the fake gateway")
	return nil
}

// newManagerWithNodeJobs builds a manager against a nodeJobGateway, with
// jobs wired in as the nodeJobs responder (nil is valid -- see
// TestNodeJobWithoutResponderIsDropped), and waits for it to connect.
func newManagerWithNodeJobs(t *testing.T, jobs NodeJobResponder) (*streamManager, *nodeJobGateway) {
	t.Helper()

	gw := newNodeJobGateway()
	_, lis := startBufGRPCServer(t, gw)

	cfg := testConfig()
	sm, _ := newTestManager(t, cfg, newTestQueue(t), lis)
	sm.nodeJobs = jobs

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	go func() { _ = sm.Run(ctx) }()

	waitFor(t, 3*time.Second, func() bool { return sm.Connected() })

	return sm, gw
}

// A node_job on the stream reaches the responder, and every event the
// responder emits rides back as AgentMessage.node_job_event.
func TestNodeJobIsDispatchedAndEventsRideBack(t *testing.T) {
	t.Parallel()

	jobs := &fakeNodeJobs{}
	mgr, gw := newManagerWithNodeJobs(t, jobs)

	gw.push(&agentv1.GatewayMessage{
		MessageId: "g1",
		Payload: &agentv1.GatewayMessage_NodeJob{
			NodeJob: &agentv1.NodeJobRequest{JobId: "j1", Node: "w1", Verb: agentv1.NodeJobVerb_NODE_JOB_VERB_CORDON},
		},
	})
	waitFor(t, 2*time.Second, func() bool {
		jobs.mu.Lock()
		defer jobs.mu.Unlock()
		return len(jobs.started) == 1
	})
	jobs.mu.Lock()
	got := jobs.started[0].GetJobId()
	emit := jobs.emitters[0]
	jobs.mu.Unlock()
	if got != "j1" {
		t.Fatalf("started job_id = %q, want j1", got)
	}

	emit(&agentv1.NodeJobEvent{JobId: "j1", Phase: agentv1.NodeJobPhase_NODE_JOB_PHASE_SUCCEEDED})
	waitFor(t, 2*time.Second, func() bool {
		for _, m := range gw.sent() {
			if m.GetNodeJobEvent().GetJobId() == "j1" {
				return true
			}
		}
		return false
	})

	gw.push(&agentv1.GatewayMessage{
		MessageId: "g2",
		Payload: &agentv1.GatewayMessage_NodeJobCancel{
			NodeJobCancel: &agentv1.NodeJobCancel{JobId: "j1"},
		},
	})
	waitFor(t, 2*time.Second, func() bool {
		jobs.mu.Lock()
		defer jobs.mu.Unlock()
		return len(jobs.cancels) == 1 && jobs.cancels[0] == "j1"
	})
	_ = mgr
}

// With no responder wired, a node_job is dropped and nothing is sent: the
// capability told the gateway not to dispatch, and an old gateway that does
// anyway gets silence, never a panic.
func TestNodeJobWithoutResponderIsDropped(t *testing.T) {
	t.Parallel()

	mgr, gw := newManagerWithNodeJobs(t, nil)
	gw.push(&agentv1.GatewayMessage{
		MessageId: "g1",
		Payload:   &agentv1.GatewayMessage_NodeJob{NodeJob: &agentv1.NodeJobRequest{JobId: "j1"}},
	})
	time.Sleep(50 * time.Millisecond)
	for _, m := range gw.sent() {
		if m.GetNodeJobEvent() != nil {
			t.Fatal("an event was sent with no responder")
		}
	}
	_ = mgr
}

// The handshake advertises node_ops exactly when a responder is wired.
func TestHandshakeAdvertisesNodeOps(t *testing.T) {
	t.Parallel()

	_, gw := newManagerWithNodeJobs(t, &fakeNodeJobs{})
	hs := gw.handshake(t)
	if !hs.GetCaps().GetNodeOps() {
		t.Fatal("node_ops false with a responder wired")
	}

	_, gw = newManagerWithNodeJobs(t, nil)
	if gw.handshake(t).GetCaps().GetNodeOps() {
		t.Fatal("node_ops true with no responder")
	}
}
