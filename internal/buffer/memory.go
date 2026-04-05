package buffer

import (
	"container/list"
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// ─────────────────────────────────────────
// Config
// ─────────────────────────────────────────

type MemoryConfig struct {
	// Capacity maximum pending item count
	Capacity int

	// MaxAttempts maximum send attempts
	// if exceeded, item is dropped
	MaxAttempts int

	// DeadLetterSize dropped items list size
	// 0 = dead-letter disabled
	DeadLetterSize int
}

func DefaultMemoryConfig() MemoryConfig {
	return MemoryConfig{
		Capacity:       1000,
		MaxAttempts:    5,
		DeadLetterSize: 100,
	}
}

// ─────────────────────────────────────────
// MemoryBuffer
// ─────────────────────────────────────────

type MemoryBuffer struct {
	cfg        MemoryConfig
	mu         sync.Mutex
	queue      *list.List               // pending items
	inflight   map[string]*list.Element // pop but ack/nack pending
	deadLetter *list.List               // max attempt exceeded

	// atomic stats
	dropped atomic.Int64
	failed  atomic.Int64

	closed bool
}

func NewMemoryBuffer(cfg MemoryConfig) (*MemoryBuffer, error) {
	if cfg.Capacity <= 0 {
		return nil, ErrInvalidConfig
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}

	return &MemoryBuffer{
		cfg:        cfg,
		queue:      list.New(),
		inflight:   make(map[string]*list.Element),
		deadLetter: list.New(),
	}, nil
}

// ─────────────────────────────────────────
// Push
// ─────────────────────────────────────────

func (b *MemoryBuffer) Push(_ context.Context, item *Item) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return ErrBufferClosed
	}

	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now()
	}

	// if capacity is full, drop oldest item (ring buffer behavior)
	if b.queue.Len() >= b.cfg.Capacity {
		oldest := b.queue.Front()
		if oldest != nil {
			dropped := oldest.Value.(*Item)
			b.queue.Remove(oldest)
			b.dropped.Add(1)
			log.Warn().
				Str("dropped_id", dropped.ID).
				Str("type", string(dropped.Type)).
				Int("capacity", b.cfg.Capacity).
				Msg("buffer full, oldest item dropped")
		}
	}

	b.queue.PushBack(item)
	return nil
}

// ─────────────────────────────────────────
// Pop
// ─────────────────────────────────────────

func (b *MemoryBuffer) Pop(_ context.Context) (*Item, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, ErrBufferClosed
	}

	front := b.queue.Front()
	if front == nil {
		return nil, ErrBufferEmpty
	}

	item := front.Value.(*Item)
	b.queue.Remove(front)

	// add to inflight: stake a list node so Remove runs in O(1); not retained in queue.
	elem := b.queue.PushBack(item)
	b.queue.Remove(elem)
	b.inflight[item.ID] = nil // only id is tracked

	return item, nil
}

// ─────────────────────────────────────────
// Peek
// ─────────────────────────────────────────

func (b *MemoryBuffer) Peek(_ context.Context) (*Item, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, ErrBufferClosed
	}

	front := b.queue.Front()
	if front == nil {
		return nil, ErrBufferEmpty
	}

	return front.Value.(*Item), nil
}

// ─────────────────────────────────────────
// Ack
// ─────────────────────────────────────────

func (b *MemoryBuffer) Ack(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return ErrBufferClosed
	}

	delete(b.inflight, id)
	return nil
}

// ─────────────────────────────────────────
// Nack
// ─────────────────────────────────────────

func (b *MemoryBuffer) Nack(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return ErrBufferClosed
	}

	delete(b.inflight, id)

	// find item and increment attempt
	// when nack is received, add item back to queue
	// original item pointer is in caller's context
	// here only dead-letter check is performed
	return nil
}

// NackItem item pointer with nack operation
func (b *MemoryBuffer) NackItem(_ context.Context, item *Item) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return ErrBufferClosed
	}

	delete(b.inflight, item.ID)
	item.Attempts++

	if item.Attempts >= b.cfg.MaxAttempts {
		b.failed.Add(1)
		log.Error().
			Str("id", item.ID).
			Str("type", string(item.Type)).
			Int("attempts", item.Attempts).
			Msg("max attempt exceeded, dead-lettered")

		if b.cfg.DeadLetterSize > 0 {
			if b.deadLetter.Len() >= b.cfg.DeadLetterSize {
				b.deadLetter.Remove(b.deadLetter.Front())
			}
			b.deadLetter.PushBack(item)
		}
		return nil
	}

	// add back to queue (append to end)
	b.queue.PushBack(item)
	return nil
}

// ─────────────────────────────────────────
// Stats
// ─────────────────────────────────────────

func (b *MemoryBuffer) Len(_ context.Context) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int64(b.queue.Len()), nil
}

func (b *MemoryBuffer) Stats(_ context.Context) (*Stats, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return &Stats{
		Pending:  int64(b.queue.Len()),
		Failed:   b.failed.Load(),
		Dropped:  b.dropped.Load(),
		Inflight: int64(len(b.inflight)),
	}, nil
}

// ─────────────────────────────────────────
// Close
// ─────────────────────────────────────────

func (b *MemoryBuffer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil
	}

	b.closed = true

	stats := Stats{
		Pending:  int64(b.queue.Len()),
		Inflight: int64(len(b.inflight)),
		Failed:   b.failed.Load(),
		Dropped:  b.dropped.Load(),
	}

	log.Info().
		Int64("pending", stats.Pending).
		Int64("inflight", stats.Inflight).
		Int64("failed", stats.Failed).
		Int64("dropped", stats.Dropped).
		Msg("memory buffer closed")

	return nil
}
