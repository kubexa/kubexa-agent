package exec

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/tools/remotecommand"
	kexec "k8s.io/client-go/util/exec"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// fakeExecutor echoes stdin to stdout, line by line, until stdin closes or
// ctx ends; "exit N" ends with that code. started closes on the first
// stream; the manager tests run several sessions on one fake.
type fakeExecutor struct {
	started chan struct{}
	once    sync.Once
}

func (f *fakeExecutor) Stream(remotecommand.StreamOptions) error { panic("unused") }
func (f *fakeExecutor) StreamWithContext(ctx context.Context, o remotecommand.StreamOptions) error {
	f.once.Do(func() { close(f.started) })
	buf := make([]byte, 64)
	for {
		n, err := o.Stdin.Read(buf)
		if n > 0 {
			line := strings.TrimSpace(string(buf[:n]))
			if strings.HasPrefix(line, "exit ") {
				return kexec.CodeExitError{Err: io.EOF, Code: 3}
			}
			_, _ = o.Stdout.Write([]byte("echo:" + line + "\n"))
		}
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// errExecutor fails the stream at once, the way a refused SPDY upgrade does.
type errExecutor struct{ err error }

func (e *errExecutor) Stream(remotecommand.StreamOptions) error { panic("unused") }
func (e *errExecutor) StreamWithContext(context.Context, remotecommand.StreamOptions) error {
	return e.err
}

func startSession(t *testing.T, resume time.Duration) (*Session, *fakeExecutor) {
	t.Helper()
	fe := &fakeExecutor{started: make(chan struct{})}
	s := newSession(sessionSpec{
		id:           "s1",
		tty:          true,
		stdin:        true,
		resumeWindow: resume,
		maxSession:   time.Minute,
		ring:         newRing(RingBytes),
	})
	go s.run(context.Background(), fe)
	<-fe.started
	return s, fe
}

func collect(t *testing.T, s *Session, want string) {
	t.Helper()
	var got strings.Builder
	deadline := time.After(2 * time.Second)
	for !strings.Contains(got.String(), want) {
		select {
		case f := <-s.Output():
			got.Write(f.Chunk)
		case <-deadline:
			t.Fatalf("waiting for %q, have %q", want, got.String())
		}
	}
}

func TestSessionEchoesAndNumbersOutput(t *testing.T) {
	s, _ := startSession(t, time.Second)
	s.Attach()
	if err := s.WriteStdin([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	collect(t, s, "echo:hello")
	frames, gap := s.Replay(0)
	if gap || len(frames) == 0 || frames[0].Seq != 1 {
		t.Fatalf("replay = %v gap=%v", frames, gap)
	}
}

func TestSessionExitCodeReachesExit(t *testing.T) {
	s, _ := startSession(t, time.Second)
	s.Attach()
	_ = s.WriteStdin([]byte("exit 3\n"))
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("session did not end")
	}
	if e := s.Exit(); e.GetCode() != 3 || e.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_UNSPECIFIED {
		t.Fatalf("exit = %v", e)
	}
}

func TestSessionSurvivesDetachInsideResumeWindow(t *testing.T) {
	s, _ := startSession(t, 500*time.Millisecond)
	s.Attach()
	_ = s.WriteStdin([]byte("one\n"))
	collect(t, s, "echo:one")
	s.Detach()
	_ = s.WriteStdin([]byte("two\n")) // produced while nobody is attached
	time.Sleep(100 * time.Millisecond)
	select {
	case <-s.Done():
		t.Fatal("session ended inside its resume window")
	default:
	}
	s.Attach()
	frames, gap := s.Replay(1)
	joined := ""
	for _, f := range frames {
		joined += string(f.Chunk)
	}
	if gap || !strings.Contains(joined, "echo:two") {
		t.Fatalf("replay after re-attach = %q gap=%v", joined, gap)
	}
}

func TestSessionEndsWhenResumeWindowExpires(t *testing.T) {
	s, _ := startSession(t, 100*time.Millisecond)
	s.Attach()
	s.Detach()
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("session outlived its resume window")
	}
	if s.Exit().GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_RESUME_EXPIRED {
		t.Fatalf("reason = %v", s.Exit())
	}
}

func TestSessionCloseFromGateway(t *testing.T) {
	s, _ := startSession(t, time.Second)
	s.Attach()
	s.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "client closed")
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("close did not end the session")
	}
	if s.Exit().GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED {
		t.Fatalf("reason = %v", s.Exit())
	}
}

// A SPDY upgrade the API server refuses with 403 reaches run as a plain
// error carrying the server's own "is forbidden" text -- client-go types
// nothing on that path -- and must surface as RBAC_DENIED, not INTERNAL.
func TestSessionMapsForbiddenUpgradeToRBACDenied(t *testing.T) {
	s := newSession(sessionSpec{
		id: "s1", tty: true, resumeWindow: time.Second, maxSession: time.Minute, ring: newRing(RingBytes),
	})
	msg := `pods "x" is forbidden: cannot create resource "pods/exec"`
	go s.run(context.Background(), &errExecutor{err: errors.New(msg)})
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("session did not end")
	}
	e := s.Exit()
	if e.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_RBAC_DENIED || e.GetMessage() != msg {
		t.Fatalf("exit = %v", e)
	}
}

// The transport may forward a resize that raced the session's end. A send
// on the closed size channel would panic the whole agent.
func TestSessionResizeAfterCloseDoesNotPanic(t *testing.T) {
	s, _ := startSession(t, time.Second)
	s.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "")
	<-s.Done()
	s.Resize(80, 24)
	s.Resize(100, 40)
}

func joinChunks(frames []Frame) string {
	var b strings.Builder
	for _, f := range frames {
		b.Write(f.Chunk)
	}
	return b.String()
}

func outputIsEmpty(t *testing.T, s *Session) {
	t.Helper()
	select {
	case f := <-s.Output():
		t.Fatalf("Output() still held seq %d %q", f.Seq, f.Chunk)
	default:
	}
}

// The ring is the only replay source. Output produced while attached but
// never consumed must not survive a Detach: after re-attach the transport
// takes Replay and then Output with no dedupe.
func TestSessionDetachEmptiesOutput(t *testing.T) {
	s, _ := startSession(t, time.Second)
	s.Attach()
	_ = s.WriteStdin([]byte("one\n"))
	time.Sleep(50 * time.Millisecond)
	_ = s.WriteStdin([]byte("two\n"))
	time.Sleep(100 * time.Millisecond) // both echoes are queued in Output, unread
	s.Detach()
	outputIsEmpty(t, s)
	s.Attach()
	frames, gap := s.Replay(0)
	if gap || len(frames) != 2 || frames[0].Seq != 1 || frames[1].Seq != 2 {
		t.Fatalf("replay = %v gap=%v", frames, gap)
	}
	if got := joinChunks(frames); !strings.Contains(got, "echo:one") || !strings.Contains(got, "echo:two") {
		t.Fatalf("replay lost output: %q", got)
	}
	outputIsEmpty(t, s)
}

// Replay is the attach-time sync point: whatever it returns is gone from
// Output, and whatever lands after it arrives on Output alone.
func TestSessionReplayResetsOutput(t *testing.T) {
	s, _ := startSession(t, time.Second)
	s.Attach()
	_ = s.WriteStdin([]byte("one\n"))
	time.Sleep(100 * time.Millisecond)
	frames, gap := s.Replay(0)
	if gap || !strings.Contains(joinChunks(frames), "echo:one") {
		t.Fatalf("replay = %v gap=%v", frames, gap)
	}
	outputIsEmpty(t, s)
	_ = s.WriteStdin([]byte("two\n"))
	collect(t, s, "echo:two")
}

// A writer blocked on a full Output with nobody reading must not stall the
// process through the whole resume window: Detach releases it, and the
// frames stay reachable through Replay.
func TestSessionDetachReleasesBlockedWriter(t *testing.T) {
	s, _ := startSession(t, time.Second)
	s.Attach()
	w := &chunkWriter{s: s, ch: agentv1.ExecChannel_EXEC_CHANNEL_STDOUT}
	wrote := make(chan struct{})
	go func() {
		defer close(wrote)
		for i := 0; i < outputQueue+1; i++ {
			if _, err := w.Write([]byte("x")); err != nil {
				t.Errorf("write %d: %v", i, err)
				return
			}
		}
	}()
	select {
	case <-wrote:
		t.Fatal("writer did not block on a full Output")
	case <-time.After(100 * time.Millisecond):
	}
	s.Detach()
	select {
	case <-wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("Detach did not release the blocked writer")
	}
	if frames, gap := s.Replay(0); gap || len(frames) != outputQueue+1 {
		t.Fatalf("replay has %d frames gap=%v, want %d", len(frames), gap, outputQueue+1)
	}
	outputIsEmpty(t, s)
}

// recordingExecutor notes whether a stream was ever started.
type recordingExecutor struct {
	called chan struct{}
}

func (r *recordingExecutor) Stream(remotecommand.StreamOptions) error { panic("unused") }
func (r *recordingExecutor) StreamWithContext(ctx context.Context, _ remotecommand.StreamOptions) error {
	close(r.called)
	<-ctx.Done()
	return ctx.Err()
}

// Close can run before the Manager's run goroutine is scheduled. run must
// then never dial: a stream nobody cancels would hold the session slot and
// the SPDY connection until max_session_sec.
func TestSessionCloseBeforeRunNeverDials(t *testing.T) {
	s := newSession(sessionSpec{
		id: "s1", tty: true, resumeWindow: time.Second, maxSession: time.Minute, ring: newRing(RingBytes),
	})
	s.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "gone before start")
	rec := &recordingExecutor{called: make(chan struct{})}
	s.run(context.Background(), rec) // synchronous: returns without streaming
	select {
	case <-rec.called:
		t.Fatal("run dialled after Close")
	default:
	}
	if e := s.Exit(); e.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED || e.GetMessage() != "gone before start" {
		t.Fatalf("exit = %v", e)
	}
}

// The window the sequential test cannot reach: finish has set exit and
// released the lock but not yet closed done. run must decide on exit, under
// that lock, or it dials a stream nobody will cancel.
func TestSessionRunSeesFinishMidFlight(t *testing.T) {
	s := newSession(sessionSpec{
		id: "s1", tty: true, resumeWindow: time.Second, maxSession: time.Minute, ring: newRing(RingBytes),
	})
	s.mu.Lock()
	s.exit = refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "mid-flight") // finish, first half
	s.mu.Unlock()
	rec := &recordingExecutor{called: make(chan struct{})}
	ran := make(chan struct{})
	go func() { defer close(ran); s.run(context.Background(), rec) }()
	select {
	case <-ran:
	case <-rec.called:
		s.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "") // release the goroutine before failing
		<-ran
		t.Fatal("run dialled although finish had already recorded the exit")
	case <-time.After(2 * time.Second):
		t.Fatal("run neither returned nor dialled")
	}
	s.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "") // finish, second half
}

// The transport Detaches before its first dial, so with resume_window_sec
// at 0 ("no resume") the watchdog would end the session on its first tick,
// before the first attach -- a fresh TLS+HTTP/2 dial -- can complete. Until
// the first Attach the bound is firstAttachGrace; after it, the configured
// window rules: a cut stream on a window-0 session ends it within a tick.
func TestSessionWindowZeroWaitsForTheFirstAttach(t *testing.T) {
	s, _ := startSession(t, 0)
	s.Detach() // what serve does before the first dial
	time.Sleep(1500 * time.Millisecond)
	select {
	case <-s.Done():
		t.Fatalf("session ended before its first attach: %v", s.Exit())
	default:
	}
	s.Attach()
	s.Detach() // the stream is cut after the first attach: window 0 rules now
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a window-0 session outlived the cut of its first stream")
	}
	if s.Exit().GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_RESUME_EXPIRED {
		t.Fatalf("reason = %v, want RESUME_EXPIRED", s.Exit())
	}
}

// The pod deleted between Open's Get and the SPDY upgrade comes back the
// same way the 403 does: a plain error carrying the server's `not found`
// text. It is NOT_FOUND, not INTERNAL.
func TestSessionMapsNotFoundUpgradeToNotFound(t *testing.T) {
	s := newSession(sessionSpec{
		id: "s1", tty: true, stdin: true, resumeWindow: time.Second, maxSession: time.Minute, ring: newRing(RingBytes),
	})
	msg := `unable to upgrade connection: pods "x" not found`
	go s.run(context.Background(), &errExecutor{err: errors.New(msg)})
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("session did not end")
	}
	e := s.Exit()
	if e.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_NOT_FOUND || e.GetMessage() != msg {
		t.Fatalf("exit = %v", e)
	}
}

// A shell the image lacks is flattened by client-go into the same plain
// error shape; it must stay INTERNAL (with the message), not become "pod
// not found".
func TestSessionKeepsMissingExecutableInternal(t *testing.T) {
	s := newSession(sessionSpec{
		id: "s1", tty: true, stdin: true, resumeWindow: time.Second, maxSession: time.Minute, ring: newRing(RingBytes),
	})
	msg := `exec: "bash": executable file not found in $PATH`
	go s.run(context.Background(), &errExecutor{err: errors.New(msg)})
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("session did not end")
	}
	e := s.Exit()
	if e.GetReason() != agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL || e.GetMessage() != msg {
		t.Fatalf("exit = %v", e)
	}
}
