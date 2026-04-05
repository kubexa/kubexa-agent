package buffer

import (
	"context"
	"sync/atomic"

	"github.com/rs/zerolog/log"
)

// SpilloverBuffer is a two-tier buffer: memory first, Redis on overflow.
// If Redis is nil, it behaves as memory-only.
//
// Push:  memory → if full, spill to Redis
// Pop:   memory first → if empty, drain one item from Redis into memory, then pop
// Ack:   remove from whichever tier held the item
// Nack:  requeue with attempt++ (memory preferred)
type SpilloverBuffer struct {
	mem      *MemoryBuffer
	redis    *RedisBuffer // nil = memory-only
	capacity int

	// stats
	spills  atomic.Int64 // times an item was spilled to Redis
	drained atomic.Int64 // times an item was pulled from Redis back to memory
}

// NewSpilloverBuffer creates a SpilloverBuffer.
// redis may be nil for memory-only mode.
func NewSpilloverBuffer(mem *MemoryBuffer, redis *RedisBuffer, capacity int) *SpilloverBuffer {
	return &SpilloverBuffer{
		mem:      mem,
		redis:    redis,
		capacity: capacity,
	}
}

// ─────────────────────────────────────────
// Push
// ─────────────────────────────────────────

// Push writes the item to memory. If memory is full and Redis is available,
// spills the oldest memory item to Redis and retries.
func (s *SpilloverBuffer) Push(ctx context.Context, item *Item) error {
	err := s.mem.Push(ctx, item)
	if err == nil {
		return nil
	}

	// memory full — spill to Redis if available
	if s.redis == nil {
		// memory-only: MemoryBuffer already dropped the oldest item internally
		log.Warn().
			Str("id", item.ID).
			Msg("spillover: memory full, no Redis available — oldest item dropped")
		return s.mem.Push(ctx, item)
	}

	// move oldest memory item to Redis to free space
	if spillErr := s.spillOldest(ctx); spillErr != nil {
		log.Warn().Err(spillErr).Msg("spillover: failed to spill to Redis, dropping oldest")
	}

	return s.mem.Push(ctx, item)
}

// spillOldest pops the oldest item from memory and pushes it to Redis.
func (s *SpilloverBuffer) spillOldest(ctx context.Context) error {
	oldest, err := s.mem.Pop(ctx)
	if err != nil {
		return err
	}

	oldest.SpilloverRedis = true
	if err := s.redis.Push(ctx, oldest); err != nil {
		// Redis also failed — requeue to memory to avoid total loss
		_ = s.mem.Push(ctx, oldest)
		return err
	}

	s.spills.Add(1)
	log.Debug().
		Str("id", oldest.ID).
		Str("type", string(oldest.Type)).
		Msg("spillover: item spilled to Redis")

	return nil
}

// ─────────────────────────────────────────
// Pop
// ─────────────────────────────────────────

// Pop returns the next item. Reads memory first.
// If memory is empty and Redis has items, drains one batch from Redis into memory.
func (s *SpilloverBuffer) Pop(ctx context.Context) (*Item, error) {
	// fast path: memory has items
	item, err := s.mem.Pop(ctx)
	if err == nil {
		return item, nil
	}

	if s.redis == nil {
		return nil, ErrBufferEmpty
	}

	// memory empty — drain one item from Redis
	rItem, rErr := s.redis.Pop(ctx)
	if rErr != nil {
		// Redis also empty
		return nil, ErrBufferEmpty
	}

	// ack from Redis tier (we're taking ownership in memory now)
	_ = s.redis.Ack(ctx, rItem.ID)
	rItem.SpilloverRedis = false
	s.drained.Add(1)

	log.Debug().
		Str("id", rItem.ID).
		Str("type", string(rItem.Type)).
		Msg("spillover: item drained from Redis to memory")

	return rItem, nil
}

// ─────────────────────────────────────────
// Peek
// ─────────────────────────────────────────

func (s *SpilloverBuffer) Peek(ctx context.Context) (*Item, error) {
	item, err := s.mem.Peek(ctx)
	if err == nil {
		return item, nil
	}
	if s.redis == nil {
		return nil, ErrBufferEmpty
	}
	return s.redis.Peek(ctx)
}

// ─────────────────────────────────────────
// Ack / Nack
// ─────────────────────────────────────────

// Ack removes the item from whichever tier it came from.
// SpilloverRedis flag determines the tier.
func (s *SpilloverBuffer) Ack(ctx context.Context, item *Item) error {
	if item.SpilloverRedis && s.redis != nil {
		return s.redis.Ack(ctx, item.ID)
	}
	return s.mem.Ack(ctx, item.ID)
}

// Nack requeues the item with attempt++.
// Always requeues to memory (memory has priority for retry speed).
func (s *SpilloverBuffer) Nack(ctx context.Context, item *Item) error {
	// clear Redis flag — retry goes through memory tier
	item.SpilloverRedis = false

	if s.redis != nil && item.SpilloverRedis {
		_ = s.redis.Ack(ctx, item.ID) // remove from Redis
	}

	return s.mem.NackItem(ctx, item)
}

// ─────────────────────────────────────────
// Len / Stats
// ─────────────────────────────────────────

// Len returns total pending items across both tiers.
func (s *SpilloverBuffer) Len(ctx context.Context) (int64, error) {
	memLen, err := s.mem.Len(ctx)
	if err != nil {
		return 0, err
	}

	if s.redis == nil {
		return memLen, nil
	}

	redisLen, err := s.redis.Len(ctx)
	if err != nil {
		// Redis unreachable — return memory count only
		log.Warn().Err(err).Msg("spillover: Redis Len failed, returning memory count only")
		return memLen, nil
	}

	return memLen + redisLen, nil
}

// Stats returns combined stats from both tiers.
func (s *SpilloverBuffer) Stats(ctx context.Context) (*Stats, error) {
	memStats, err := s.mem.Stats(ctx)
	if err != nil {
		return nil, err
	}

	if s.redis == nil {
		return memStats, nil
	}

	redisStats, err := s.redis.Stats(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("spillover: Redis Stats failed, returning memory stats only")
		return memStats, nil
	}

	return &Stats{
		Pending:  memStats.Pending + redisStats.Pending,
		Inflight: memStats.Inflight + redisStats.Inflight,
		Failed:   memStats.Failed + redisStats.Failed,
		Dropped:  memStats.Dropped + redisStats.Dropped,
	}, nil
}

// ─────────────────────────────────────────
// Close
// ─────────────────────────────────────────

func (s *SpilloverBuffer) Close() error {
	stats, _ := s.Stats(context.Background())
	if stats != nil {
		log.Info().
			Int64("pending", stats.Pending).
			Int64("inflight", stats.Inflight).
			Int64("spills", s.spills.Load()).
			Int64("drained", s.drained.Load()).
			Msg("spillover buffer closing")
	}

	if err := s.mem.Close(); err != nil {
		return err
	}

	if s.redis != nil {
		return s.redis.Close()
	}

	return nil
}
