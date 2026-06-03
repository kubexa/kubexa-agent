package logs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kubexa/kubexa-agent/internal/collector/logs/checkpoint"
)

type mockCheckpointStore struct {
	mu    sync.Mutex
	data  map[checkpoint.Key]time.Time
	loads int
	saves int
}

func (m *mockCheckpointStore) Load(_ context.Context, key checkpoint.Key) (time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loads++
	ts, ok := m.data[key]
	return ts, ok, nil
}

func (m *mockCheckpointStore) Save(_ context.Context, key checkpoint.Key, lastProcessed time.Time, _ checkpoint.Metadata) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves++
	if m.data == nil {
		m.data = make(map[checkpoint.Key]time.Time)
	}
	m.data[key] = lastProcessed.UTC()
	return nil
}

func (m *mockCheckpointStore) Flush(context.Context) error { return nil }

func (m *mockCheckpointStore) Close() error { return nil }

func TestStreamCursorLoadsCheckpoint(t *testing.T) {
	t.Parallel()

	store := &mockCheckpointStore{
		data: map[checkpoint.Key]time.Time{
			{PodUID: "uid-1", Container: "app"}: time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
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

	store := &mockCheckpointStore{data: make(map[checkpoint.Key]time.Time)}
	key := checkpoint.Key{PodUID: "uid-2", Container: "c"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cur := newStreamCursor(ctx, store, key, checkpoint.Metadata{Namespace: "ns", PodName: "pod"}, nil)
	ts := time.Date(2024, 6, 1, 13, 0, 0, 0, time.UTC)
	cur.markProcessed(ts)
	cur.close()

	store.mu.Lock()
	got, ok := store.data[key]
	saves := store.saves
	store.mu.Unlock()

	if !ok || !got.Equal(ts) {
		t.Fatalf("stored checkpoint = %v ok=%v, want %v", got, ok, ts)
	}
	if saves == 0 {
		t.Fatal("expected at least one Save on close")
	}
}
