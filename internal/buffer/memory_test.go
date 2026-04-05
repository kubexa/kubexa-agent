package buffer

import (
	"context"
	"testing"
	"time"
)

// ─────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────

func newTestMemory(t *testing.T, capacity int) *MemoryBuffer {
	t.Helper()
	cfg := MemoryConfig{
		Capacity:       capacity,
		MaxAttempts:    3,
		DeadLetterSize: 10,
	}
	buf, err := NewMemoryBuffer(cfg)
	if err != nil {
		t.Fatalf("NewMemoryBuffer: %v", err)
	}
	t.Cleanup(func() { _ = buf.Close() })
	return buf
}

func newItem(id string, eventType EventType) *Item {
	return &Item{
		ID:        id,
		Type:      eventType,
		Payload:   []byte("payload-" + id),
		CreatedAt: time.Now(),
	}
}

func ctx() context.Context {
	return context.Background()
}

// ─────────────────────────────────────────
// Push / Pop
// ─────────────────────────────────────────

func TestMemoryBuffer_PushPop_Order(t *testing.T) {
	buf := newTestMemory(t, 10)

	items := []*Item{
		newItem("a", EventTypePod),
		newItem("b", EventTypePod),
		newItem("c", EventTypeLog),
	}

	for _, it := range items {
		if err := buf.Push(ctx(), it); err != nil {
			t.Fatalf("Push(%s): %v", it.ID, err)
		}
	}

	for _, want := range items {
		got, err := buf.Pop(ctx())
		if err != nil {
			t.Fatalf("Pop: %v", err)
		}
		if got.ID != want.ID {
			t.Errorf("order: got %s, want %s", got.ID, want.ID)
		}
	}
}

func TestMemoryBuffer_Pop_EmptyReturnsError(t *testing.T) {
	buf := newTestMemory(t, 10)

	_, err := buf.Pop(ctx())
	if err != ErrBufferEmpty {
		t.Errorf("expected ErrBufferEmpty, got %v", err)
	}
}

func TestMemoryBuffer_Peek_DoesNotRemove(t *testing.T) {
	buf := newTestMemory(t, 10)

	item := newItem("peek-1", EventTypePod)
	_ = buf.Push(ctx(), item)

	peeked, err := buf.Peek(ctx())
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if peeked.ID != item.ID {
		t.Errorf("Peek: got %s, want %s", peeked.ID, item.ID)
	}

	// item still in queue
	n, _ := buf.Len(ctx())
	if n != 1 {
		t.Errorf("Len after Peek: got %d, want 1", n)
	}
}

// ─────────────────────────────────────────
// Capacity / Ring Buffer
// ─────────────────────────────────────────

func TestMemoryBuffer_Capacity_DropsOldest(t *testing.T) {
	buf := newTestMemory(t, 3)

	for i := 0; i < 5; i++ {
		id := string(rune('a' + i)) // a, b, c, d, e
		_ = buf.Push(ctx(), newItem(id, EventTypePod))
	}

	// capacity 3: a and b dropped, c/d/e remain
	n, _ := buf.Len(ctx())
	if n != 3 {
		t.Fatalf("Len: got %d, want 3", n)
	}

	first, _ := buf.Pop(ctx())
	if first.ID != "c" {
		t.Errorf("oldest surviving: got %s, want c", first.ID)
	}

	stats, _ := buf.Stats(ctx())
	if stats.Dropped < 2 {
		t.Errorf("Dropped: got %d, want >= 2", stats.Dropped)
	}
}

// ─────────────────────────────────────────
// Ack
// ─────────────────────────────────────────

func TestMemoryBuffer_Ack_RemovesInflight(t *testing.T) {
	buf := newTestMemory(t, 10)
	_ = buf.Push(ctx(), newItem("x", EventTypePod))

	item, _ := buf.Pop(ctx())

	stats, _ := buf.Stats(ctx())
	if stats.Inflight != 1 {
		t.Errorf("Inflight before Ack: got %d, want 1", stats.Inflight)
	}

	if err := buf.Ack(ctx(), item.ID); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	stats, _ = buf.Stats(ctx())
	if stats.Inflight != 0 {
		t.Errorf("Inflight after Ack: got %d, want 0", stats.Inflight)
	}
}

// ─────────────────────────────────────────
// Nack / Requeue
// ─────────────────────────────────────────

func TestMemoryBuffer_Nack_RequeuesItem(t *testing.T) {
	buf := newTestMemory(t, 10)
	item := newItem("nack-1", EventTypePod)
	_ = buf.Push(ctx(), item)

	popped, _ := buf.Pop(ctx())

	if err := buf.NackItem(ctx(), popped); err != nil {
		t.Fatalf("NackItem: %v", err)
	}

	n, _ := buf.Len(ctx())
	if n != 1 {
		t.Errorf("Len after Nack: got %d, want 1", n)
	}

	requeued, _ := buf.Pop(ctx())
	if requeued.ID != item.ID {
		t.Errorf("requeued ID: got %s, want %s", requeued.ID, item.ID)
	}
	if requeued.Attempts != 1 {
		t.Errorf("Attempts: got %d, want 1", requeued.Attempts)
	}
}

func TestMemoryBuffer_Nack_DeadLetterOnMaxAttempts(t *testing.T) {
	cfg := MemoryConfig{
		Capacity:       10,
		MaxAttempts:    2,
		DeadLetterSize: 5,
	}
	buf, _ := NewMemoryBuffer(cfg)
	defer buf.Close()

	item := newItem("dead-1", EventTypePod)
	_ = buf.Push(ctx(), item)

	// attempt 1
	popped, _ := buf.Pop(ctx())
	_ = buf.NackItem(ctx(), popped)

	// attempt 2 — exceeds MaxAttempts (2), goes to dead-letter
	popped, _ = buf.Pop(ctx())
	_ = buf.NackItem(ctx(), popped)

	stats, _ := buf.Stats(ctx())
	if stats.Failed != 1 {
		t.Errorf("Failed: got %d, want 1", stats.Failed)
	}

	// queue must be empty now
	n, _ := buf.Len(ctx())
	if n != 0 {
		t.Errorf("Len after dead-letter: got %d, want 0", n)
	}
}

// ─────────────────────────────────────────
// Closed Buffer
// ─────────────────────────────────────────

func TestMemoryBuffer_ClosedBuffer_ReturnsError(t *testing.T) {
	buf := newTestMemory(t, 10)
	_ = buf.Close()

	if err := buf.Push(ctx(), newItem("z", EventTypePod)); err != ErrBufferClosed {
		t.Errorf("Push after Close: got %v, want ErrBufferClosed", err)
	}

	if _, err := buf.Pop(ctx()); err != ErrBufferClosed {
		t.Errorf("Pop after Close: got %v, want ErrBufferClosed", err)
	}
}

// ─────────────────────────────────────────
// Stats
// ─────────────────────────────────────────

func TestMemoryBuffer_Stats(t *testing.T) {
	buf := newTestMemory(t, 10)

	for i := 0; i < 3; i++ {
		_ = buf.Push(ctx(), newItem(string(rune('a'+i)), EventTypePod))
	}

	stats, err := buf.Stats(ctx())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Pending != 3 {
		t.Errorf("Pending: got %d, want 3", stats.Pending)
	}
}

// ─────────────────────────────────────────
// Concurrent Safety
// ─────────────────────────────────────────

func TestMemoryBuffer_Concurrent(t *testing.T) {
	buf := newTestMemory(t, 500)
	const goroutines = 10
	const itemsEach = 50

	done := make(chan struct{})

	// concurrent pushers
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			for i := 0; i < itemsEach; i++ {
				id := string(rune('A'+g)) + string(rune('0'+i%10))
				_ = buf.Push(ctx(), newItem(id, EventTypePod))
			}
		}(g)
	}

	// concurrent popper
	popped := make(chan *Item, goroutines*itemsEach)
	go func() {
		defer close(done)
		for i := 0; i < goroutines*itemsEach; i++ {
			var item *Item
			var err error
			for {
				item, err = buf.Pop(ctx())
				if err == nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
			popped <- item
		}
	}()

	<-done
	close(popped)

	count := 0
	for range popped {
		count++
	}

	if count != goroutines*itemsEach {
		t.Errorf("concurrent: got %d items, want %d", count, goroutines*itemsEach)
	}
}
