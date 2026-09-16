package stream

import (
	"context"
	"fmt"
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

// nodeJobSession is one gRPC session observed by nodeJobReconnectGateway:
// the HandshakeRequest that opened it and every AgentMessage it received.
type nodeJobSession struct {
	mu   sync.Mutex
	hs   *agentv1.HandshakeRequest
	msgs []*agentv1.AgentMessage
}

func (s *nodeJobSession) sent() []*agentv1.AgentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*agentv1.AgentMessage, len(s.msgs))
	copy(out, s.msgs)
	return out
}

// nodeJobReconnectGateway is like nodeJobGateway but keeps what it observes
// split by session, and lets the test force the CURRENT session's stream to
// end (forceReconnect) so a real second handshake happens on a fresh
// stream -- the only way to test that a reconnect re-announces retained
// node job snapshots.
type nodeJobReconnectGateway struct {
	agentv1.UnimplementedAgentServiceServer

	closeCh chan struct{}

	mu       sync.Mutex
	sessions []*nodeJobSession
}

func newNodeJobReconnectGateway() *nodeJobReconnectGateway {
	return &nodeJobReconnectGateway{closeCh: make(chan struct{}, 1)}
}

func (g *nodeJobReconnectGateway) Connect(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.GatewayMessage]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	sess := &nodeJobSession{hs: first.GetHandshake()}
	g.mu.Lock()
	n := len(g.sessions) + 1
	g.sessions = append(g.sessions, sess)
	g.mu.Unlock()

	if err := stream.Send(&agentv1.GatewayMessage{
		Payload: &agentv1.GatewayMessage_Handshake{
			Handshake: &agentv1.HandshakeResponse{Accepted: true, SessionId: fmt.Sprintf("sess-%d", n)},
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
			sess.mu.Lock()
			sess.msgs = append(sess.msgs, m)
			sess.mu.Unlock()
		}
	}()

	select {
	case <-g.closeCh:
		return nil
	case err := <-recvErr:
		return err
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
}

// forceReconnect ends whichever session is currently open, the same way a
// vanished client network would: the server side returns first, the client
// notices on its next Send or Recv and reconnects.
func (g *nodeJobReconnectGateway) forceReconnect() {
	g.closeCh <- struct{}{}
}

// session waits for the Nth (1-indexed) session to be observed and returns
// it.
func (g *nodeJobReconnectGateway) session(t *testing.T, n int) *nodeJobSession {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		ok := len(g.sessions) >= n
		var sess *nodeJobSession
		if ok {
			sess = g.sessions[n-1]
		}
		g.mu.Unlock()
		if ok {
			return sess
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session %d never observed", n)
	return nil
}

func newManagerWithReconnectableNodeJobs(t *testing.T, jobs NodeJobResponder) (*streamManager, *nodeJobReconnectGateway) {
	t.Helper()

	gw := newNodeJobReconnectGateway()
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

// A node job event emitted into a stream that dies without the manager
// noticing -- Send "succeeds" into a doomed stream, see handleNodeJob's doc
// -- is not lost: the manager retains the latest snapshot per job and
// re-sends it, oldest-updated first, the moment the next handshake is
// accepted.
func TestNodeJobSnapshotsAreReannouncedOnReconnect(t *testing.T) {
	t.Parallel()

	jobs := &fakeNodeJobs{}
	sm, gw := newManagerWithReconnectableNodeJobs(t, jobs)

	sm.handleNodeJob(&agentv1.NodeJobRequest{JobId: "j1", Node: "w1", Verb: agentv1.NodeJobVerb_NODE_JOB_VERB_DRAIN})
	sm.handleNodeJob(&agentv1.NodeJobRequest{JobId: "j2", Node: "w2", Verb: agentv1.NodeJobVerb_NODE_JOB_VERB_CORDON})

	waitFor(t, 2*time.Second, func() bool {
		jobs.mu.Lock()
		defer jobs.mu.Unlock()
		return len(jobs.emitters) == 2
	})
	jobs.mu.Lock()
	emitJ1, emitJ2 := jobs.emitters[0], jobs.emitters[1]
	jobs.mu.Unlock()

	// j1 goes RUNNING then FAILED (drain timed out); j2 goes straight to
	// SUCCEEDED. Only the latest per job should ever be re-announced.
	emitJ1(&agentv1.NodeJobEvent{JobId: "j1", Phase: agentv1.NodeJobPhase_NODE_JOB_PHASE_RUNNING})
	emitJ1(&agentv1.NodeJobEvent{JobId: "j1", Phase: agentv1.NodeJobPhase_NODE_JOB_PHASE_FAILED})
	emitJ2(&agentv1.NodeJobEvent{JobId: "j2", Phase: agentv1.NodeJobPhase_NODE_JOB_PHASE_SUCCEEDED})

	session1 := gw.session(t, 1)
	waitFor(t, 2*time.Second, func() bool { return len(session1.sent()) >= 3 })

	gw.forceReconnect()
	waitFor(t, 3*time.Second, func() bool { return sm.Connected() && sm.SessionID() == "sess-2" })

	session2 := gw.session(t, 2)
	var events []*agentv1.NodeJobEvent
	waitFor(t, 2*time.Second, func() bool {
		events = nil
		for _, m := range session2.sent() {
			if ev := m.GetNodeJobEvent(); ev != nil {
				events = append(events, ev)
			}
		}
		return len(events) == 2
	})

	if got := events[0].GetJobId(); got != "j1" {
		t.Fatalf("first re-announced job = %q, want j1 (oldest-updated first)", got)
	}
	if got := events[0].GetPhase(); got != agentv1.NodeJobPhase_NODE_JOB_PHASE_FAILED {
		t.Fatalf("j1 re-announced phase = %v, want FAILED (latest snapshot, not RUNNING)", got)
	}
	if got := events[1].GetJobId(); got != "j2" {
		t.Fatalf("second re-announced job = %q, want j2", got)
	}
	if got := events[1].GetPhase(); got != agentv1.NodeJobPhase_NODE_JOB_PHASE_SUCCEEDED {
		t.Fatalf("j2 re-announced phase = %v, want SUCCEEDED", got)
	}

	// Nothing else rode along on the second session.
	for _, m := range session2.sent() {
		if m.GetNodeJobEvent() == nil {
			t.Fatalf("session 2 received a non-node-job message: %+v", m)
		}
	}
}

// The retention sweep drops a snapshot once it is older than the retention
// window, so a job that finished long before a reconnect is not
// re-announced.
func TestNodeJobRetentionSweepDropsExpiredSnapshots(t *testing.T) {
	t.Parallel()

	sm, _ := newTestManager(t, testConfig(), newTestQueue(t), nil)

	now := time.Now()
	sm.nodeJobClock = func() time.Time { return now }
	sm.retainNodeJobEvent(&agentv1.NodeJobEvent{JobId: "old", Phase: agentv1.NodeJobPhase_NODE_JOB_PHASE_SUCCEEDED})

	// Advance the clock past the retention window and retain a second,
	// fresher job. The sweep runs on every store, so storing "new" alone
	// should evict "old" without anything reading the map in between.
	now = now.Add(sm.nodeJobRetention + time.Minute)
	sm.retainNodeJobEvent(&agentv1.NodeJobEvent{JobId: "new", Phase: agentv1.NodeJobPhase_NODE_JOB_PHASE_RUNNING})

	sm.nodeJobMu.Lock()
	_, oldStillThere := sm.nodeJobLast["old"]
	_, newThere := sm.nodeJobLast["new"]
	count := len(sm.nodeJobLast)
	sm.nodeJobMu.Unlock()

	if oldStillThere {
		t.Fatal("expired snapshot was not swept")
	}
	if !newThere {
		t.Fatal("fresh snapshot was dropped")
	}
	if count != 1 {
		t.Fatalf("retained snapshot count = %d, want 1", count)
	}
}
