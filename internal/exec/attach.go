package exec

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/kubexa/kubexa-agent/internal/logger"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// Dialer opens one ExecSession stream. It is supplied by the stream package
// so this one never learns the gateway address or its credentials.
type Dialer func(ctx context.Context) (agentv1.AgentService_ExecSessionClient, error)

// Identity is what ExecAttach carries to prove who is attaching.
type Identity struct {
	ClusterID   string
	TenantToken string
}

// exitDrainTimeout bounds how long a pump waits, after sending the exit,
// for the gateway to end the stream before the attach context is cancelled
// underneath it. A well-behaved gateway ends it at once; the bound only
// keeps a silent one from pinning a goroutine and a stream forever.
const exitDrainTimeout = 5 * time.Second

// heartbeatInterval paces an empty, seq-0 STDOUT frame on an attached
// ExecSession stream. nginx's grpc_read_timeout (60s) and Cloudflare's idle
// cut both end a silent stream, and HTTP/2 PINGs do not reset nginx's
// per-stream timer, so a shell whose user is reading would otherwise be cut
// every minute and resumed. The gateway sends the same frame the other way
// (an empty seq-0 STDIN); both sides ignore seq 0 / empty on receipt -- it
// is never a ring frame, never a stdin write (io.Pipe blocks even a
// zero-length write until the process reads), never counted.
const heartbeatInterval = 20 * time.Second

// Transport turns an ExecOpen into a running session plus the stream that
// carries its bytes, and keeps re-dialing that stream while the session is
// inside its resume window.
type Transport struct {
	m     *Manager
	dial  Dialer
	ident Identity
	log   *logger.Logger
	// heartbeat overrides heartbeatInterval; zero means the const. Tests only.
	heartbeat time.Duration
}

func (t *Transport) heartbeatEvery() time.Duration {
	if t.heartbeat > 0 {
		return t.heartbeat
	}
	return heartbeatInterval
}

func NewTransport(m *Manager, dial Dialer, ident Identity, log *logger.Logger) *Transport {
	if log == nil {
		log = logger.New("exec-transport")
	}
	return &Transport{m: m, dial: dial, ident: ident, log: log}
}

// Open satisfies stream.ExecResponder. It returns immediately; the session
// runs on its own goroutines -- Manager.Open included, since it looks the
// pod up on the API server and the caller is the Connect recv loop, which
// must not wait on that round trip. ctx is the agent session context: when
// the Connect stream ends the console does NOT end with it -- the resume
// window covers a Connect reconnect too -- so ctx is used only for the
// initial Open and a detached background context drives the pumps.
func (t *Transport) Open(ctx context.Context, open *agentv1.ExecOpen) {
	ctx = context.WithoutCancel(ctx)
	go func() {
		sess, refusal := t.m.Open(ctx, open)
		t.serve(open.GetSessionId(), sess, refusal)
	}()
}

func (t *Transport) serve(id string, sess *Session, refusal *agentv1.ExecExit) {
	ctx := context.Background()
	if refusal != nil {
		// No process. Attach once so the gateway learns why, then leave.
		stream, cancel, err := t.attach(ctx, id, 0)
		if err != nil {
			t.log.Err(err).Warn("could not deliver console refusal", logger.F("session_id", id))
			return
		}
		defer cancel()
		_ = stream.Send(&agentv1.ExecClientMessage{Payload: &agentv1.ExecClientMessage_Exit{Exit: refusal}})
		_ = stream.CloseSend()
		awaitEnd(recvUntilError(stream))
		return
	}

	// The resume window starts now, not at the first Detach: a gateway the
	// agent cannot reach for the first attach must end the session the same
	// way a dropped stream does, rather than letting it run to
	// max_session_sec with the redial loop as its only bound.
	sess.Detach()

	backoff := time.Second
	// exitDeadline is zero until the session's end is first seen here, on the
	// failure path. A session that ends while the transport is between
	// streams still owes the gateway its exit: the gateway keeps the session
	// parked for its own resume window and accepts a re-attach, and pump's
	// Done branch delivers the exit on the first accepted one. So once Done
	// is closed the loop keeps re-dialing for the agent's resume window
	// (never longer than the gateway's) instead of leaving the console to
	// wait out agent_gone with the exit code lost.
	var exitDeadline time.Time
	for {
		stream, cancel, err := t.attach(ctx, id, sess.LastStdinSeq())
		if err == nil {
			var ack *agentv1.ExecAttachAck
			ack, err = recvAck(stream)
			if err == nil && !ack.GetAccepted() {
				cancel()
				reason := "gateway rejected the attach"
				if ack.GetRejectionReason() != "" {
					reason = ack.GetRejectionReason()
				}
				sess.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, reason)
				return
			}
			if err == nil {
				backoff = time.Second
				sess.Attach()
				done := t.pump(stream, sess, ack.GetReplayFromSeq())
				sess.Detach()
				cancel()
				if done {
					return
				}
				// Stream dropped: loop and re-dial. With the process still
				// running the session's own resume watchdog bounds this;
				// with the process gone, exitDeadline does.
				continue
			}
			cancel()
		}
		// Dial, attach-send or ack-recv failed: the stream never carried a
		// frame, so retry inside the resume window.
		if t.sessionOver(sess) {
			if exitDeadline.IsZero() {
				exitDeadline = time.Now().Add(sess.spec.resumeWindow)
			} else if time.Now().After(exitDeadline) {
				t.log.Err(err).Warn("console exit not delivered; the gateway stayed unreachable for the resume window",
					logger.F("session_id", id), logger.F("reason", sess.Exit().GetReason().String()))
				return
			}
		}
		t.log.Err(err).Warn("console attach failed; retrying inside the resume window",
			logger.F("session_id", id))
		if exitDeadline.IsZero() {
			if !sleepUnless(sess.Done(), backoff) {
				// The session ended during the wait: re-dial at once to
				// deliver its exit, and start the bounded window now.
				exitDeadline = time.Now().Add(sess.spec.resumeWindow)
				continue
			}
		} else {
			time.Sleep(backoff)
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

// attach dials one stream under a context of its own and sends the attach
// frame. The returned cancel ends that stream -- and the recv goroutine
// pump leaves on it -- once the caller is done with it.
func (t *Transport) attach(ctx context.Context, id string, stdinSeq uint64) (agentv1.AgentService_ExecSessionClient, context.CancelFunc, error) {
	sctx, cancel := context.WithCancel(ctx)
	stream, err := t.dial(sctx)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	err = stream.Send(&agentv1.ExecClientMessage{Payload: &agentv1.ExecClientMessage_Attach{Attach: &agentv1.ExecAttach{
		SessionId: id, ClusterId: t.ident.ClusterID, TenantToken: t.ident.TenantToken, ResumeSeq: stdinSeq,
	}}})
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return stream, cancel, nil
}

func recvAck(stream agentv1.AgentService_ExecSessionClient) (*agentv1.ExecAttachAck, error) {
	msg, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	ack := msg.GetAck()
	if ack == nil {
		return nil, errors.New("first gateway frame was not an ack")
	}
	return ack, nil
}

// pump runs one attached stream: replay, then live output out and stdin/
// resize/close in. It returns true when the SESSION is over (exit sent or
// close received) and false when only the STREAM died.
//
// Session protocol: Attach (done by the caller), Replay, then Output. Replay
// retires the live channel, so Output is fetched after it and read for the
// life of this one attachment; the channel goes silent -- never closed --
// when the caller Detaches, which is why every wait here also watches Done
// and the stream.
func (t *Transport) pump(stream agentv1.AgentService_ExecSessionClient, sess *Session, replayFrom uint64) bool {
	frames, gap := sess.Replay(replayFrom)
	if gap {
		t.log.Warn("console replay has a gap; the ring buffer was overrun",
			logger.F("session_id", sess.ID()), logger.F("replay_from", replayFrom))
	}
	for _, f := range frames {
		if err := sendData(stream, f); err != nil {
			return false
		}
	}
	out := sess.Output()

	// streamErr carries stream.Recv errors ONLY. A WriteStdin failure is
	// the session ending (finish closed the pipe, or done is closed), never
	// the stream breaking; reporting it here would race the Done branch
	// below, and losing that race drops the exit frame.
	streamErr := make(chan error, 1)
	go func() {
		stdinClosed := false
		for {
			msg, err := stream.Recv()
			if err != nil {
				streamErr <- err
				return
			}
			switch p := msg.GetPayload().(type) {
			case *agentv1.ExecServerMessage_Stdin:
				if p.Stdin.GetSeq() == 0 || len(p.Stdin.GetChunk()) == 0 {
					continue // gateway heartbeat: not stdin, not a seq (see heartbeatInterval)
				}
				if stdinClosed {
					continue
				}
				if err := sess.WriteStdin(p.Stdin.GetChunk()); err != nil {
					// The session is over, or was opened without stdin;
					// keep consuming so the Done branch still sees the
					// gateway end the stream, but write nothing more.
					stdinClosed = true
					continue
				}
				// Noted only once applied: resume_seq promises the gateway
				// that everything up to it reached the process.
				sess.NoteStdinSeq(p.Stdin.GetSeq())
			case *agentv1.ExecServerMessage_Resize:
				sess.Resize(p.Resize.GetCols(), p.Resize.GetRows())
			case *agentv1.ExecServerMessage_Close:
				sess.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, p.Close.GetReason())
			}
		}
	}()

	// The heartbeat shares this goroutine with the output frames: one stream
	// never has two concurrent senders. Stopped when the pump leaves.
	beat := time.NewTicker(t.heartbeatEvery())
	defer beat.Stop()

	for {
		select {
		case f := <-out:
			if err := sendData(stream, f); err != nil {
				return false
			}
		case <-beat.C:
			if err := sendData(stream, Frame{Channel: agentv1.ExecChannel_EXEC_CHANNEL_STDOUT}); err != nil {
				return false
			}
		case <-sess.Done():
			// Drain what is still queued, then the exit. A drain failure is
			// tolerated (the frames stay in the ring for the replay), but
			// the exit itself must reach the gateway: if its Send fails
			// the stream is gone and only a re-dial can carry it, so this
			// is reported as the STREAM dying, not the session ending.
		drain:
			for {
				select {
				case f := <-out:
					_ = sendData(stream, f)
				default:
					break drain
				}
			}
			if err := stream.Send(&agentv1.ExecClientMessage{Payload: &agentv1.ExecClientMessage_Exit{Exit: sess.Exit()}}); err != nil {
				return false
			}
			_ = stream.CloseSend()
			// Let the gateway end the stream so the exit is on the wire
			// before the caller cancels the attach context under it.
			awaitEnd(streamErr)
			return true
		case err := <-streamErr:
			if errors.Is(err, io.EOF) {
				// The gateway closed its send side: treat as a close.
				sess.Close(agentv1.ExecExitReason_EXEC_EXIT_REASON_CLOSED, "gateway closed the stream")
				return true
			}
			// The stream died. Even when the session is over in the same
			// select round the exit has not been sent on this stream, so
			// the caller must re-dial and let the Done branch deliver it.
			return false
		}
	}
}

func sendData(stream agentv1.AgentService_ExecSessionClient, f Frame) error {
	return stream.Send(&agentv1.ExecClientMessage{Payload: &agentv1.ExecClientMessage_Data{Data: &agentv1.ExecData{
		Channel: f.Channel, Chunk: f.Chunk, Seq: f.Seq,
	}}})
}

// recvUntilError reads and discards frames until the stream ends, reporting
// the end on the returned channel. Used where nothing on the stream matters
// any more but its end does.
func recvUntilError(stream agentv1.AgentService_ExecSessionClient) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				errCh <- err
				return
			}
		}
	}()
	return errCh
}

// awaitEnd waits for the stream's end, bounded by exitDrainTimeout.
func awaitEnd(streamErr <-chan error) {
	select {
	case <-streamErr:
	case <-time.After(exitDrainTimeout):
	}
}

func (t *Transport) sessionOver(sess *Session) bool {
	select {
	case <-sess.Done():
		return true
	default:
		return false
	}
}

func sleepUnless(done <-chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return false
	case <-time.After(d):
		return true
	}
}
