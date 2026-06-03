package logs

import (
	"testing"
	"time"
)

func TestStreamCursor_markProcessed(t *testing.T) {
	t.Parallel()

	cur := &streamCursor{}
	ts1 := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	ts2 := time.Date(2024, 6, 1, 12, 0, 1, 0, time.UTC)
	tsOlder := time.Date(2024, 6, 1, 11, 59, 0, 0, time.UTC)

	cur.markProcessed(ts1)
	cur.markProcessed(tsOlder)
	cur.markProcessed(ts2)

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
	cur.markProcessed(ts)

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
	cur.markProcessed(time.Now())
	if _, ok := cur.sinceForReconnect(time.Second); ok {
		t.Fatal("nil cursor should not report resume point")
	}
}
