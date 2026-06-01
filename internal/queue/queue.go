// Package queue provides a durable two-tier buffer between collectors and the gRPC export stream.
package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kubexa/kubexa-agent/internal/logger"
	"github.com/kubexa/kubexa-agent/pkg/config"
)

const (
	// avgItemSizeEstimate is used to size the in-memory channel from MaxMemoryBytes.
	avgItemSizeEstimate = 4096
)

// Item is a single buffered agent message awaiting delivery.
type Item struct {
	// ID uniquely identifies the item; generated on enqueue when empty.
	ID string
	// Payload holds the protobuf-serialized AgentMessage.
	Payload []byte
	// EnqueuedAt is when the item entered the queue.
	EnqueuedAt time.Time
	// Attempts counts how many times the item was nacked for retry.
	Attempts int
}

// Queue buffers collected data between producers and the gRPC stream with optional disk spill.
type Queue interface {
	// Enqueue adds an item to the queue.
	// Blocks if memory is full and disk spill is disabled.
	// Returns error only on unrecoverable failure.
	Enqueue(ctx context.Context, item Item) error

	// DequeueBatch returns up to n items from the queue.
	// Blocks until at least one item is available or ctx is done.
	DequeueBatch(ctx context.Context, n int) ([]Item, error)

	// Ack marks items as successfully delivered and removes them permanently.
	// Items that are not acked will be re-delivered after restart (if disk spill enabled).
	Ack(ids []string) error

	// Nack returns items back to the front of the queue for immediate retry.
	Nack(ids []string) error

	// Depth returns current number of items in queue (memory + disk).
	Depth() int64

	// DroppedTotal returns total number of dropped items since start.
	DroppedTotal() int64

	// Close flushes pending items to disk (if spill enabled) and releases resources.
	Close() error
}

type inflightEntry struct {
	item   Item
	onDisk bool
}

// bufferedQueue implements Queue with an in-memory channel and optional WAL disk spill.
type bufferedQueue struct {
	cfg     *config.BufferConfig
	log     *logger.Logger
	metrics *queueMetrics

	mu     sync.Mutex
	cond   *sync.Cond
	closed bool

	memCh     chan Item
	memBytes  int64
	memCount  int64
	diskCount int64
	diskHead  []Item
	disk      *diskStore

	inflight map[string]*inflightEntry

	dropped atomic.Int64
}

// New constructs a ready-to-use Queue from cfg, logging through log and registering metrics on reg.
func New(cfg *config.BufferConfig, log *logger.Logger, reg prometheus.Registerer) (Queue, error) {
	if cfg == nil {
		return nil, errors.New("buffer config is nil")
	}
	if err := validateBufferConfig(cfg); err != nil {
		return nil, err
	}
	if log == nil {
		log = logger.New("queue")
	}

	metrics, err := newQueueMetrics(reg)
	if err != nil {
		return nil, fmt.Errorf("init queue metrics: %w", err)
	}

	slots := memorySlotCapacity(cfg.MaxMemoryBytes)
	q := &bufferedQueue{
		cfg:      cfg,
		log:      log,
		metrics:  metrics,
		memCh:    make(chan Item, slots),
		inflight: make(map[string]*inflightEntry),
	}
	q.cond = sync.NewCond(&q.mu)

	if cfg.SpillDir != "" {
		maxDisk := cfg.MaxDiskBytes
		if maxDisk <= 0 {
			maxDisk = 512 << 20
		}
		ds, err := newDiskStore(cfg.SpillDir, maxDisk, log, metrics)
		if err != nil {
			return nil, fmt.Errorf("init disk spill: %w", err)
		}
		q.disk = ds

		recovered, err := ds.recover()
		if err != nil {
			_ = ds.close()
			return nil, fmt.Errorf("recover spill segments: %w", err)
		}
		for _, item := range recovered {
			q.diskHead = append(q.diskHead, item)
			q.diskCount++
		}
		if len(recovered) > 0 {
			log.Info("recovered items from disk spill", logger.F("count", len(recovered)))
		}
	}

	q.updateDepthMetrics()
	return q, nil
}

func validateBufferConfig(cfg *config.BufferConfig) error {
	if cfg.MaxMemoryBytes <= 0 {
		return fmt.Errorf("buffer.max_memory_bytes must be greater than 0")
	}
	if cfg.SpillDir != "" && cfg.MaxDiskBytes < 0 {
		return fmt.Errorf("buffer.max_disk_bytes must not be negative")
	}
	return nil
}

func memorySlotCapacity(maxMemoryBytes int64) int {
	slots := int(maxMemoryBytes / avgItemSizeEstimate)
	if slots < 1 {
		return 1
	}
	return slots
}

func (q *bufferedQueue) itemSize(item Item) int64 {
	return int64(len(item.Payload) + len(item.ID) + 32)
}

// Enqueue adds an item to the queue, respecting context cancellation and capacity limits.
func (q *bufferedQueue) Enqueue(ctx context.Context, item Item) error {
	if item.ID == "" {
		item.ID = uuid.NewString()
	}
	if item.EnqueuedAt.IsZero() {
		item.EnqueuedAt = time.Now().UTC()
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return errors.New("queue is closed")
		}

		size := q.itemSize(item)
		switch {
		case q.canAcceptInMemory(size):
			if err := q.enqueueMemoryUnlocked(item, size); err != nil {
				q.mu.Unlock()
				return err
			}
			q.signalWaitersLocked()
			q.mu.Unlock()
			return nil

		case q.disk != nil:
			if err := q.makeMemoryRoomUnlocked(size); err != nil {
				q.mu.Unlock()
				return err
			}
			if q.canAcceptInMemory(size) {
				if err := q.enqueueMemoryUnlocked(item, size); err != nil {
					q.mu.Unlock()
					return err
				}
				q.signalWaitersLocked()
				q.mu.Unlock()
				return nil
			}
			if err := q.spillEnqueueUnlocked(item); err != nil {
				if errors.Is(err, ErrDiskFull) {
					q.waitForCapacity(ctx)
					q.mu.Unlock()
					continue
				}
				q.mu.Unlock()
				return err
			}
			q.signalWaitersLocked()
			q.mu.Unlock()
			return nil

		default:
			q.dropOldestUnlocked()
			if q.canAcceptInMemory(size) {
				if err := q.enqueueMemoryUnlocked(item, size); err != nil {
					q.mu.Unlock()
					return err
				}
				q.signalWaitersLocked()
				q.mu.Unlock()
				return nil
			}
			q.mu.Unlock()
		}
	}
}

func (q *bufferedQueue) waitForCapacity(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			q.mu.Lock()
			q.cond.Broadcast()
			q.mu.Unlock()
		case <-done:
		}
	}()
	q.cond.Wait()
}

func (q *bufferedQueue) canAcceptInMemory(additional int64) bool {
	return q.memBytes+additional <= q.cfg.MaxMemoryBytes &&
		int64(len(q.memCh)) < int64(cap(q.memCh))
}

func (q *bufferedQueue) enqueueMemoryUnlocked(item Item, size int64) error {
	return q.putMemoryUnlocked(item, size, true)
}

func (q *bufferedQueue) putMemoryUnlocked(item Item, size int64, countEnqueue bool) error {
	select {
	case q.memCh <- item:
		q.memBytes += size
		q.memCount++
		if countEnqueue {
			q.metrics.enqueuedTotal.Inc()
		}
		q.updateDepthMetricsLocked()
		return nil
	default:
		return errors.New("memory channel full")
	}
}

func (q *bufferedQueue) makeMemoryRoomUnlocked(need int64) error {
	for !q.canAcceptInMemory(need) && (q.memCount > 0 || len(q.memCh) > 0) {
		if err := q.evictOldestMemoryUnlocked(); err != nil {
			return err
		}
	}
	return nil
}

func (q *bufferedQueue) evictOldestMemoryUnlocked() error {
	select {
	case oldest := <-q.memCh:
		q.memBytes -= q.itemSize(oldest)
		q.memCount--
		if q.disk != nil {
			if err := q.disk.appendItem(oldest); err != nil {
				q.memCh <- oldest
				q.memBytes += q.itemSize(oldest)
				q.memCount++
				return fmt.Errorf("spill item to disk: %w", err)
			}
			q.diskHead = append(q.diskHead, oldest)
			q.diskCount++
			q.updateDepthMetricsLocked()
			return nil
		}
		q.dropItemUnlocked(oldest)
		return nil
	default:
		return nil
	}
}

func (q *bufferedQueue) spillEnqueueUnlocked(item Item) error {
	if err := q.disk.appendItem(item); err != nil {
		return err
	}
	q.diskHead = append(q.diskHead, item)
	q.diskCount++
	q.metrics.enqueuedTotal.Inc()
	q.updateDepthMetricsLocked()
	return nil
}

func (q *bufferedQueue) dropOldestUnlocked() {
	select {
	case oldest := <-q.memCh:
		q.memBytes -= q.itemSize(oldest)
		q.memCount--
		q.dropItemUnlocked(oldest)
	default:
		if len(q.diskHead) > 0 {
			dropped := q.diskHead[0]
			q.diskHead = q.diskHead[1:]
			q.diskCount--
			q.dropItemUnlocked(dropped)
		}
	}
}

func (q *bufferedQueue) dropItemUnlocked(item Item) {
	q.dropped.Add(1)
	q.metrics.droppedTotal.Inc()
	depth := q.memCount + q.diskCount
	q.log.Warn("queue dropped oldest item",
		logger.F("item_id", item.ID),
		logger.F("depth", depth),
	)
	q.updateDepthMetricsLocked()
}

// DequeueBatch returns up to n items, blocking until at least one is available or ctx is canceled.
func (q *bufferedQueue) DequeueBatch(ctx context.Context, n int) ([]Item, error) {
	if n <= 0 {
		return nil, errors.New("batch size must be positive")
	}

	type pulled struct {
		item     Item
		fromDisk bool
	}

	var pulledItems []pulled

	for len(pulledItems) == 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		q.mu.Lock()
		if q.closed && q.memCount == 0 && q.diskCount == 0 && len(q.memCh) == 0 {
			q.mu.Unlock()
			return nil, nil
		}

		for len(pulledItems) < n {
			item, fromDisk, ok, err := q.dequeueOneUnlocked(ctx)
			if err != nil {
				q.mu.Unlock()
				return nil, err
			}
			if !ok {
				break
			}
			pulledItems = append(pulledItems, pulled{item: item, fromDisk: fromDisk})
		}

		if len(pulledItems) > 0 {
			for _, p := range pulledItems {
				q.inflight[p.item.ID] = &inflightEntry{item: p.item, onDisk: p.fromDisk}
				q.metrics.dequeuedTotal.Inc()
			}
			q.updateDepthMetricsLocked()
			q.mu.Unlock()
			break
		}

		q.waitForCapacity(ctx)
		q.mu.Unlock()
	}

	batch := make([]Item, len(pulledItems))
	for i, p := range pulledItems {
		batch[i] = p.item
	}
	return batch, nil
}

func (q *bufferedQueue) dequeueOneUnlocked(ctx context.Context) (Item, bool, bool, error) {
	if err := ctx.Err(); err != nil {
		return Item{}, false, false, err
	}

	select {
	case item := <-q.memCh:
		q.memBytes -= q.itemSize(item)
		q.memCount--
		q.updateDepthMetricsLocked()
		return item, false, true, nil
	default:
	}

	if len(q.diskHead) > 0 {
		item := q.diskHead[0]
		q.diskHead = q.diskHead[1:]
		q.diskCount--
		q.updateDepthMetricsLocked()
		return item, true, true, nil
	}

	return Item{}, false, false, nil
}

// Ack permanently removes delivered items.
func (q *bufferedQueue) Ack(ids []string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return errors.New("queue is closed")
	}

	for _, id := range ids {
		entry, ok := q.inflight[id]
		if !ok {
			continue
		}
		delete(q.inflight, id)
		q.metrics.ackTotal.Inc()
		if entry.onDisk && q.disk != nil {
			if err := q.disk.appendAck(id); err != nil {
				return fmt.Errorf("persist ack for %q: %w", id, err)
			}
		}
	}
	q.signalWaitersLocked()
	return nil
}

// Nack returns items to the front of the queue for immediate retry.
func (q *bufferedQueue) Nack(ids []string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return errors.New("queue is closed")
	}

	var front []Item
	for _, id := range ids {
		entry, ok := q.inflight[id]
		if !ok {
			continue
		}
		delete(q.inflight, id)
		item := entry.item
		item.Attempts++
		front = append(front, item)
		q.metrics.nackTotal.Inc()
	}

	for i := len(front) - 1; i >= 0; i-- {
		item := front[i]
		size := q.itemSize(item)
		if q.canAcceptInMemory(size) {
			if err := q.enqueueFrontUnlocked(item, size); err != nil {
				return err
			}
			continue
		}
		if q.disk != nil {
			if err := q.prependDiskUnlocked(item); err != nil {
				return err
			}
			continue
		}
		q.dropItemUnlocked(item)
	}

	q.updateDepthMetricsLocked()
	q.signalWaitersLocked()
	return nil
}

func (q *bufferedQueue) enqueueFrontUnlocked(item Item, size int64) error {
	var rest []Item
drain:
	for {
		select {
		case existing := <-q.memCh:
			q.memBytes -= q.itemSize(existing)
			q.memCount--
			rest = append(rest, existing)
		default:
			break drain
		}
	}

	if err := q.putMemoryUnlocked(item, size, false); err != nil {
		for _, r := range rest {
			_ = q.putMemoryUnlocked(r, q.itemSize(r), false)
		}
		return err
	}

	for i := len(rest) - 1; i >= 0; i-- {
		if err := q.putMemoryUnlocked(rest[i], q.itemSize(rest[i]), false); err != nil {
			return err
		}
	}
	return nil
}

func (q *bufferedQueue) prependDiskUnlocked(item Item) error {
	if err := q.disk.appendItem(item); err != nil {
		return fmt.Errorf("nack spill to disk: %w", err)
	}
	q.diskHead = append([]Item{item}, q.diskHead...)
	q.diskCount++
	return nil
}

// Depth returns the combined memory and disk queue depth (excluding in-flight).
func (q *bufferedQueue) Depth() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.memCount + q.diskCount
}

// DroppedTotal returns the number of items dropped since startup.
func (q *bufferedQueue) DroppedTotal() int64 {
	return q.dropped.Load()
}

// Close flushes memory items to disk when spill is enabled and releases resources.
func (q *bufferedQueue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return nil
	}
	q.closed = true

	if q.disk != nil {
		for {
			select {
			case item := <-q.memCh:
				q.memBytes -= q.itemSize(item)
				q.memCount--
				if err := q.disk.appendItem(item); err != nil {
					return fmt.Errorf("flush item on close: %w", err)
				}
				q.diskHead = append(q.diskHead, item)
				q.diskCount++
			default:
				goto closeDisk
			}
		}
	closeDisk:
		if err := q.disk.close(); err != nil {
			return fmt.Errorf("close disk store: %w", err)
		}
	}

	q.signalWaitersLocked()
	q.updateDepthMetricsLocked()
	return nil
}

func (q *bufferedQueue) signalWaitersLocked() {
	q.cond.Broadcast()
}

func (q *bufferedQueue) updateDepthMetrics() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.updateDepthMetricsLocked()
}

func (q *bufferedQueue) updateDepthMetricsLocked() {
	if q.metrics == nil {
		return
	}
	q.metrics.setDepth("memory", q.memCount)
	q.metrics.setDepth("disk", q.diskCount)
}
