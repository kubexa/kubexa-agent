package exec

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/remotecommand"
	kexec "k8s.io/client-go/util/exec"

	"github.com/kubexa/kubexa-agent/internal/exec/policy"
	"github.com/kubexa/kubexa-agent/internal/k8s"
	"github.com/kubexa/kubexa-agent/pkg/config"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// execCall is one ExecSession handler invocation, driven by the test: the
// attach it saw, the frames the agent sent after it, a channel the test
// pushes gateway frames through, and two ways for it to end.
type execCall struct {
	attach    *agentv1.ExecAttach
	fromAgent chan *agentv1.ExecClientMessage
	toAgent   chan *agentv1.ExecServerMessage
	cut       chan struct{} // close to make the handler return an error mid-stream
	ended     chan struct{} // closed when the handler returns
}

// fakeExecServer answers ExecSession the way the gateway does: reads the
// attach, acks it (ackFor decides how, per call), then relays.
type fakeExecServer struct {
	agentv1.UnimplementedAgentServiceServer
	calls  chan *execCall
	n      atomic.Int32
	ackFor func(n int, a *agentv1.ExecAttach) *agentv1.ExecAttachAck
}

func (s *fakeExecServer) ExecSession(stream agentv1.AgentService_ExecSessionServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	attach := first.GetAttach()
	if attach == nil {
		return status.Error(codes.InvalidArgument, "first frame was not an attach")
	}
	n := int(s.n.Add(1))
	call := &execCall{
		attach:    attach,
		fromAgent: make(chan *agentv1.ExecClientMessage, 64),
		toAgent:   make(chan *agentv1.ExecServerMessage, 16),
		cut:       make(chan struct{}),
		ended:     make(chan struct{}),
	}
	defer close(call.ended)

	ack := &agentv1.ExecAttachAck{Accepted: true}
	if s.ackFor != nil {
		ack = s.ackFor(n, attach)
	}
	if err := stream.Send(&agentv1.ExecServerMessage{Payload: &agentv1.ExecServerMessage_Ack{Ack: ack}}); err != nil {
		return err
	}
	s.calls <- call
	if !ack.GetAccepted() {
		return nil
	}

	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			call.fromAgent <- msg
		}
	}()
	for {
		select {
		case m := <-call.toAgent:
			if err := stream.Send(m); err != nil {
				return err
			}
		case <-call.cut:
			return status.Error(codes.Unavailable, "stream cut by the test")
		case err := <-recvErr:
			if errors.Is(err, io.EOF) {
				return nil // the agent closed its send side after the exit
			}
			return err
		}
	}
}

func startExecServer(t *testing.T, srv *fakeExecServer) Dialer {
	t.Helper()
	if srv.calls == nil {
		srv.calls = make(chan *execCall, 8)
	}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		gs.Stop()
		_ = lis.Close()
	})
	return func(ctx context.Context) (agentv1.AgentService_ExecSessionClient, error) {
		return agentv1.NewAgentServiceClient(conn).ExecSession(ctx)
	}
}

func waitCall(t *testing.T, srv *fakeExecServer) *execCall {
	t.Helper()
	select {
	case c := <-srv.calls:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("the transport never dialled ExecSession")
		return nil
	}
}

func waitEnded(t *testing.T, c *execCall) {
	t.Helper()
	select {
	case <-c.ended:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream never ended")
	}
}

// nextData returns the next ExecData frame the agent sent on this call.
func nextData(t *testing.T, c *execCall) *agentv1.ExecData {
	t.Helper()
	select {
	case m := <-c.fromAgent:
		d := m.GetData()
		if d == nil {
			t.Fatalf("expected an ExecData frame, got %v", m)
		}
		return d
	case <-time.After(3 * time.Second):
		t.Fatal("no ExecData frame arrived")
		return nil
	}
}

// nextExit skips data frames and returns the ExecExit the agent sent.
func nextExit(t *testing.T, c *execCall) *agentv1.ExecExit {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-c.fromAgent:
			if e := m.GetExit(); e != nil {
				return e
			}
		case <-deadline:
			t.Fatal("no ExecExit frame arrived")
			return nil
		}
	}
}

func pushStdin(c *execCall, seq uint64, s string) {
	c.toAgent <- &agentv1.ExecServerMessage{Payload: &agentv1.ExecServerMessage_Stdin{Stdin: &agentv1.ExecData{
		Channel: agentv1.ExecChannel_EXEC_CHANNEL_STDIN, Chunk: []byte(s), Seq: seq,
	}}}
}

func devRule() []config.PodExecRule {
	return []config.PodExecRule{{Namespace: "dev", Containers: []string{"app"}}}
}

func TestTransportAttachesReplaysAndForwardsExit(t *testing.T) {
	srv := &fakeExecServer{}
	dial := startExecServer(t, srv)
	m, _ := newTestManager(t, devRule(), 4, webPod())
	tr := NewTransport(m, dial, Identity{ClusterID: "c1", TenantToken: "tok"}, nil)

	tr.Open(context.Background(), open("dev", "web-1", ""))

	call := waitCall(t, srv)
	a := call.attach
	if a.GetSessionId() != "sid" || a.GetClusterId() != "c1" || a.GetTenantToken() != "tok" || a.GetResumeSeq() != 0 {
		t.Fatalf("attach = %v, want {sid c1 tok 0}", a)
	}

	pushStdin(call, 1, "hello\n")
	d := nextData(t, call)
	if d.GetChannel() != agentv1.ExecChannel_EXEC_CHANNEL_STDOUT || d.GetSeq() != 1 || !strings.Contains(string(d.GetChunk()), "echo:hello") {
		t.Fatalf("data = %v, want STDOUT seq 1 containing echo:hello", d)
	}

	pushStdin(call, 2, "exit 3\n")
	e := nextExit(t, call)
	if e.GetCode() != 3 || e.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_UNSPECIFIED {
		t.Fatalf("exit = %v, want code 3", e)
	}
	waitEnded(t, call)
}

func TestTransportRedialsInsideResumeWindowAndReplaysFromAck(t *testing.T) {
	srv := &fakeExecServer{ackFor: func(n int, _ *agentv1.ExecAttach) *agentv1.ExecAttachAck {
		if n == 2 {
			return &agentv1.ExecAttachAck{Accepted: true, ReplayFromSeq: 1}
		}
		return &agentv1.ExecAttachAck{Accepted: true}
	}}
	dial := startExecServer(t, srv)
	m, _ := newTestManager(t, devRule(), 4, webPod())
	tr := NewTransport(m, dial, Identity{ClusterID: "c1", TenantToken: "tok"}, nil)

	tr.Open(context.Background(), open("dev", "web-1", ""))

	first := waitCall(t, srv)
	pushStdin(first, 1, "one\n")
	if d := nextData(t, first); d.GetSeq() != 1 || !strings.Contains(string(d.GetChunk()), "echo:one") {
		t.Fatalf("frame 1 = %v", d)
	}
	pushStdin(first, 2, "two\n")
	if d := nextData(t, first); d.GetSeq() != 2 || !strings.Contains(string(d.GetChunk()), "echo:two") {
		t.Fatalf("frame 2 = %v", d)
	}

	// Cut the stream with the process still running.
	close(first.cut)
	waitEnded(t, first)

	second := waitCall(t, srv)
	if second.attach.GetResumeSeq() != 2 {
		t.Fatalf("second attach resume_seq = %d, want 2 (highest stdin seq applied)", second.attach.GetResumeSeq())
	}
	// The ack said the gateway had seq 1: frame 2 is replayed first.
	if d := nextData(t, second); d.GetSeq() != 2 || !strings.Contains(string(d.GetChunk()), "echo:two") {
		t.Fatalf("replayed frame = %v, want seq 2 echo:two", d)
	}
	// ... then live output continues with the next seq.
	pushStdin(second, 3, "three\n")
	if d := nextData(t, second); d.GetSeq() != 3 || !strings.Contains(string(d.GetChunk()), "echo:three") {
		t.Fatalf("live frame after replay = %v, want seq 3 echo:three", d)
	}

	second.toAgent <- &agentv1.ExecServerMessage{Payload: &agentv1.ExecServerMessage_Close{Close: &agentv1.ExecClose{Reason: "done"}}}
	if e := nextExit(t, second); e.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED {
		t.Fatalf("exit after close = %v, want CLOSED", e)
	}
	waitEnded(t, second)
}

func TestTransportGivesUpWhenAckRejects(t *testing.T) {
	srv := &fakeExecServer{ackFor: func(int, *agentv1.ExecAttach) *agentv1.ExecAttachAck {
		return &agentv1.ExecAttachAck{Accepted: false, RejectionReason: "unknown session"}
	}}
	dial := startExecServer(t, srv)
	m, _ := newTestManager(t, devRule(), 4, webPod())
	tr := NewTransport(m, dial, Identity{ClusterID: "c1", TenantToken: "tok"}, nil)

	// Open through the manager first so the test holds the session.
	sess, refusal := m.Open(context.Background(), open("dev", "web-1", ""))
	if refusal != nil {
		t.Fatalf("refused: %v", refusal)
	}
	go tr.serve("sid", sess, nil)

	call := waitCall(t, srv)
	waitEnded(t, call)

	select {
	case <-sess.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("session did not end after the gateway rejected the attach")
	}
	e := sess.Exit()
	if e.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED || !strings.Contains(e.GetMessage(), "unknown session") {
		t.Fatalf("exit = %v, want CLOSED carrying the rejection reason", e)
	}
	select {
	case c := <-srv.calls:
		t.Fatalf("transport redialled after a rejected ack: %v", c.attach)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestTransportSendsRefusalAsExit(t *testing.T) {
	srv := &fakeExecServer{}
	dial := startExecServer(t, srv)
	// The rule permits only "app" in dev; "sidecar" is refused by policy.
	m, _ := newTestManager(t, devRule(), 4, webPod())
	tr := NewTransport(m, dial, Identity{ClusterID: "c1", TenantToken: "tok"}, nil)

	tr.Open(context.Background(), open("dev", "web-1", "sidecar"))

	call := waitCall(t, srv)
	if call.attach.GetSessionId() != "sid" || call.attach.GetResumeSeq() != 0 {
		t.Fatalf("attach = %v", call.attach)
	}
	e := nextExit(t, call)
	if e.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_POLICY_DENIED {
		t.Fatalf("exit = %v, want POLICY_DENIED", e)
	}
	waitEnded(t, call)
}

// A node open attaches BEFORE the helper is Running: the gateway's ack
// lands, then heartbeats/status lines, then -- when the wait fails -- the
// HELPER_REJECTED exit on the SAME stream. Before this change the agent
// attached only after the wait and the gateway's attach timeout ran out
// during an image pull.
func TestTransportAttachesNodeSessionBeforeTheHelperWait(t *testing.T) {
	srv := &fakeExecServer{}
	dial := startExecServer(t, srv)
	m, _, cs := newNodeTestManager(t, []string{"*"}, 1, node("n1"))
	m.opts.Node.Settings.HelperReadyTimeoutSec = 1
	cs.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		g := a.(k8stesting.GetAction)
		return true, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: g.GetName(), Namespace: g.GetNamespace()},
			Status:     corev1.PodStatus{Phase: corev1.PodPending},
		}, nil
	})
	tr := NewTransport(m, dial, Identity{ClusterID: "c1", TenantToken: "tok"}, nil)
	start := time.Now()
	tr.Open(context.Background(), nodeOpen("n1"))
	call := waitCall(t, srv)
	if took := time.Since(start); took > 900*time.Millisecond {
		t.Fatalf("attach after %s; must precede the 1 s helper wait", took)
	}
	d := nextData(t, call)
	if d.GetChannel() != agentv1.ExecChannel_EXEC_CHANNEL_STDERR || !strings.HasPrefix(string(d.GetChunk()), "kubexa: starting node shell helper") {
		t.Fatalf("first frame = %v, want the starting status line", d)
	}
	e := nextExit(t, call)
	if e.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_HELPER_REJECTED || !strings.Contains(e.GetMessage(), "not running after") {
		t.Fatalf("exit = %v", e)
	}
	waitEnded(t, call)
}

// burstThenExitExecutor reads the first line, floods stdout with more
// frames than outputQueue holds (so the pump is still draining them when
// the process ends), then reads exactly ONE byte of the next stdin frame --
// which synchronises on that frame's WriteStdin being inside the unbuffered
// pipe -- and returns exit 3 with the rest of the write still blocked
// there. finish then closes the pipe under the blocked write, so the write's
// failure and the session's end coincide by construction, while the pump is
// mid-drain and its next select sees output, Done and any stream error
// ready together.
type burstThenExitExecutor struct{ frames int }

func (burstThenExitExecutor) Stream(remotecommand.StreamOptions) error { panic("unused") }
func (e burstThenExitExecutor) StreamWithContext(_ context.Context, o remotecommand.StreamOptions) error {
	buf := make([]byte, 64)
	if _, err := o.Stdin.Read(buf); err != nil {
		return err
	}
	for i := 0; i < e.frames; i++ {
		if _, err := o.Stdout.Write([]byte("x")); err != nil {
			return err
		}
	}
	if _, err := o.Stdin.Read(buf[:1]); err != nil {
		return err
	}
	return kexec.CodeExitError{Err: io.EOF, Code: 3}
}

// TestTransportSendsExitWhenStdinWriteFailsAtSessionEnd pins that a stdin
// chunk in flight as the process dies does not cost the gateway the exit
// frame: the WriteStdin failure is the session ending, not the stream
// breaking, and must never be reported as the latter. Every output frame
// produced before the exit must precede it on the stream.
func TestTransportSendsExitWhenStdinWriteFailsAtSessionEnd(t *testing.T) {
	const burst = outputQueue + 64
	srv := &fakeExecServer{}
	dial := startExecServer(t, srv)

	c := &config.Config{}
	on := true
	c.Exec.Pod.Enabled = &on
	c.Exec.Pod.Rules = devRule()
	pol, err := policy.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{
		Policy:   pol,
		Clients:  k8s.ExecClients{Clientset: fake.NewSimpleClientset(webPod()), REST: &rest.Config{Host: "https://example"}},
		Settings: c.ExecPodSettings(),
		newExecutor: func(*rest.Config, *url.URL) (remotecommand.Executor, error) {
			return burstThenExitExecutor{frames: burst}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tr := NewTransport(m, dial, Identity{ClusterID: "c1", TenantToken: "tok"}, nil)
	tr.Open(context.Background(), open("dev", "web-1", ""))

	call := waitCall(t, srv)
	pushStdin(call, 1, "go\n")
	pushStdin(call, 2, "more\n")

	var data int
	deadline := time.After(3 * time.Second)
	for {
		select {
		case msg := <-call.fromAgent:
			if e := msg.GetExit(); e != nil {
				if e.GetCode() != 3 || e.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_UNSPECIFIED {
					t.Fatalf("exit = %v, want code 3", e)
				}
				if data != burst {
					t.Fatalf("%d output frames preceded the exit, want %d", data, burst)
				}
				waitEnded(t, call)
				return
			}
			if msg.GetData() != nil {
				data++
			}
		case <-deadline:
			t.Fatalf("no ExecExit arrived; %d output frames seen", data)
		}
	}
}

// sizeOnlyExecutor is a process blocked in `sleep`: it never reads stdin
// and only consumes terminal resizes, reporting each to the test. It ends
// when the size queue closes, which finish does.
type sizeOnlyExecutor struct {
	sizes chan remotecommand.TerminalSize
}

func (sizeOnlyExecutor) Stream(remotecommand.StreamOptions) error { panic("unused") }
func (e sizeOnlyExecutor) StreamWithContext(_ context.Context, o remotecommand.StreamOptions) error {
	for {
		sz := o.TerminalSizeQueue.Next()
		if sz == nil {
			return nil
		}
		e.sizes <- *sz
	}
}

// newManagerWith builds a Manager whose every session runs ex.
func newManagerWith(t *testing.T, ex remotecommand.Executor) *Manager {
	t.Helper()
	c := &config.Config{}
	on := true
	c.Exec.Pod.Enabled = &on
	c.Exec.Pod.Rules = devRule()
	pol, err := policy.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{
		Policy:   pol,
		Clients:  k8s.ExecClients{Clientset: fake.NewSimpleClientset(webPod()), REST: &rest.Config{Host: "https://example"}},
		Settings: c.ExecPodSettings(),
		newExecutor: func(*rest.Config, *url.URL) (remotecommand.Executor, error) {
			return ex, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func pushStdinFrame(c *execCall, seq uint64, chunk []byte) {
	c.toAgent <- &agentv1.ExecServerMessage{Payload: &agentv1.ExecServerMessage_Stdin{Stdin: &agentv1.ExecData{
		Channel: agentv1.ExecChannel_EXEC_CHANNEL_STDIN, Chunk: chunk, Seq: seq,
	}}}
}

func isHeartbeat(m *agentv1.ExecClientMessage) bool {
	d := m.GetData()
	return d != nil && d.GetSeq() == 0
}

// TestTransportIgnoresGatewayHeartbeat: the gateway heartbeats an attached
// stream with an empty, seq-0 STDIN frame every 20 s. It is not stdin: the
// process may be blocked in `sleep` and never read, and io.Pipe blocks even
// a zero-length write until the reader reads, so writing it would wedge the
// recv goroutine -- every resize and close behind it -- for as long as the
// process stays silent. It is not a seq either: resume_seq must never claim
// a heartbeat reached the process. Proven by sending the heartbeat (and the
// two half-forms: seq 0 with bytes, a seq with no bytes) ahead of a resize
// to a process that never reads stdin, and requiring the resize to reach
// the session anyway.
func TestTransportIgnoresGatewayHeartbeat(t *testing.T) {
	srv := &fakeExecServer{}
	dial := startExecServer(t, srv)
	ex := sizeOnlyExecutor{sizes: make(chan remotecommand.TerminalSize, 8)}
	m := newManagerWith(t, ex)
	tr := NewTransport(m, dial, Identity{ClusterID: "c1", TenantToken: "tok"}, nil)

	sess, refusal := m.Open(context.Background(), open("dev", "web-1", ""))
	if refusal != nil {
		t.Fatalf("refused: %v", refusal)
	}
	go tr.serve("sid", sess, nil)

	call := waitCall(t, srv)
	pushStdinFrame(call, 0, nil)         // the heartbeat
	pushStdinFrame(call, 0, []byte("x")) // bytes without a seq: not stdin either
	pushStdinFrame(call, 7, nil)         // a seq without bytes: not counted
	call.toAgent <- &agentv1.ExecServerMessage{Payload: &agentv1.ExecServerMessage_Resize{Resize: &agentv1.ExecResize{Cols: 120, Rows: 40}}}

	select {
	case sz := <-ex.sizes:
		if sz.Width != 120 || sz.Height != 40 {
			t.Fatalf("resize = %+v, want 120x40", sz)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("the resize never reached the session: the recv goroutine is wedged on the heartbeat's stdin write")
	}
	if got := sess.LastStdinSeq(); got != 0 {
		t.Fatalf("LastStdinSeq = %d, want 0: a heartbeat or an empty frame must never count as applied stdin", got)
	}

	call.toAgent <- &agentv1.ExecServerMessage{Payload: &agentv1.ExecServerMessage_Close{Close: &agentv1.ExecClose{Reason: "done"}}}
	if e := nextExit(t, call); e.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED {
		t.Fatalf("exit after close = %v, want CLOSED", e)
	}
	waitEnded(t, call)
}

// TestTransportSendsHeartbeatWhileAttached: an attached pump sends an
// empty, seq-0 STDOUT frame every heartbeat interval so nginx's
// grpc_read_timeout and Cloudflare's idle cut never see a silent stream.
// The ticker belongs to one attachment: a re-dialled stream heartbeats
// afresh, and the exit is the last frame a pump ever sends.
func TestTransportSendsHeartbeatWhileAttached(t *testing.T) {
	srv := &fakeExecServer{}
	dial := startExecServer(t, srv)
	m, _ := newTestManager(t, devRule(), 4, webPod())
	tr := NewTransport(m, dial, Identity{ClusterID: "c1", TenantToken: "tok"}, nil)
	tr.heartbeat = 50 * time.Millisecond

	tr.Open(context.Background(), open("dev", "web-1", ""))

	// awaitHeartbeats requires n heartbeat frames on c within 500 ms and
	// checks the shape of each.
	awaitHeartbeats := func(c *execCall, n int) {
		t.Helper()
		deadline := time.After(500 * time.Millisecond)
		seen := 0
		for seen < n {
			select {
			case msg := <-c.fromAgent:
				if !isHeartbeat(msg) {
					t.Fatalf("unexpected frame while the shell is silent: %v", msg)
				}
				d := msg.GetData()
				if d.GetChannel() != agentv1.ExecChannel_EXEC_CHANNEL_STDOUT || len(d.GetChunk()) != 0 {
					t.Fatalf("heartbeat = %v, want STDOUT, empty chunk, seq 0", d)
				}
				seen++
			case <-deadline:
				t.Fatalf("%d heartbeat frames within 500 ms, want at least %d", seen, n)
			}
		}
	}

	first := waitCall(t, srv)
	awaitHeartbeats(first, 2)

	// Cut the stream with the process still running: the redialled
	// attachment gets a ticker of its own.
	close(first.cut)
	waitEnded(t, first)
	second := waitCall(t, srv)
	awaitHeartbeats(second, 1)

	// The pump leaves on the session's end and its ticker with it: the exit
	// is the final frame, nothing follows it up to the stream's end.
	second.toAgent <- &agentv1.ExecServerMessage{Payload: &agentv1.ExecServerMessage_Close{Close: &agentv1.ExecClose{Reason: "done"}}}
	if e := nextExit(t, second); e.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED {
		t.Fatalf("exit after close = %v, want CLOSED", e)
	}
	waitEnded(t, second)
	for {
		select {
		case msg := <-second.fromAgent:
			t.Fatalf("frame after the exit: %v", msg)
		default:
			return
		}
	}
}

// awaitClosed fails the test unless ch closes within 3 s.
func awaitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not happen", what)
	}
}

// TestTransportDeliversExitWhenSessionEndsBetweenStreams: the stream is cut
// (Cloudflare's 150 s metronome), the re-dial fails once, and the process
// exits while the transport is waiting to try again. The gateway still
// holds the session parked and accepts the next attach, so the exit must
// arrive on it -- exactly once, with the code -- rather than the transport
// giving up because the session is over and leaving the console to wait
// out agent_gone with exit_code NULL.
func TestTransportDeliversExitWhenSessionEndsBetweenStreams(t *testing.T) {
	srv := &fakeExecServer{}
	dial := startExecServer(t, srv)
	var dials atomic.Int32
	dialFailed := make(chan struct{})
	flaky := func(ctx context.Context) (agentv1.AgentService_ExecSessionClient, error) {
		if dials.Add(1) == 2 {
			close(dialFailed)
			return nil, errors.New("gateway unreachable")
		}
		return dial(ctx)
	}
	m, _ := newTestManager(t, devRule(), 4, webPod())
	tr := NewTransport(m, flaky, Identity{ClusterID: "c1", TenantToken: "tok"}, nil)

	sess, refusal := m.Open(context.Background(), open("dev", "web-1", ""))
	if refusal != nil {
		t.Fatalf("refused: %v", refusal)
	}
	go tr.serve("sid", sess, nil)

	first := waitCall(t, srv)
	pushStdin(first, 1, "one\n")
	if d := nextData(t, first); d.GetSeq() != 1 {
		t.Fatalf("frame 1 = %v", d)
	}
	close(first.cut)
	waitEnded(t, first)
	awaitClosed(t, dialFailed, "the second dial")

	// Between streams, with the re-dial backing off: the process exits.
	if err := sess.WriteStdin([]byte("exit 3\n")); err != nil {
		t.Fatal(err)
	}
	awaitClosed(t, sess.Done(), "the session's end")

	second := waitCall(t, srv)
	if second.attach.GetResumeSeq() != 1 {
		t.Fatalf("re-attach resume_seq = %d, want 1", second.attach.GetResumeSeq())
	}
	e := nextExit(t, second)
	if e.GetCode() != 3 || e.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_UNSPECIFIED {
		t.Fatalf("exit = %v, want code 3", e)
	}
	waitEnded(t, second)
	select {
	case c := <-srv.calls:
		t.Fatalf("transport dialled again after delivering the exit: %v", c.attach)
	case <-time.After(200 * time.Millisecond):
	}
}

// brokenStream is an ExecSession stream for driving pump directly. Recv
// blocks until the test feeds recvErr; Send fails with sendErr when set.
// Only Send, Recv and CloseSend are ever called on it.
type brokenStream struct {
	grpc.ClientStream
	recvErr chan error
	sendErr error
	sent    []*agentv1.ExecClientMessage
}

func (b *brokenStream) Send(m *agentv1.ExecClientMessage) error {
	if b.sendErr != nil {
		return b.sendErr
	}
	b.sent = append(b.sent, m)
	return nil
}
func (b *brokenStream) Recv() (*agentv1.ExecServerMessage, error) { return nil, <-b.recvErr }
func (b *brokenStream) CloseSend() error                          { return nil }

func endedSession(t *testing.T) *Session {
	t.Helper()
	s, _ := startSession(t, time.Second)
	s.Attach()
	if err := s.WriteStdin([]byte("exit 3\n")); err != nil {
		t.Fatal(err)
	}
	awaitClosed(t, s.Done(), "the session's end")
	return s
}

// TestPumpReportsStreamLossWhenExitSendFails: the session is over and the
// stream breaks under the exit frame itself. pump must answer "the stream
// died" (false) so serve re-dials and the Done branch sends the exit again
// on the next stream -- not "the session is over" (true), which drops it.
func TestPumpReportsStreamLossWhenExitSendFails(t *testing.T) {
	s := endedSession(t)
	stream := &brokenStream{recvErr: make(chan error, 1), sendErr: status.Error(codes.Unavailable, "transport is closing")}
	tr := NewTransport(nil, nil, Identity{}, nil)
	if done := tr.pump(stream, s, 0); done {
		t.Fatal("pump reported the session over although the exit never left: the gateway would wait out agent_gone")
	}
	stream.recvErr <- io.EOF // release the recv goroutine
}

// TestPumpReportsStreamLossWhenStreamDiesAsSessionEnds: the stream's error
// and the session's end are ready in the same select round. Whichever
// branch wins, the exit has not been delivered on this stream, so pump
// must return false; before the fix the stream-error branch answered
// "session over" whenever Done was already closed, and the redial that
// would carry the exit never happened.
func TestPumpReportsStreamLossWhenStreamDiesAsSessionEnds(t *testing.T) {
	s := endedSession(t)
	stream := &brokenStream{recvErr: make(chan error, 1), sendErr: status.Error(codes.Unavailable, "transport is closing")}
	stream.recvErr <- status.Error(codes.Unavailable, "stream cut")
	tr := NewTransport(nil, nil, Identity{}, nil)
	if done := tr.pump(stream, s, 0); done {
		t.Fatal("pump reported the session over on a dead stream that never carried the exit")
	}
}
