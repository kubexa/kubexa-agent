package exec

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/tools/remotecommand"
	kexec "k8s.io/client-go/util/exec"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

const (
	// MaxChunkBytes bounds one output frame. grpclimits.MaxExecFrameBytes on
	// the gateway is 64 KiB; half of it leaves the envelope and any
	// framing well clear of the ceiling.
	MaxChunkBytes = 32 << 10
	// RingBytes is how much recent output a session keeps for replay.
	RingBytes = 256 << 10
	// outputQueue is how many live frames may wait for the transport before
	// the process's stdout blocks. Backpressure, not a drop.
	outputQueue = 256
)

type sessionSpec struct {
	id           string
	tty          bool
	resumeWindow time.Duration
	maxSession   time.Duration
	ring         *ring
}

// Session is one running console. The transport (attach.go) Attaches to
// consume Output, Detaches when its stream drops, and the session keeps
// running for resumeWindow with nobody attached.
type Session struct {
	spec sessionSpec
	// target and ruleID are set by the Manager for the transport's log lines.
	target podTarget
	ruleID string

	stdinR *io.PipeReader
	stdinW *io.PipeWriter
	sizeQ  *sizeQueue
	out    chan Frame

	mu           sync.Mutex
	attached     bool
	detachedAt   time.Time
	lastStdinSeq uint64

	done     chan struct{}
	doneOnce sync.Once
	exit     *agentv1.ExecExit
	cancel   context.CancelFunc
}

func newSession(spec sessionSpec) *Session {
	pr, pw := io.Pipe()
	return &Session{
		spec:   spec,
		stdinR: pr,
		stdinW: pw,
		sizeQ:  newSizeQueue(),
		out:    make(chan Frame, outputQueue),
		done:   make(chan struct{}),
	}
}

func (s *Session) ID() string            { return s.spec.id }
func (s *Session) Output() <-chan Frame  { return s.out }
func (s *Session) Done() <-chan struct{} { return s.done }
func (s *Session) Exit() *agentv1.ExecExit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exit
}
func (s *Session) Replay(after uint64) ([]Frame, bool) { return s.spec.ring.since(after) }
func (s *Session) LastStdinSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastStdinSeq
}
func (s *Session) NoteStdinSeq(seq uint64) {
	s.mu.Lock()
	if seq > s.lastStdinSeq {
		s.lastStdinSeq = seq
	}
	s.mu.Unlock()
}

// Attach marks a transport as present. Output frames are only queued while
// attached; while detached they go to the ring alone.
func (s *Session) Attach() {
	s.mu.Lock()
	s.attached = true
	s.detachedAt = time.Time{}
	s.mu.Unlock()
}

// Detach starts the resume window.
func (s *Session) Detach() {
	s.mu.Lock()
	s.attached = false
	s.detachedAt = time.Now()
	s.mu.Unlock()
}

func (s *Session) WriteStdin(b []byte) error {
	select {
	case <-s.done:
		return errors.New("session ended")
	default:
	}
	_, err := s.stdinW.Write(b)
	return err
}

func (s *Session) Resize(cols, rows int32) {
	s.sizeQ.push(remotecommand.TerminalSize{Width: uint16(cols), Height: uint16(rows)})
}

// Close ends the session with a reason that is not a process exit.
func (s *Session) Close(reason agentv1.ExecExitReason, msg string) {
	s.finish(&agentv1.ExecExit{Code: -1, Reason: reason, Message: msg})
}

func (s *Session) finish(e *agentv1.ExecExit) {
	s.doneOnce.Do(func() {
		s.mu.Lock()
		s.exit = e
		cancel := s.cancel
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		_ = s.stdinW.Close()
		s.sizeQ.close()
		close(s.done)
	})
}

// run drives the remote process and returns when it ends. It owns the
// resume-window and max-session timers.
func (s *Session) run(ctx context.Context, ex remotecommand.Executor) {
	ctx, cancel := context.WithTimeout(ctx, s.spec.maxSession)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
	defer cancel()

	// Closed before the process started (the transport can Close between
	// Open returning and this goroutine being scheduled): finish already
	// recorded the reason and found no cancel to call, so do not dial.
	select {
	case <-s.done:
		return
	default:
	}

	// Resume-window watchdog.
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-t.C:
				s.mu.Lock()
				expired := !s.attached && !s.detachedAt.IsZero() &&
					time.Since(s.detachedAt) > s.spec.resumeWindow
				s.mu.Unlock()
				if expired {
					s.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_RESUME_EXPIRED,
						"no stream re-attached inside the resume window")
					return
				}
			}
		}
	}()

	opts := remotecommand.StreamOptions{
		Stdin:  s.stdinR,
		Stdout: &chunkWriter{s: s, ch: agentv1.ExecChannel_EXEC_CHANNEL_STDOUT},
		Stderr: &chunkWriter{s: s, ch: agentv1.ExecChannel_EXEC_CHANNEL_STDERR},
		Tty:    s.spec.tty,
	}
	if s.spec.tty {
		opts.TerminalSizeQueue = s.sizeQ
	}
	err := ex.StreamWithContext(ctx, opts)

	switch {
	case err == nil:
		s.finish(&agentv1.ExecExit{Code: 0})
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil:
		s.finish(&agentv1.ExecExit{Code: -1,
			Reason: agentv1.ExecExitReason_EXEC_EXIT_REASON_MAX_SESSION, Message: "max_session_sec reached"})
	default:
		// A SPDY upgrade the API server refuses with 403 comes back as a
		// plain error carrying the server's own text ("... is forbidden:
		// User ... cannot create resource "pods/exec""); client-go types
		// nothing on that path, so the text is all there is to go on.
		if strings.Contains(err.Error(), "Forbidden") || strings.Contains(err.Error(), "forbidden") {
			s.finish(refuse(agentv1.ExecExitReason_EXEC_EXIT_REASON_RBAC_DENIED, err.Error()))
			return
		}
		var ce kexec.CodeExitError
		if errors.As(err, &ce) {
			s.finish(&agentv1.ExecExit{Code: int32(ce.Code)})
			return
		}
		// finish is a no-op if Close already ran (CLOSED / RESUME_EXPIRED).
		s.finish(&agentv1.ExecExit{Code: -1,
			Reason: agentv1.ExecExitReason_EXEC_EXIT_REASON_INTERNAL, Message: err.Error()})
	}
}

// chunkWriter turns process output into ring frames and, while attached,
// live frames. Chunks are capped at MaxChunkBytes.
type chunkWriter struct {
	s  *Session
	ch agentv1.ExecChannel
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		n := len(p)
		if n > MaxChunkBytes {
			n = MaxChunkBytes
		}
		f := w.s.spec.ring.push(w.ch, p[:n])
		w.s.mu.Lock()
		attached := w.s.attached
		w.s.mu.Unlock()
		if attached {
			select {
			case w.s.out <- f:
			case <-w.s.done:
				return total, io.ErrClosedPipe
			}
		}
		p = p[n:]
	}
	return total, nil
}

// sizeQueue feeds terminal resizes to remotecommand. Next blocks until a size
// arrives or the queue closes (nil ends the resize goroutine cleanly). push
// after close is a no-op rather than a panic: the transport may forward a
// resize that raced the session's end.
type sizeQueue struct {
	mu     sync.Mutex
	ch     chan remotecommand.TerminalSize
	closed bool
}

func newSizeQueue() *sizeQueue { return &sizeQueue{ch: make(chan remotecommand.TerminalSize, 8)} }

func (q *sizeQueue) push(sz remotecommand.TerminalSize) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	select {
	case q.ch <- sz:
	default: // a burst of resizes: keep the newest, drop the oldest
		select {
		case <-q.ch:
		default:
		}
		q.ch <- sz
	}
}

func (q *sizeQueue) Next() *remotecommand.TerminalSize {
	sz, ok := <-q.ch
	if !ok {
		return nil
	}
	return &sz
}

func (q *sizeQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		close(q.ch)
	}
}
