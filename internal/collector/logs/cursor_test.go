package logs

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kubexa/kubexa-agent/internal/collector/logs/checkpoint"
)

type mockCheckpointStore struct {
	mu    sync.Mutex
	data  map[checkpoint.Key]checkpoint.Position
	loads int
	saves int
}

func (m *mockCheckpointStore) Load(_ context.Context, key checkpoint.Key) (checkpoint.Position, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loads++
	pos, ok := m.data[key]
	return pos, ok, nil
}

func (m *mockCheckpointStore) Save(_ context.Context, key checkpoint.Key, pos checkpoint.Position, _ checkpoint.Metadata) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves++
	if m.data == nil {
		m.data = make(map[checkpoint.Key]checkpoint.Position)
	}
	m.data[key] = pos
	return nil
}

func (m *mockCheckpointStore) Flush(context.Context) error { return nil }

func (m *mockCheckpointStore) Close() error { return nil }

func TestStreamCursorLoadsCheckpoint(t *testing.T) {
	t.Parallel()

	store := &mockCheckpointStore{
		data: map[checkpoint.Key]checkpoint.Position{
			{PodUID: "uid-1", Container: "app"}: {LastProcessed: time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)},
		},
	}
	key := checkpoint.Key{PodUID: "uid-1", Container: "app"}

	cur := newStreamCursor(context.Background(), store, key, checkpoint.Metadata{}, nil)
	since, ok := cur.sinceForReconnect(reconnectSinceOverlap)
	if !ok {
		t.Fatal("sinceForReconnect() = false, want true")
	}
	want := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC).Add(-reconnectSinceOverlap)
	if !since.Equal(want) {
		t.Fatalf("since = %v, want %v", since, want)
	}
}

func TestStreamCursorPersistsOnClose(t *testing.T) {
	t.Parallel()

	store := &mockCheckpointStore{data: make(map[checkpoint.Key]checkpoint.Position)}
	key := checkpoint.Key{PodUID: "uid-2", Container: "c"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cur := newStreamCursor(ctx, store, key, checkpoint.Metadata{Namespace: "ns", PodName: "pod"}, nil)
	ts := time.Date(2024, 6, 1, 13, 0, 0, 0, time.UTC)
	cur.markDelivered(deliveredLine(ts, "line"))
	cur.close()

	store.mu.Lock()
	got, ok := store.data[key]
	saves := store.saves
	store.mu.Unlock()

	if !ok || !got.LastProcessed.Equal(ts) {
		t.Fatalf("stored checkpoint = %v ok=%v, want %v", got.LastProcessed, ok, ts)
	}
	if saves == 0 {
		t.Fatal("expected at least one Save on close")
	}
}

// deliveredLine is the shape the de-duplication set is keyed on: the kubelet
// read time, and the bytes that reach Loki.
func deliveredLine(readTime time.Time, raw string) ParsedLine {
	return ParsedLine{
		Raw:       []byte(raw),
		Message:   raw,
		Timestamp: readTime,
		ReadTime:  readTime,
	}
}

// TestCursorSkipsARecordItAlreadyDelivered is the defect this set exists for.
//
// A resume deliberately rewinds past the last line it handled, and the
// Kubernetes log API's SinceTime is serialized at SECOND precision, so a
// reconnect always replays lines the stream has already sent. Nothing checked
// them against anything, so every reconnect re-delivered them. Measured on
// apexcrawl 2026-11-11: one cert-manager-webhook line stored SEVEN times, and
// 137 of cert-manager-cainjector's 432 stored lines (32%) were copies.
func TestCursorSkipsARecordItAlreadyDelivered(t *testing.T) {
	t.Parallel()

	cur := &streamCursor{}
	rec := deliveredLine(time.Date(2026, 11, 11, 8, 14, 51, 123456789, time.UTC), "connection refused")

	if cur.alreadyDelivered(rec) {
		t.Fatal("alreadyDelivered() = true before the record was ever delivered")
	}
	cur.markDelivered(rec)
	if !cur.alreadyDelivered(rec) {
		t.Fatal("alreadyDelivered() = false for the exact record just delivered; every reconnect re-sends it")
	}
}

// The set may never silence a line the stream has not sent. Two distinct
// payloads sharing one read time is the case that makes a timestamp-only
// check unsafe, so the key carries the bytes as well.
func TestCursorDeliversADistinctRecordWithTheSameReadTime(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 11, 8, 14, 51, 123456789, time.UTC)
	cur := &streamCursor{}
	cur.markDelivered(deliveredLine(at, "connection refused"))

	if cur.alreadyDelivered(deliveredLine(at, "connection reset")) {
		t.Fatal("alreadyDelivered() = true for a different payload at the same read time; de-duplication must never drop a line the stream has not sent")
	}
}

// A record with no kubelet prefix has no position, and a set keyed on a zero
// time would collapse every such record onto one key -- silently dropping all
// but the first. Absent a position, deliver.
func TestCursorNeverSkipsARecordWithoutAReadTime(t *testing.T) {
	t.Parallel()

	cur := &streamCursor{}
	rec := ParsedLine{Raw: []byte("no prefix"), Timestamp: time.Unix(5, 0)}
	cur.markDelivered(rec)
	if cur.alreadyDelivered(rec) {
		t.Fatal("alreadyDelivered() = true for a record carrying no read time")
	}
}

// TestCursorResumesInTheKubeletFrame is a gap, not a duplicate: the opposite
// failure from the rest of this file.
//
// The resume point is handed to the Kubernetes log API, which filters by its
// OWN timestamps. Resuming from Timestamp meant resuming from whatever clock
// the workload printed in its JSON "time" field. A workload whose clock runs
// AHEAD of the node asks kubelet to skip past lines it has not read yet, and
// the 5s overlap covers only skew smaller than itself.
func TestCursorResumesInTheKubeletFrame(t *testing.T) {
	t.Parallel()

	read := time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)
	cur := &streamCursor{}
	cur.markDelivered(ParsedLine{
		Raw:       []byte(`{"time":"2026-11-11T09:00:00Z"}`),
		Timestamp: read.Add(time.Hour), // the workload's clock, an hour fast
		ReadTime:  read,
	})

	since, ok := cur.sinceForReconnect(reconnectSinceOverlap)
	if !ok {
		t.Fatal("sinceForReconnect() = false after a delivery")
	}
	if want := read.Add(-reconnectSinceOverlap); !since.Equal(want) {
		t.Fatalf("since = %v, want %v: the resume must be in the frame kubelet filters in, not the payload's own clock", since, want)
	}
}

// The set is bounded in both directions: it forgets what the resume can no
// longer replay, and it never grows past its cap however busy the stream is.
func TestCursorForgetsWhatTheResumeCannotReplay(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)
	cur := &streamCursor{}
	old := deliveredLine(base, "old line")
	cur.markDelivered(old)
	cur.markDelivered(deliveredLine(base.Add(recentRetention+time.Second), "new line"))

	if cur.alreadyDelivered(old) {
		t.Fatal("the set still holds a record older than the resume window; it would grow without bound on a busy stream")
	}
}

func TestCursorCapsTheDeduplicationSet(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)
	cur := &streamCursor{}
	for i := 0; i < maxRecentDelivered*2; i++ {
		cur.markDelivered(deliveredLine(base.Add(time.Duration(i)*time.Millisecond), fmt.Sprintf("line %d", i)))
	}
	if got := cur.recentLen(); got > maxRecentDelivered {
		t.Fatalf("recent set holds %d, want at most %d", got, maxRecentDelivered)
	}
}

// TestCursorRestoresItsDeduplicationSetFromTheStore is why the set is
// persisted rather than kept in memory.
//
// The checkpoint survives a restart on a PVC, so a restarting agent resumes
// from the right place -- and then re-delivers the replayed lines, because
// the only record of having sent them died with the process. An in-memory set
// leaves one duplicate per stream per restart: the same lie, smaller, and
// re-opened by every upgrade.
func TestCursorRestoresItsDeduplicationSetFromTheStore(t *testing.T) {
	t.Parallel()

	key := checkpoint.Key{PodUID: "uid-9", Container: "app"}
	store := &mockCheckpointStore{data: make(map[checkpoint.Key]checkpoint.Position)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := deliveredLine(time.Date(2026, 11, 11, 8, 14, 51, 123456789, time.UTC), "connection refused")

	first := newStreamCursor(ctx, store, key, checkpoint.Metadata{}, nil)
	first.markDelivered(rec)
	first.close()

	second := newStreamCursor(ctx, store, key, checkpoint.Metadata{}, nil)
	if !second.alreadyDelivered(rec) {
		t.Fatal("a restarted cursor re-delivers the record the resume replays; the de-duplication set must outlive the process")
	}
	since, ok := second.sinceForReconnect(reconnectSinceOverlap)
	if !ok || !since.Equal(rec.ReadTime.Add(-reconnectSinceOverlap)) {
		t.Fatalf("restored since = %v ok=%v, want %v", since, ok, rec.ReadTime.Add(-reconnectSinceOverlap))
	}
}

// TestCursorPersistsFarLessThanItRemembers keeps a checkpoint write small.
//
// The two sets answer different questions. In memory, the set must cover
// every record a RECONNECT can replay, and a reconnect happens on any stream
// at any time. On disk it only has to cover a RESTART -- and the asymmetry
// that matters is which streams suffer from a missed duplicate. A quiet
// stream's window holds a line or two and its duplicate is the expensive one:
// by the time the copy is sent, the stream's head has often moved more than
// Loki's unordered-write window past it, and the whole batch is refused. A
// busy stream's window can hold hundreds, and its duplicate is the cheap one:
// the head is seconds away, so Loki stores it. Writing hundreds of digests to
// a PVC every debounce interval, per stream, to spare the cheap case would be
// the worst trade available.
func TestCursorPersistsFarLessThanItRemembers(t *testing.T) {
	t.Parallel()

	key := checkpoint.Key{PodUID: "uid-busy", Container: "app"}
	store := &mockCheckpointStore{data: make(map[checkpoint.Key]checkpoint.Position)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cur := newStreamCursor(ctx, store, key, checkpoint.Metadata{}, nil)
	base := time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)
	for i := 0; i < maxRecentDelivered; i++ {
		cur.markDelivered(deliveredLine(base.Add(time.Duration(i)*time.Millisecond), fmt.Sprintf("line %d", i)))
	}
	cur.close()

	store.mu.Lock()
	got := store.data[key]
	store.mu.Unlock()

	if len(got.Recent) > maxPersistedDelivered {
		t.Fatalf("persisted %d digests, want at most %d", len(got.Recent), maxPersistedDelivered)
	}
	if len(got.Recent) != maxPersistedDelivered {
		t.Fatalf("persisted %d digests, want exactly %d: the newest are the ones a resume replays", len(got.Recent), maxPersistedDelivered)
	}
	// The NEWEST must be the ones kept: a resume replays from the end.
	last := got.Recent[len(got.Recent)-1]
	if !last.ReadTime.Equal(base.Add(time.Duration(maxRecentDelivered-1) * time.Millisecond)) {
		t.Fatalf("newest persisted digest is %v, want the last record delivered", last.ReadTime)
	}
}
