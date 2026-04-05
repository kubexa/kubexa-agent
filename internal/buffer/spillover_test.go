package buffer

import (
	"context"
	"testing"
)

// ─────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────

func newTestSpillover(t *testing.T, capacity int) *SpilloverBuffer {
	t.Helper()
	mem, err := NewMemoryBuffer(MemoryConfig{
		Capacity:       capacity,
		MaxAttempts:    3,
		DeadLetterSize: 10,
	})
	if err != nil {
		t.Fatalf("NewMemoryBuffer: %v", err)
	}
	sp := NewSpilloverBuffer(mem, nil, capacity) // memory-only (no Redis)
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// ─────────────────────────────────────────
// Basic Push / Pop
// ─────────────────────────────────────────

func TestSpillover_PushPop_Basic(t *testing.T) {
	sp := newTestSpillover(t, 10)

	item := newItem("s1", EventTypePod)
	if err := sp.Push(context.Background(), item); err != nil {
		t.Fatalf("Push: %v", err)
	}

	got, err := sp.Pop(context.Background())
	if err != nil {
		t.Fatalf("Pop: %v", err)
	}
	if got.ID != item.ID {
		t.Errorf("ID: got %s, want %s", got.ID, item.ID)
	}
}

func TestSpillover_Pop_EmptyReturnsError(t *testing.T) {
	sp := newTestSpillover(t, 10)

	_, err := sp.Pop(context.Background())
	if err != ErrBufferEmpty {
		t.Errorf("expected ErrBufferEmpty, got %v", err)
	}
}

// ─────────────────────────────────────────
// Capacity — memory only
// ─────────────────────────────────────────

func TestSpillover_MemoryOnly_DropOldestWhenFull(t *testing.T) {
	sp := newTestSpillover(t, 3)

	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		_ = sp.Push(context.Background(), newItem(id, EventTypePod))
	}

	n, _ := sp.Len(context.Background())
	if n != 3 {
		t.Fatalf("Len: got %d, want 3", n)
	}

	first, _ := sp.Pop(context.Background())
	if first.ID != "c" {
		t.Errorf("oldest surviving: got %s, want c", first.ID)
	}
}

// ─────────────────────────────────────────
// Ack
// ─────────────────────────────────────────

func TestSpillover_Ack_ClearsInflight(t *testing.T) {
	sp := newTestSpillover(t, 10)
	_ = sp.Push(context.Background(), newItem("ack-1", EventTypePod))

	item, _ := sp.Pop(context.Background())

	if err := sp.Ack(context.Background(), item); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	n, _ := sp.Len(context.Background())
	if n != 0 {
		t.Errorf("Len after Ack: got %d, want 0", n)
	}
}

// ─────────────────────────────────────────
// Nack / Requeue
// ─────────────────────────────────────────

func TestSpillover_Nack_Requeues(t *testing.T) {
	sp := newTestSpillover(t, 10)
	item := newItem("nack-s1", EventTypePod)
	_ = sp.Push(context.Background(), item)

	popped, _ := sp.Pop(context.Background())
	if err := sp.Nack(context.Background(), popped); err != nil {
		t.Fatalf("Nack: %v", err)
	}

	n, _ := sp.Len(context.Background())
	if n != 1 {
		t.Errorf("Len after Nack: got %d, want 1", n)
	}
}

// ─────────────────────────────────────────
// Len across both tiers
// ─────────────────────────────────────────

func TestSpillover_Len_MemoryOnly(t *testing.T) {
	sp := newTestSpillover(t, 10)

	for i := 0; i < 4; i++ {
		_ = sp.Push(context.Background(), newItem(string(rune('a'+i)), EventTypeLog))
	}

	n, err := sp.Len(context.Background())
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 4 {
		t.Errorf("Len: got %d, want 4", n)
	}
}

// ─────────────────────────────────────────
// Stats
// ─────────────────────────────────────────

func TestSpillover_Stats(t *testing.T) {
	sp := newTestSpillover(t, 10)

	_ = sp.Push(context.Background(), newItem("st1", EventTypePod))
	_ = sp.Push(context.Background(), newItem("st2", EventTypeLog))

	stats, err := sp.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Pending != 2 {
		t.Errorf("Pending: got %d, want 2", stats.Pending)
	}
}
