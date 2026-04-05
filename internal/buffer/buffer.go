package buffer

import (
	"context"
	"errors"
	"time"
)

// ─────────────────────────────────────────
// Errors
// ─────────────────────────────────────────

var (
	ErrBufferFull    = errors.New("buffer full, event dropped")
	ErrBufferEmpty   = errors.New("buffer empty")
	ErrBufferClosed  = errors.New("buffer closed")
	ErrInvalidConfig = errors.New("invalid buffer config")
)

// ─────────────────────────────────────────
// Types
// ─────────────────────────────────────────

// EventType buffer will store event type
type EventType string

const (
	EventTypePod EventType = "pod"
	EventTypeLog EventType = "log"
)

// Item buffer will store single unit
type Item struct {
	ID             string    // unique id (ulid/uuid)
	Type           EventType // pod | log
	Payload        []byte    // proto marshaled data
	CreatedAt      time.Time
	Attempts       int  // how many times it has been tried to send
	SpilloverRedis bool `json:"spillover_redis,omitempty"` // set when item was popped from Redis tier
}

// Stats buffer's current state
type Stats struct {
	Pending  int64
	Failed   int64 // max attempt exceeded
	Dropped  int64 // dropped because buffer is full
	Inflight int64 // in flight
}

// ─────────────────────────────────────────
// Interface
// ─────────────────────────────────────────

// Buffer events are temporarily stored and consumed in order.
// Implementations: MemoryBuffer, RedisBuffer
type Buffer interface {
	// Push new item.
	// If buffer is full, return ErrBufferFull.
	Push(ctx context.Context, item *Item) error

	// Pop next item and remove from buffer.
	// Buffer boşsa ErrBufferEmpty döner.
	Pop(ctx context.Context) (*Item, error)

	// Peek next item but do not remove from buffer.
	Peek(ctx context.Context) (*Item, error)

	// Ack successfully processed item and remove from buffer.
	Ack(ctx context.Context, id string) error

	// Nack failed item and put back to buffer (attempt++).
	// If maxAttempts exceeded, item is dead-lettered.
	Nack(ctx context.Context, id string) error

	// Len pending item count.
	Len(ctx context.Context) (int64, error)

	// Stats current buffer statistics.
	Stats(ctx context.Context) (*Stats, error)

	// Close buffer and release resources.
	Close() error
}
