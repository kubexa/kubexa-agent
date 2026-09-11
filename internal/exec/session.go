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
//
// The ring is the only replay source. After Detach returns, Output holds
// nothing; Replay is the sole source of anything produced before re-attach,
// and it also empties Output, so a transport consumes Replay(replayFrom)
// and then Output with no dedupe. Output's channel identity changes at
// those two points: fetch it after Replay, not before.
type Session struct {
	spec sessionSpec
	// target and ruleID are set by the Manager for the transport's log lines.
	target podTarget
	ruleID string

	stdinR *io.PipeReader
	stdinW *io.PipeWriter
	sizeQ  *sizeQueue

	mu         sync.Mutex
	attached   bool
	detachedAt time.Time
	// out is the live channel for the current attachment. Detach and Replay
	// retire it (close outGen, swap in an empty one) so a writer blocked on
	// the old channel moves on and nothing queued there is ever read; the
	// ring already holds every such frame.
	out          chan Frame
	outGen       chan struct{}
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
		outGen: make(chan struct{}),
		done:   make(chan struct{}),
	}
}

func (s *Session) ID() string { return s.spec.id }

// Output is the live channel of the current attachment; see the type's
// doc comment for when it is replaced.
func (s *Session) Output() <-chan Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out
}
func (s *Session) Done() <-chan struct{} { return s.done }
func (s *Session) Exit() *agentv1.ExecExit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exit
}

// Replay returns every retained frame with Seq > after, and whether older
// ones were evicted. It is the attach-time sync point: under the same lock
// the writer decides with, it retires Output, so a frame is either in the
// returned slice or will arrive on the Output fetched afterwards -- never
// both.
func (s *Session) Replay(after uint64) ([]Frame, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	frames, gap := s.spec.ring.since(after)
	s.retireOutputLocked()
	return frames, gap
}
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

// Detach starts the resume window. After it returns, Output holds nothing:
// whatever was queued for the departed transport is still in the ring, and
// Replay on re-attach is the sole source of it.
func (s *Session) Detach() {
	s.mu.Lock()
	s.attached = false
	s.detachedAt = time.Now()
	s.retireOutputLocked()
	s.mu.Unlock()
}

// retireOutputLocked swaps in an empty live channel. Closing outGen frees a
// writer blocked on the old channel; a send that still lands there goes to
// a channel nobody will ever read. Caller holds mu.
func (s *Session) retireOutputLocked() {
	close(s.outGen)
	s.out = make(chan Frame, outputQueue)
	s.outGen = make(chan struct{})
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
	defer cancel()

	// Close can run before this goroutine is scheduled (the transport may
	// Close between Open returning and here). The decision is made under
	// the lock finish sets exit under, so exactly one of two things holds:
	// finish saw this cancel and called it, or run sees exit and never
	// dials. Checking done instead would leave a window -- finish closes
	// done after releasing the lock -- in which a stream nobody cancels
	// holds the session slot and the SPDY connection until max_session_sec.
	s.mu.Lock()
	if s.exit != nil {
		s.mu.Unlock()
		return
	}
	s.cancel = cancel
	s.mu.Unlock()

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
		// The ring push and the live-or-not decision sit under one lock with
		// Replay and Detach, so each frame lands on exactly one side of
		// those sync points. Only the send itself happens outside it: it
		// may block (backpressure), and it blocks on the channel of THIS
		// attachment, which retiring releases.
		w.s.mu.Lock()
		f := w.s.spec.ring.push(w.ch, p[:n])
		attached, out, gen := w.s.attached, w.s.out, w.s.outGen
		w.s.mu.Unlock()
		if attached {
			select {
			case out <- f:
			case <-gen: // retired: the frame is reachable through Replay
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
