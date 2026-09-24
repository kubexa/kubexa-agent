package capability

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

type captureWriter struct {
	mu   sync.Mutex
	msgs []*agentv1.AgentMessage
}

func (w *captureWriter) Write(_ context.Context, msg *agentv1.AgentMessage) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.msgs = append(w.msgs, msg)
	return nil
}

func (w *captureWriter) all() []*agentv1.AgentMessage {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*agentv1.AgentMessage{}, w.msgs...)
}

// notifyingWriter is a captureWriter that also signals every Write, so a test
// can wait on the actual publish instead of sleeping a fixed duration and
// hoping one landed inside it.
type notifyingWriter struct {
	captureWriter
	published chan struct{}
}

func newNotifyingWriter() *notifyingWriter {
	return &notifyingWriter{published: make(chan struct{}, 8)}
}

func (w *notifyingWriter) Write(ctx context.Context, msg *agentv1.AgentMessage) error {
	if err := w.captureWriter.Write(ctx, msg); err != nil {
		return err
	}
	select {
	case w.published <- struct{}{}:
	default:
	}
	return nil
}

type failingWriter struct{ calls int }

func (w *failingWriter) Write(context.Context, *agentv1.AgentMessage) error {
	w.calls++
	return errors.New("queue full")
}

func caps(n int) []Capability {
	out := make([]Capability, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Capability{
			GVR:     GVR{Group: "apps", Version: "v1", Resource: "r" + string(rune('a'+i))},
			CanList: true,
		})
	}
	return out
}

// buildCatalog is the pure part of the reporter: everything about turning a
// probe result into a wire message, with no scheduling involved.
func TestBuildCatalogCarriesMillisecondsAndFailedGroups(t *testing.T) {
	at := time.Date(2026, 7, 30, 10, 7, 56, 0, time.UTC)

	cat := buildCatalog(caps(2), []string{"metrics.k8s.io/v1beta1"}, "sha256:x", at)

	if cat.GetCollectedAt() != at.UnixMilli() {
		t.Fatalf("collectedAt = %d, want %d (unix MILLIseconds, per catalog.proto)",
			cat.GetCollectedAt(), at.UnixMilli())
	}
	if cat.GetFingerprint() != "sha256:x" {
		t.Fatalf("fingerprint = %q, want sha256:x", cat.GetFingerprint())
	}
	if len(cat.GetEntries()) != 2 {
		t.Fatalf("entries = %d, want 2", len(cat.GetEntries()))
	}
	if len(cat.GetFailedGroups()) != 1 {
		t.Fatalf("failedGroups = %v, want one entry", cat.GetFailedGroups())
	}
}

// The expensive sweep must not run on every discovery tick. This is the whole
// reason the fingerprint exists.
func TestNeedsSweepSkipsWhenFingerprintUnchanged(t *testing.T) {
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	s := sweepState{lastFingerprint: "sha256:a", lastSweep: now, ran: true}

	if s.needsSweep("sha256:a", now.Add(5*time.Minute), time.Hour, false) {
		t.Fatal("needsSweep = true for an unchanged fingerprint inside the safety interval")
	}
}

func TestNeedsSweepOnFingerprintChange(t *testing.T) {
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	s := sweepState{lastFingerprint: "sha256:a", lastSweep: now}

	if !s.needsSweep("sha256:b", now.Add(time.Minute), time.Hour, false) {
		t.Fatal("needsSweep = false after the GVR set changed")
	}
}

// RBAC changes are invisible to discovery: widening a ClusterRole does not
// change which GVRs exist. Without the time-based sweep a new grant would
// never surface.
func TestNeedsSweepAfterSafetyInterval(t *testing.T) {
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	s := sweepState{lastFingerprint: "sha256:a", lastSweep: now}

	if !s.needsSweep("sha256:a", now.Add(61*time.Minute), time.Hour, false) {
		t.Fatal("needsSweep = false past the safety interval with unchanged discovery")
	}
}

// A never-run reporter always sweeps, whatever the fingerprint.
func TestNeedsSweepOnFirstRun(t *testing.T) {
	var s sweepState
	if !s.needsSweep("sha256:a", time.Now(), time.Hour, false) {
		t.Fatal("needsSweep = false on the first run")
	}
}

// The mirror of the failure case: what publish actually hands the queue. The
// catalog alone is not enough -- the server routes on the payload type and
// stamps freshness from meta.timestamp, so a catalog published without a meta
// (r.agentMeta is nil until the stream handshake fills it) must still carry
// one rather than nil-deref or arrive undatable.
func TestPublishSendsCatalogWithStampedMeta(t *testing.T) {
	w := &captureWriter{}
	r := &Reporter{writer: w}
	at := time.Date(2026, 7, 30, 10, 7, 56, 0, time.UTC)

	if err := r.publish(context.Background(), caps(1), nil, "sha256:a", at); err != nil {
		t.Fatalf("publish: %v", err)
	}

	msgs := w.all()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	msg := msgs[0]
	if msg.GetMessageId() == "" {
		t.Fatal("messageId is empty; the server dedupes on it")
	}
	if msg.GetMeta().GetTimestamp() != at.UnixMilli() {
		t.Fatalf("meta.timestamp = %d, want %d", msg.GetMeta().GetTimestamp(), at.UnixMilli())
	}
	if msg.GetCatalog().GetFingerprint() != "sha256:a" {
		t.Fatalf("payload = %T, want a catalog with fingerprint sha256:a", msg.GetPayload())
	}
}

// A catalog that never reached the queue is not a completed sweep. Recording
// one would mean no catalog at all until the hourly safety sweep -- an hour of
// an empty type list because the queue was briefly full at startup. refresh
// only advances r.state after r.publish succeeds, so drive that path directly
// through the extracted publish step rather than re-simulating it.
func TestFailedPublishLeavesSweepStateUnset(t *testing.T) {
	w := &failingWriter{}
	r := &Reporter{writer: w}

	err := r.publish(context.Background(), caps(1), nil, "sha256:a", time.Now())
	if err == nil {
		t.Fatal("expected publish to fail")
	}
	if w.calls != 1 {
		t.Fatalf("writer calls = %d, want 1", w.calls)
	}
	if r.state.ran {
		t.Fatal("state.ran = true after a failed publish; the next tick would not retry")
	}
	if !r.state.needsSweep("sha256:a", time.Now(), time.Hour, false) {
		t.Fatal("needsSweep = false after a failed publish; the catalog would be stuck until the safety sweep")
	}
}

// Both sibling collectors (state, metrics) reject a second Start rather than
// silently overwriting their cancel func -- an overwrite orphans the first
// goroutine's cancel reference, and Stop's wg.Wait can then block on a
// goroutine nothing can ever cancel. Reporter must match.
func TestStartTwiceReturnsError(t *testing.T) {
	r, err := NewReporter(Options{
		Clientset: fake.NewSimpleClientset(),
		Writer:    &captureWriter{},
	})
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := r.Start(ctx); err != nil {
		t.Fatalf("first Start: unexpected error: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		_ = r.Stop(stopCtx)
	}()

	if err := r.Start(ctx); err == nil {
		t.Fatal("second Start: expected error, got nil")
	}
}

// Discover calls discovery.ServerPreferredResources, which takes no context
// and can stall on an unhealthy aggregated APIService. Cancelling the refresh
// loop's context does not abort a call already in flight, so Stop must bound
// its wait on the context it is handed rather than blocking on wg.Wait
// indefinitely -- otherwise a stuck discovery call holds up the agent's whole
// shutdown sequence past its SLA.
func TestStopReturnsContextErrorInsteadOfBlocking(t *testing.T) {
	r := &Reporter{}
	_, cancel := context.WithCancel(context.Background())
	r.cancel = cancel

	// Stand in for a refresh goroutine stuck in an uncancellable API call.
	block := make(chan struct{})
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		<-block
	}()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stopCancel()

	err := r.Stop(stopCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop err = %v, want context.DeadlineExceeded", err)
	}

	close(block)
	r.wg.Wait()
}

// A nudge makes the next cycle sweep even though the fingerprint is unchanged
// and the interval has not elapsed -- otherwise a policy swap is invisible to
// the explorer for up to defaultSweepInterval. The discovery interval below
// is set to an hour specifically so that only the nudge, never a tick, could
// produce the second publish this test waits on.
func TestRequestSweepForcesTheNextCycle(t *testing.T) {
	w := newNotifyingWriter()
	r, err := NewReporter(Options{
		Clientset:         fake.NewSimpleClientset(),
		Writer:            w,
		DiscoveryInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		_ = r.Stop(stopCtx)
	}()

	// Wait for the initial, on-Start sweep before nudging, so the second
	// publish we wait for below is unambiguously the nudge's doing.
	select {
	case <-w.published:
	case <-time.After(2 * time.Second):
		t.Fatal("initial sweep never published")
	}

	r.RequestSweep()

	// The fingerprint has not changed and the next tick is an hour away, so
	// nothing but the nudge can produce another publish inside this
	// timeout. Waiting on the publish itself (rather than sleeping a fixed
	// duration) is what actually proves the loop woke up, not just that a
	// flag got set.
	select {
	case <-w.published:
	case <-time.After(2 * time.Second):
		t.Fatal("RequestSweep did not force a prompt sweep; the explorer would lag a policy swap for up to the discovery interval")
	}
}

// A nudge must not wedge: calling it with no cycle running (or in a burst)
// must never block, and it must never grow an unbounded backlog -- a second
// nudge arriving before the first is consumed collapses into the one sweep
// already pending, per the buffered-size-1, non-blocking send RequestSweep
// uses.
func TestRequestSweepIsIdempotentAndNonBlocking(t *testing.T) {
	r, err := NewReporter(Options{
		Clientset: fake.NewSimpleClientset(),
		Writer:    &captureWriter{},
	})
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}

	// No cycle is running (Start was never called). A burst of nudges must
	// still return immediately rather than blocking on an unread channel.
	done := make(chan struct{})
	go func() {
		r.RequestSweep()
		r.RequestSweep()
		r.RequestSweep()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RequestSweep blocked with no cycle running; a nudge must be dropped, not queued")
	}

	// However many times it was called, at most one nudge is pending: the
	// burst collapsed into a single sweep instead of an unbounded backlog.
	if n := len(r.nudge); n != 1 {
		t.Fatalf("pending nudges = %d, want 1 (a burst must collapse, not queue)", n)
	}

	<-r.nudge
	select {
	case <-r.nudge:
		t.Fatal("a second nudge was queued behind the first; RequestSweep must coalesce, not queue")
	default:
	}
}
