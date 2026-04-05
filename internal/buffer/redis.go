package buffer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

// ─────────────────────────────────────────
// Config
// ─────────────────────────────────────────

type RedisBufferConfig struct {
	Address     string
	Password    string
	DB          int
	MaxAttempts int

	// key prefix — different agents can share the same redis
	KeyPrefix string

	// TTL maximum item lifetime in redis
	// if backend never comes, it will live forever
	ItemTTL time.Duration

	// DeadLetterTTL dead-letter item TTL
	DeadLetterTTL time.Duration

	// DialTimeout connection timeout
	DialTimeout time.Duration

	// ReadTimeout / WriteTimeout
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// PoolSize connection pool
	PoolSize int
}

func DefaultRedisBufferConfig(address, keyPrefix string) RedisBufferConfig {
	return RedisBufferConfig{
		Address:       address,
		DB:            0,
		MaxAttempts:   5,
		KeyPrefix:     keyPrefix,
		ItemTTL:       24 * time.Hour,
		DeadLetterTTL: 72 * time.Hour,
		DialTimeout:   5 * time.Second,
		ReadTimeout:   3 * time.Second,
		WriteTimeout:  3 * time.Second,
		PoolSize:      5,
	}
}

// ─────────────────────────────────────────
// Keys
// ─────────────────────────────────────────

func (c *RedisBufferConfig) pendingKey() string {
	return fmt.Sprintf("%s:buffer:pending", c.KeyPrefix)
}

func (c *RedisBufferConfig) inflightKey() string {
	return fmt.Sprintf("%s:buffer:inflight", c.KeyPrefix)
}

func (c *RedisBufferConfig) deadLetterKey() string {
	return fmt.Sprintf("%s:buffer:dead", c.KeyPrefix)
}

func (c *RedisBufferConfig) itemKey(id string) string {
	return fmt.Sprintf("%s:buffer:item:%s", c.KeyPrefix, id)
}

// ─────────────────────────────────────────
// RedisBuffer
// ─────────────────────────────────────────

type RedisBuffer struct {
	cfg    RedisBufferConfig
	client *redis.Client

	dropped atomic.Int64
	failed  atomic.Int64

	closed atomic.Bool
}

func NewRedisBuffer(cfg RedisBufferConfig) (*RedisBuffer, error) {
	if cfg.Address == "" {
		return nil, ErrInvalidConfig
	}
	if cfg.KeyPrefix == "" {
		return nil, fmt.Errorf("%w: keyPrefix cannot be empty", ErrInvalidConfig)
	}

	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Address,
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		PoolSize:     cfg.PoolSize,
	})

	// bağlantıyı doğrula
	ctx, cancel := context.WithTimeout(context.Background(), cfg.DialTimeout)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis connection failed: %w", err)
	}

	log.Info().
		Str("address", cfg.Address).
		Str("prefix", cfg.KeyPrefix).
		Msg("redis buffer connection established")

	return &RedisBuffer{
		cfg:    cfg,
		client: client,
	}, nil
}

// ─────────────────────────────────────────
// Push
// ─────────────────────────────────────────

func (b *RedisBuffer) Push(ctx context.Context, item *Item) error {
	if b.closed.Load() {
		return ErrBufferClosed
	}

	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now()
	}

	data, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("item marshal error: %w", err)
	}

	pipe := b.client.Pipeline()

	// item data to separate key with TTL
	pipe.Set(ctx, b.cfg.itemKey(item.ID), data, b.cfg.ItemTTL)

	// add id to pending list
	pipe.RPush(ctx, b.cfg.pendingKey(), item.ID)

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis push error: %w", err)
	}

	return nil
}

// ─────────────────────────────────────────
// Pop
// ─────────────────────────────────────────

func (b *RedisBuffer) Pop(ctx context.Context) (*Item, error) {
	if b.closed.Load() {
		return nil, ErrBufferClosed
	}

	// pending → inflight (atomic move)
	id, err := b.client.LMove(ctx,
		b.cfg.pendingKey(),
		b.cfg.inflightKey(),
		"LEFT", "RIGHT",
	).Result()

	if errors.Is(err, redis.Nil) {
		return nil, ErrBufferEmpty
	}
	if err != nil {
		return nil, fmt.Errorf("redis pop error: %w", err)
	}

	item, err := b.getItem(ctx, id)
	if err != nil {
		// item data may have been lost (TTL expired)
		// clean up from inflight
		_ = b.client.LRem(ctx, b.cfg.inflightKey(), 1, id).Err()
		return nil, fmt.Errorf("item data not found (id: %s): %w", id, err)
	}

	return item, nil
}

// ─────────────────────────────────────────
// Peek
// ─────────────────────────────────────────

func (b *RedisBuffer) Peek(ctx context.Context) (*Item, error) {
	if b.closed.Load() {
		return nil, ErrBufferClosed
	}

	ids, err := b.client.LRange(ctx, b.cfg.pendingKey(), 0, 0).Result()
	if err != nil {
		return nil, fmt.Errorf("redis peek error: %w", err)
	}
	if len(ids) == 0 {
		return nil, ErrBufferEmpty
	}

	return b.getItem(ctx, ids[0])
}

// ─────────────────────────────────────────
// Ack
// ─────────────────────────────────────────

func (b *RedisBuffer) Ack(ctx context.Context, id string) error {
	if b.closed.Load() {
		return ErrBufferClosed
	}

	pipe := b.client.Pipeline()
	pipe.LRem(ctx, b.cfg.inflightKey(), 1, id)
	pipe.Del(ctx, b.cfg.itemKey(id))

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis ack error: %w", err)
	}

	return nil
}

// ─────────────────────────────────────────
// Nack
// ─────────────────────────────────────────

func (b *RedisBuffer) Nack(ctx context.Context, id string) error {
	if b.closed.Load() {
		return ErrBufferClosed
	}

	item, err := b.getItem(ctx, id)
	if err != nil {
		_ = b.client.LRem(ctx, b.cfg.inflightKey(), 1, id).Err()
		return fmt.Errorf("nack item data not found: %w", err)
	}

	item.Attempts++

	// remove from inflight
	if err := b.client.LRem(ctx, b.cfg.inflightKey(), 1, id).Err(); err != nil {
		return fmt.Errorf("redis nack inflight remove error: %w", err)
	}

	if item.Attempts >= b.cfg.MaxAttempts {
		return b.moveToDeadLetter(ctx, item)
	}

	// update item with current attempt
	data, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("item marshal error: %w", err)
	}

	pipe := b.client.Pipeline()
	pipe.Set(ctx, b.cfg.itemKey(id), data, b.cfg.ItemTTL)
	pipe.RPush(ctx, b.cfg.pendingKey(), id) // append to end

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis nack requeue error: %w", err)
	}

	return nil
}

// ─────────────────────────────────────────
// Dead Letter
// ─────────────────────────────────────────

func (b *RedisBuffer) moveToDeadLetter(ctx context.Context, item *Item) error {
	b.failed.Add(1)

	log.Error().
		Str("id", item.ID).
		Str("type", string(item.Type)).
		Int("attempts", item.Attempts).
		Msg("max attempt exceeded, dead-lettered")

	data, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("dead-letter marshal error: %w", err)
	}

	pipe := b.client.Pipeline()
	// write item data to dead-letter key with long TTL
	pipe.Set(ctx, b.cfg.itemKey(item.ID)+":dead", data, b.cfg.DeadLetterTTL)
	pipe.RPush(ctx, b.cfg.deadLetterKey(), item.ID)
	// delete original item
	pipe.Del(ctx, b.cfg.itemKey(item.ID))

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("dead-letter move error: %w", err)
	}

	return nil
}

// ─────────────────────────────────────────
// Stats
// ─────────────────────────────────────────

func (b *RedisBuffer) Len(ctx context.Context) (int64, error) {
	n, err := b.client.LLen(ctx, b.cfg.pendingKey()).Result()
	if err != nil {
		return 0, fmt.Errorf("redis len error: %w", err)
	}
	return n, nil
}

func (b *RedisBuffer) Stats(ctx context.Context) (*Stats, error) {
	pipe := b.client.Pipeline()
	pendingCmd := pipe.LLen(ctx, b.cfg.pendingKey())
	inflightCmd := pipe.LLen(ctx, b.cfg.inflightKey())
	deadCmd := pipe.LLen(ctx, b.cfg.deadLetterKey())

	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("redis stats error: %w", err)
	}

	return &Stats{
		Pending:  pendingCmd.Val(),
		Inflight: inflightCmd.Val(),
		Failed:   deadCmd.Val(),
		Dropped:  b.dropped.Load(),
	}, nil
}

// ─────────────────────────────────────────
// Close
// ─────────────────────────────────────────

func (b *RedisBuffer) Close() error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}

	stats, _ := b.Stats(context.Background())
	if stats != nil {
		log.Info().
			Int64("pending", stats.Pending).
			Int64("inflight", stats.Inflight).
			Int64("failed", stats.Failed).
			Int64("dropped", b.dropped.Load()).
			Msg("redis buffer closed")
	}

	return b.client.Close()
}

// ─────────────────────────────────────────
// Helper
// ─────────────────────────────────────────

func (b *RedisBuffer) getItem(ctx context.Context, id string) (*Item, error) {
	data, err := b.client.Get(ctx, b.cfg.itemKey(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("item not found (id: %s)", id)
	}
	if err != nil {
		return nil, fmt.Errorf("redis get error: %w", err)
	}

	var item Item
	if err := json.Unmarshal(data, &item); err != nil {
		return nil, fmt.Errorf("item unmarshal error: %w", err)
	}

	return &item, nil
}
