package logs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubexa/kubexa-agent/internal/ingestrules"
	"github.com/kubexa/kubexa-agent/internal/logger"
	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
	commonv1 "github.com/kubexa/kubexa-agent/proto/gen/go/common/v1"
)

func TestStreamCursor_markDelivered(t *testing.T) {
	t.Parallel()

	cur := &streamCursor{}
	ts1 := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	ts2 := time.Date(2024, 6, 1, 12, 0, 1, 0, time.UTC)
	tsOlder := time.Date(2024, 6, 1, 11, 59, 0, 0, time.UTC)

	cur.markDelivered(deliveredLine(ts1, "one"))
	cur.markDelivered(deliveredLine(tsOlder, "older"))
	cur.markDelivered(deliveredLine(ts2, "two"))

	since, ok := cur.sinceForReconnect(reconnectSinceOverlap)
	if !ok {
		t.Fatal("sinceForReconnect() = false, want true")
	}
	want := ts2.Add(-reconnectSinceOverlap)
	if !since.Equal(want) {
		t.Fatalf("since = %v, want %v", since, want)
	}
}

func TestStreamCursor_sinceForReconnect_noProgress(t *testing.T) {
	t.Parallel()

	cur := &streamCursor{}
	if _, ok := cur.sinceForReconnect(reconnectSinceOverlap); ok {
		t.Fatal("sinceForReconnect() = true before any lines, want false")
	}
}

func TestStreamCursor_sinceForReconnect_zeroOverlap(t *testing.T) {
	t.Parallel()

	cur := &streamCursor{}
	ts := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	cur.markDelivered(deliveredLine(ts, "line"))

	since, ok := cur.sinceForReconnect(0)
	if !ok {
		t.Fatal("sinceForReconnect() = false, want true")
	}
	if !since.Equal(ts) {
		t.Fatalf("since = %v, want %v", since, ts)
	}
}

func TestStreamCursor_nilSafe(t *testing.T) {
	t.Parallel()

	var cur *streamCursor
	cur.markDelivered(deliveredLine(time.Now(), "line"))
	if cur.alreadyDelivered(deliveredLine(time.Now(), "line")) {
		t.Fatal("nil cursor should not claim a record was delivered")
	}
	if _, ok := cur.sinceForReconnect(time.Second); ok {
		t.Fatal("nil cursor should not report resume point")
	}
}

func TestHandleLogLinePopulatesWorkload(t *testing.T) {
	ctrl := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "api-1", Namespace: "stage",
		OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db", Controller: &ctrl}},
	}, Spec: corev1.PodSpec{NodeName: "node-2"}}

	entry := buildLogEntry(streamTarget{pod: pod, container: "app"},
		ParsedLine{Message: "hi", Raw: []byte("hi"), Timestamp: time.Unix(0, 7)},
		workloadRef{Name: "db", Kind: "StatefulSet"})

	if entry.GetWorkload() != "db" || entry.GetWorkloadKind() != "StatefulSet" {
		t.Fatalf("workload = %q/%q", entry.GetWorkloadKind(), entry.GetWorkload())
	}
	if entry.GetNodeName() != "node-2" {
		t.Fatalf("node = %q", entry.GetNodeName())
	}
}

// recordingWriter is a test double for Writer that captures the entries it
// was handed and whether the context it received was already Done at call
// time — the exact thing a reused, canceled stream context would look like.
type recordingWriter struct {
	entries    []*agentv1.LogEntry
	sawDoneCtx bool
	// batchSizes is the entry count of each message, in order. The entries
	// field above FLATTENS every batch into one list, which is convenient and
	// is also why nothing in this package could see how many entries a single
	// message carried -- a collector that packed a hundred entries into one
	// LogBatch would have left every test here green. See
	// TestEachLogMessageCarriesExactlyOneEntry.
	batchSizes []int
}

func (w *recordingWriter) Write(ctx context.Context, msg *agentv1.AgentMessage) error {
	if ctx.Err() != nil {
		w.sawDoneCtx = true
	}
	if msg.GetLogs() != nil {
		w.batchSizes = append(w.batchSizes, len(msg.GetLogs().GetEntries()))
	}
	w.entries = append(w.entries, msg.GetLogs().GetEntries()...)
	return nil
}

// A stream tears down on more than reconnect: pod deletion, rule removal,
// and agent shutdown all cancel the stream's context with no further chance
// to resend. If the deferred flush reused that context, the record the
// joiner is still holding — often the tail of a multi-line stack trace
// inside the hold window — would be dropped instead of reaching the writer.
func TestFlushJoinerDeliversAPendingRecordAfterTheStreamContextIsCanceled(t *testing.T) {
	m, err := newMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("newMetrics: %v", err)
	}

	w := &recordingWriter{}
	c := &Collector{
		writer:       w,
		log:          logger.New("test"),
		metrics:      m,
		agentMeta:    &commonv1.AgentMetadata{},
		workloads:    newWorkloadResolver(fake.NewSimpleClientset(), time.Minute),
		writeTimeout: 50 * time.Millisecond,
	}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "stage"}}
	target := streamTarget{pod: pod, container: "app"}

	join := newJoiner(maxJoinedBytes, maxJoinHold)
	join.Add(ParsedLine{Message: "panic: boom", Raw: []byte("panic: boom"), Timestamp: time.Unix(0, 1)}, time.Now())

	streamCtx, cancel := context.WithCancel(context.Background())
	cancel() // teardown: the stream's own context is already Done.

	c.flushJoiner(streamCtx, c.log, target, join, &streamCursor{})

	if len(w.entries) != 1 {
		t.Fatalf("writes = %d, want 1 (pending record must still reach the writer)", len(w.entries))
	}
	if w.entries[0].GetMessage() != "panic: boom" {
		t.Fatalf("message = %q", w.entries[0].GetMessage())
	}
	if w.sawDoneCtx {
		t.Fatal("flushJoiner handed the writer a context that was already Done; it must detach from the stream context")
	}
}

// The wire entry carries two payload fields, Raw and Message, and the
// consumer indexes Message. handleLogLine's cut reached only Raw, so a line
// longer than max_line_bytes shipped with Message at full length. Observed
// live on 2026-08-19: the gateway logged message_bytes 262177 against a
// negotiated max_line_bytes of 262144 -- the reader's cap
// (max_line_bytes + maxK8sLogPrefixBytes) less the 31-byte RFC3339Nano
// prefix, to the byte.
func TestTheCutReachesMessageAndNotOnlyRaw(t *testing.T) {
	const limit = 128

	m, err := newMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("newMetrics: %v", err)
	}
	rules := ingestrules.NewStore()
	rules.Set(ingestrules.Rules{MaxLineBytes: limit})

	w := &recordingWriter{}
	c := &Collector{
		writer:       w,
		log:          logger.New("test"),
		metrics:      m,
		agentMeta:    &commonv1.AgentMetadata{},
		workloads:    newWorkloadResolver(fake.NewSimpleClientset(), time.Minute),
		rules:        rules,
		limiter:      ingestrules.NewLimiter(),
		counters:     ingestrules.NewCounters(),
		writeTimeout: 50 * time.Millisecond,
	}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "report-1", Namespace: "e12"}}
	target := streamTarget{pod: pod, container: "app"}

	// A plain (non-JSON) line: ParseLine leaves Message and Raw holding the
	// same bytes, which is exactly the shape the live 262 KB SQL lines had.
	payload := strings.Repeat("s", limit+49)
	parsed := ParseLine(payload, time.Unix(0, 1))

	if err := c.handleLogLine(context.Background(), target, parsed); err != nil {
		t.Fatalf("handleLogLine: %v", err)
	}
	if len(w.entries) != 1 {
		t.Fatalf("writes = %d, want 1", len(w.entries))
	}
	entry := w.entries[0]
	if got := len(entry.GetRaw()); got > limit {
		t.Errorf("Raw = %d bytes, want <= %d", got, limit)
	}
	if got := len(entry.GetMessage()); got > limit {
		t.Errorf("Message = %d bytes, want <= %d -- the cut never reached Message", got, limit)
	}
	if !entry.GetTruncated() {
		t.Error("Truncated = false, want true")
	}
}

// TestResumeSinceClampsToTheAgeFloor pins the fix for a four-hour loop measured
// on apexcrawl between 2026-10-11 and 2026-11-07.
//
// kubelet closes an idle log stream at streamingConnectionIdleTimeout, 4h by
// default, and every stream opened at agent start goes idle together -- so the
// quiet streams reconnect as one synchronized cohort every four hours, forever.
// On each reconnect sinceForReconnect asks the API for logs since the last line
// it processed. For a pod that has been silent LONGER than max_sample_age that
// timestamp is one the drain will certainly reject: the agent re-reads a line
// it delivered a week ago, enqueues it, drops it at ageDropReason, and reports
// the drop to the platform as data loss. Measured: 10 batches per tick, 88
// KubexaAgentDropsTooOld notifications in four weeks, not one of them real --
// and that noise is what the genuine 9.6-day outage had to compete with.
//
// Clamping costs nothing that could have been ingested, because a line older
// than the floor is one ageDropReason would discard anyway. It also does not
// suppress what the counter exists for: records that expire while sitting in
// the WAL still age out there and still count.
func TestResumeSinceClampsToTheAgeFloor(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0).UTC()
	const maxSampleAge = 168 * time.Hour // the deployed value on apexcrawl

	cases := []struct {
		name         string
		checkpoint   time.Time
		maxSampleAge time.Duration
		want         time.Time
	}{
		{
			// The apexcrawl case: a pod silent for eight days.
			name:         "a checkpoint older than the floor is clamped",
			checkpoint:   now.Add(-8 * 24 * time.Hour),
			maxSampleAge: maxSampleAge,
			want:         now.Add(-maxSampleAge),
		},
		{
			name:         "a checkpoint inside the window is left alone",
			checkpoint:   now.Add(-2 * time.Hour),
			maxSampleAge: maxSampleAge,
			want:         now.Add(-2 * time.Hour),
		},
		{
			// Exactly at the floor is kept by ageDropReason (it drops on
			// strictly greater), so it must not be moved.
			name:         "a checkpoint exactly at the floor is left alone",
			checkpoint:   now.Add(-maxSampleAge),
			maxSampleAge: maxSampleAge,
			want:         now.Add(-maxSampleAge),
		},
		{
			// Same contract as TestQueuedMessageAgeFilterOffByDefault: with no
			// rule pushed, nothing about age may change behaviour.
			name:         "no rule pushed means no floor",
			checkpoint:   now.Add(-100 * 24 * time.Hour),
			maxSampleAge: 0,
			want:         now.Add(-100 * 24 * time.Hour),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resumeSince(tc.checkpoint, tc.maxSampleAge, now)
			if !got.Equal(tc.want) {
				t.Fatalf("resumeSince = %s, want %s", got.Format(time.RFC3339Nano), tc.want.Format(time.RFC3339Nano))
			}
		})
	}
}

// TestEachLogMessageCarriesExactlyOneEntry pins an invariant this package does
// not enforce and another package silently depends on.
//
// internal/stream's ageDropReason judges a queued log batch by its OLDEST
// entry and drops the batch WHOLE, and its own comment justifies that with
// "the collection path writes one entry per message, so in practice that is
// the same thing". That is true -- and it is true only here, at the one place
// a LogBatch is built. The moment this collector packs several entries into
// one message, whole-batch dropping stops being equivalent and becomes real
// loss: a single stale line, say a duplicate replayed after a reconnect, would
// take every fresh line beside it with it, and the agent would report the
// discard as expected behaviour.
//
// So the invariant is pinned on the producing side, which is the only side
// that can break it, with the consequence written down. If batching ever
// becomes worth doing, ageDropReason must become per-entry FIRST, and this
// test is where that ordering is recorded.
func TestEachLogMessageCarriesExactlyOneEntry(t *testing.T) {
	m, err := newMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("newMetrics: %v", err)
	}
	w := &recordingWriter{}
	c := &Collector{
		writer:       w,
		log:          logger.New("test"),
		metrics:      m,
		agentMeta:    &commonv1.AgentMetadata{},
		workloads:    newWorkloadResolver(fake.NewSimpleClientset(), time.Minute),
		writeTimeout: 50 * time.Millisecond,
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "stage"}}
	target := streamTarget{pod: pod, container: "app"}

	// Two separate records, so the assertion is about a PROPERTY of every
	// message rather than about the one message a single-write test produces.
	for i, line := range []string{"first line", "second line"} {
		join := newJoiner(maxJoinedBytes, maxJoinHold)
		join.Add(ParsedLine{Message: line, Raw: []byte(line), Timestamp: time.Unix(0, int64(i+1))}, time.Now())
		c.flushJoiner(context.Background(), c.log, target, join, &streamCursor{})
	}

	if len(w.batchSizes) != 2 {
		t.Fatalf("messages written = %d, want 2; got batch sizes %v", len(w.batchSizes), w.batchSizes)
	}
	for i, n := range w.batchSizes {
		if n != 1 {
			t.Errorf("message %d carries %d entries, want exactly 1. internal/stream's ageDropReason drops a log batch WHOLE on its oldest entry and is only safe while this is 1; make it per-entry before batching here.", i, n)
		}
	}
}

// TestPlanLogReadFloorsTheFreshStart is the half resumeSince never covered.
//
// streamCursor is in-memory only, so EVERY agent start takes this branch for
// every stream: the pod keeps its undelivered queue on a PVC and forgets where
// it was reading. Without a floor, the read asks for each container's last
// tailLines lines with no lower bound on age, and for a pod quiet longer than
// max_sample_age all of them are past the floor -- read, enqueued, discarded at
// ageDropReason, and reported to the platform as data loss. Measured on
// apexcrawl 2026-11-08: five such drops within three minutes of a restart.
func TestPlanLogReadFloorsTheFreshStart(t *testing.T) {
	target := streamTarget{
		pod:       &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "quiet", Namespace: "prod"}},
		container: "app",
		rule:      ruleView{follow: true, tailLines: 100},
	}
	plan := planLogRead(target, &streamCursor{}, 168*time.Hour, time.Now())

	if plan.opts.Since != 168*time.Hour {
		t.Errorf("Since = %v, want 168h: without it a restart re-reads lines the age filter will certainly discard and reports them as loss", plan.opts.Since)
	}
	// TailLines must survive alongside it. The API applies both bounds, and
	// dropping it would turn a bounded read into "everything in the window".
	if plan.opts.TailLines != 100 {
		t.Errorf("TailLines = %d, want 100", plan.opts.TailLines)
	}
	if !plan.opts.SinceTime.IsZero() {
		t.Errorf("SinceTime = %v, want zero on a fresh start (Since is relative and resolved at request time)", plan.opts.SinceTime)
	}
}

// TestPlanLogReadLeavesAFreshStartAloneWithoutRules keeps the age floor from
// inventing a bound nobody asked for. A zero max_sample_age means no ingest
// rule has been pushed, and then nothing about age may change behaviour --
// the same contract resumeSince and ageDropReason both keep.
func TestPlanLogReadLeavesAFreshStartAloneWithoutRules(t *testing.T) {
	target := streamTarget{
		pod:       &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "n"}},
		container: "app",
		rule:      ruleView{tailLines: 50},
	}
	plan := planLogRead(target, &streamCursor{}, 0, time.Now())
	if plan.opts.Since != 0 {
		t.Fatalf("Since = %v, want 0 when no rule sets max_sample_age", plan.opts.Since)
	}
	if plan.opts.TailLines != 50 {
		t.Fatalf("TailLines = %d, want 50", plan.opts.TailLines)
	}
}

// TestPlanLogReadResumesFromTheCheckpoint is the reconnect branch, kept here so
// the two branches are asserted side by side -- the whole reason planLogRead is
// one function.
func TestPlanLogReadResumesFromTheCheckpoint(t *testing.T) {
	now := time.Now()
	cur := &streamCursor{}
	cur.lastProcessed = now.Add(-time.Minute)

	target := streamTarget{
		pod:       &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "n"}},
		container: "app",
		rule:      ruleView{tailLines: 100},
	}
	plan := planLogRead(target, cur, 168*time.Hour, now)

	if plan.opts.SinceTime.IsZero() {
		t.Fatal("a cursor with progress must resume from its checkpoint, not re-read the tail")
	}
	if plan.opts.TailLines != 0 {
		t.Errorf("TailLines = %d, want 0 on a resume: asking for both would re-read the tail as well", plan.opts.TailLines)
	}
	if !plan.clampedFrom.IsZero() {
		t.Errorf("clampedFrom = %v, want zero: a one-minute-old checkpoint is nowhere near the floor", plan.clampedFrom)
	}
}

// TestPlanLogReadClampsAnExpiredCheckpoint is the case agent v0.11.3 shipped
// for: the four-hourly re-read. It belongs next to the three above because the
// clamp and the fresh-start floor are now the same decision taken twice.
func TestPlanLogReadClampsAnExpiredCheckpoint(t *testing.T) {
	now := time.Now()
	cur := &streamCursor{}
	cur.lastProcessed = now.Add(-300 * time.Hour) // a pod silent far longer than the floor

	target := streamTarget{
		pod:       &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "n"}},
		container: "app",
		rule:      ruleView{tailLines: 100},
	}
	plan := planLogRead(target, cur, 168*time.Hour, now)

	if plan.clampedFrom.IsZero() {
		t.Fatal("an expired checkpoint must be reported as clamped, or the operator cannot see why the resume point moved")
	}
	want := now.Add(-168 * time.Hour)
	if !plan.opts.SinceTime.Equal(want) {
		t.Fatalf("SinceTime = %v, want the floor %v", plan.opts.SinceTime, want)
	}
}

// TestFlushJoinerMarksWhatItDelivered is the defect that made a quiet pod's
// last line immortal.
//
// The joiner holds a record until the next line arrives or the stream ends,
// so on a quiet stream the LAST line is delivered by flushJoiner at EOF --
// and flushJoiner advanced nothing. The cursor stayed one record behind
// forever, every reconnect resumed before a line that had already been sent,
// and the line was delivered again. Measured on apexcrawl 2026-11-11: a
// single cert-manager-webhook line from 11-08 stored seven times, still being
// re-sent three days later, by then far enough behind its own stream's head
// that Loki refused it outright -- which is what the platform had been
// reading as late telemetry loss.
func TestFlushJoinerMarksWhatItDelivered(t *testing.T) {
	m, err := newMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("newMetrics: %v", err)
	}

	w := &recordingWriter{}
	c := &Collector{
		writer:       w,
		log:          logger.New("test"),
		metrics:      m,
		agentMeta:    &commonv1.AgentMetadata{},
		workloads:    newWorkloadResolver(fake.NewSimpleClientset(), time.Minute),
		writeTimeout: 50 * time.Millisecond,
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "quiet-1", Namespace: "cert-manager"}}
	target := streamTarget{pod: pod, container: "app"}

	readTime := time.Date(2026, 11, 8, 0, 8, 22, 0, time.UTC)
	record := ParsedLine{
		Message:   "serving webhook",
		Raw:       []byte("serving webhook"),
		Timestamp: readTime,
		ReadTime:  readTime,
	}

	join := newJoiner(maxJoinedBytes, maxJoinHold)
	join.Add(record, time.Now())

	cursor := &streamCursor{}
	c.flushJoiner(context.Background(), c.log, target, join, cursor)

	if len(w.entries) != 1 {
		t.Fatalf("writes = %d, want 1", len(w.entries))
	}
	if !cursor.alreadyDelivered(record) {
		t.Error("flushJoiner delivered a record the cursor does not know about; the next resume sends it again")
	}
	since, ok := cursor.sinceForReconnect(reconnectSinceOverlap)
	if !ok {
		t.Fatal("sinceForReconnect() = false after flushJoiner delivered a record; the stream resumes from before it")
	}
	if want := readTime.Add(-reconnectSinceOverlap); !since.Equal(want) {
		t.Errorf("since = %v, want %v", since, want)
	}
}

// A duplicate the resume replays must not reach the writer at all. Dropping it
// at the consumer, or letting Loki refuse it, costs a batch and an alert for a
// line that is already stored.
func TestAReplayedRecordIsNotDeliveredTwice(t *testing.T) {
	m, err := newMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("newMetrics: %v", err)
	}

	w := &recordingWriter{}
	c := &Collector{
		writer:       w,
		log:          logger.New("test"),
		metrics:      m,
		agentMeta:    &commonv1.AgentMetadata{},
		workloads:    newWorkloadResolver(fake.NewSimpleClientset(), time.Minute),
		writeTimeout: 50 * time.Millisecond,
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "quiet-2", Namespace: "cert-manager"}}
	target := streamTarget{pod: pod, container: "app"}

	readTime := time.Date(2026, 11, 8, 0, 8, 22, 0, time.UTC)
	record := ParsedLine{
		Message:   "serving webhook",
		Raw:       []byte("serving webhook"),
		Timestamp: readTime,
		ReadTime:  readTime,
	}
	cursor := &streamCursor{}

	for i := 0; i < 3; i++ {
		join := newJoiner(maxJoinedBytes, maxJoinHold)
		join.Add(record, time.Now())
		c.flushJoiner(context.Background(), c.log, target, join, cursor)
	}

	if len(w.entries) != 1 {
		t.Fatalf("writes = %d, want 1: the same record was replayed by two reconnects and must only reach Loki once", len(w.entries))
	}
}

// TestAttemptAfterReadResetsWhenAStreamDeliveredSomething keeps the backoff a
// backoff.
//
// attempt was reset only when consumeStream returned nil, and a follow stream
// never does -- it returns io.EOF by construction every time kubelet closes
// it. So the delay ratcheted to streamBackoffMax over a pod's life and stayed
// there: apexcrawl's agent was reading attempt=16 after 35 hours, meaning
// every reconnect waited the full 30s whether or not the previous read had
// worked perfectly.
func TestAttemptAfterReadResetsWhenAStreamDeliveredSomething(t *testing.T) {
	t.Parallel()

	if got := attemptAfterRead(16, 1); got != 0 {
		t.Errorf("attemptAfterRead(16, delivered=1) = %d, want 0: a stream that read lines is not failing", got)
	}
	// A read that delivered nothing leaves the count alone; the caller's own
	// increment is what advances the backoff.
	if got := attemptAfterRead(3, 0); got != 3 {
		t.Errorf("attemptAfterRead(3, delivered=0) = %d, want 3", got)
	}
}
