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

	"github.com/kubexa/kubexa-agent/internal/logger"
	agentmetrics "github.com/kubexa/kubexa-agent/internal/metrics"
	"github.com/kubexa/kubexa-agent/pkg/config"
)

const (
	// avgItemSizeEstimate converts byte budgets into item counts. Measured
	// agent traffic averages ~180 KB per item, so this is off by roughly 44x
	// and the counts it produces are conservative, never generous. Both tiers
	// have a real bound that does not depend on it: memory is gated by
	// memBytes, and the disk tier by the WAL's own byte cap. It survives only
	// as a slot count.
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

// CapacityAware is implemented by Queue implementations that expose a maximum item capacity.
type CapacityAware interface {
	Queue
	// Capacity returns the maximum number of items the queue can hold across tiers.
	Capacity() int64
}

// inflightEntry holds one dequeued item awaiting ack or nack.
//
// For a disk-sourced item, ref is set and item is left zero: the payload
// stays on disk, and a nack simply puts the reference back. Retaining the
// payload here would reintroduce the very cost this change removes, one
// batch at a time.
type inflightEntry struct {
	item Item
	ref  *diskRef
}

// bufferedQueue implements Queue with an in-memory channel and optional WAL disk spill.
type bufferedQueue struct {
	cfg     *config.BufferConfig
	log     *logger.Logger
	metrics *agentmetrics.QueueMetrics

	mu     sync.Mutex
	cond   *sync.Cond
	closed bool

	memCh     chan Item
	memBytes  int64
	memCount  int64
	diskCount int64
	diskRefs  []diskRef
	disk      *diskStore

	// refsPerSegment counts live references into each WAL segment. Task 7
	// uses it to decide which segments can be deleted. It is incremented when
	// a reference is created and decremented ONLY on ack or drop — a dequeued
	// item still lives in inflight and still pins its segment.
	refsPerSegment map[int]int64
	lowestSegment  int
	// compactFailing is true while segment removal is failing, so the warning
	// is logged on the transition instead of on every attempt.
	// compactFailures counts every failed attempt. Both guarded by mu.
	compactFailing  bool
	compactFailures int64
	// refUnderflows counts releases of a segment claim that was never held.
	// Guarded by mu, like refsPerSegment itself.
	refUnderflows int64

	inflight map[string]*inflightEntry

	dropped atomic.Int64
}

// New constructs a ready-to-use Queue from cfg, logging through log and recording metrics via m.
func New(cfg *config.BufferConfig, log *logger.Logger, m *agentmetrics.QueueMetrics) (Queue, error) {
	if cfg == nil {
		return nil, errors.New("buffer config is nil")
	}
	if err := validateBufferConfig(cfg); err != nil {
		return nil, err
	}
	if log == nil {
		log = logger.New("queue")
	}

	q := &bufferedQueue{
		cfg:            cfg,
		log:            log,
		metrics:        m,
		inflight:       make(map[string]*inflightEntry),
		refsPerSegment: make(map[int]int64),
	}
	q.cond = sync.NewCond(&q.mu)

	slots := memorySlotCapacity(cfg.MaxMemoryBytes)
	q.memCh = make(chan Item, slots)

	if cfg.SpillDir != "" {
		maxDisk := cfg.MaxDiskBytes
		if maxDisk <= 0 {
			maxDisk = 512 << 20
		}
		ds, err := newDiskStore(cfg.SpillDir, maxDisk, log, m)
		if err != nil {
			return nil, fmt.Errorf("init disk spill: %w", err)
		}
		q.disk = ds

		recovered, err := ds.recover()
		if err != nil {
			_ = ds.close()
			return nil, fmt.Errorf("recover spill segments: %w", err)
		}

		// Recovery runs at startup -- exactly the window a mirrored-payload
		// queue used to OOM the pod in -- so the reference cap has to hold
		// here too, not just on the two live-traffic paths. recover() returns
		// refs oldest-first, so the front is what survives and the tail (the
		// newest arrivals) is what gets dropped.
		limit := diskSlotCapacity(maxDisk)
		kept := recovered
		var droppedRefs []diskRef
		if int64(len(recovered)) > limit {
			kept = recovered[:limit]
			droppedRefs = recovered[limit:]
		}

		q.diskRefs = append(q.diskRefs, kept...)
		q.diskCount = int64(len(kept))
		for _, ref := range kept {
			q.refsPerSegment[ref.Segment]++
		}

		// Where compaction starts scanning. The cursor must sit at or below
		// every segment that could still need collecting, so it is seeded from
		// the files actually in the spill directory -- not from the recovered
		// references, and not from the active segment.
		//
		// Those two are both wrong on their own, in the same direction. When
		// every record in the WAL was already acked, recovery returns nothing
		// and the seed would collapse onto the active segment, leaving every
		// older file below the cursor permanently uncollectable: the queue is
		// empty, the disk is full, and no ack can move a cursor that already
		// sits above them. That is precisely the state an upgrade from a
		// pre-compaction binary starts in.
		//
		// Erring low is free -- a few no-op iterations over numbers whose files
		// are gone -- so the minimum is taken over all three sources.
		q.lowestSegment = q.disk.activeSegment()
		if oldest, ok := q.disk.lowestSegmentOnDisk(); ok && oldest < q.lowestSegment {
			q.lowestSegment = oldest
		}
		for _, ref := range recovered {
			if ref.Segment < q.lowestSegment {
				q.lowestSegment = ref.Segment
			}
		}

		for _, ref := range droppedRefs {
			// The WAL record survives unless acked here, so without this the
			// very next restart recovers it again instead of the cap
			// converging on the same kept set.
			q.settleDroppedRefUnlocked(ref)
			q.dropped.Add(1)
			q.metrics.IncDropped()
		}

		if len(recovered) > 0 {
			log.Info("recovered items from disk spill", logger.F("count", len(recovered)))
		}
		if len(droppedRefs) > 0 {
			log.Error("disk reference cap exceeded at recovery; dropped newest-first tail past the cap",
				logger.F("recovered", len(recovered)),
				logger.F("kept", len(kept)),
				logger.F("dropped", len(droppedRefs)),
				logger.F("cap", limit),
				logger.F("raise_limit_via", "buffer.max_disk_bytes"),
			)
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

func diskSlotCapacity(maxDiskBytes int64) int64 {
	if maxDiskBytes <= 0 {
		return 0
	}
	slots := maxDiskBytes / avgItemSizeEstimate
	if slots < 1 {
		return 1
	}
	return slots
}

// Capacity returns the maximum number of items the queue can hold in memory and on disk.
func (q *bufferedQueue) Capacity() int64 {
	if q == nil {
		return 0
	}
	capacity := int64(cap(q.memCh))
	if q.disk != nil {
		maxDisk := q.cfg.MaxDiskBytes
		if maxDisk <= 0 {
			maxDisk = 512 << 20
		}
		capacity += diskSlotCapacity(maxDisk)
	}
	return capacity
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
			if err := q.spillEnqueueUnlocked(item, true); err != nil {
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
			q.metrics.IncEnqueued()
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
			if q.diskRefsFullUnlocked() {
				q.memCh <- oldest
				q.memBytes += q.itemSize(oldest)
				q.memCount++
				return fmt.Errorf("spill item to disk: %w (reference cap)", ErrDiskFull)
			}
			segment, offset, err := q.disk.appendItem(oldest)
			if err != nil {
				q.memCh <- oldest
				q.memBytes += q.itemSize(oldest)
				q.memCount++
				return fmt.Errorf("spill item to disk: %w", err)
			}
			q.addDiskRefUnlocked(diskRef{
				ID:         oldest.ID,
				Segment:    segment,
				Offset:     offset,
				EnqueuedAt: oldest.EnqueuedAt,
				Attempts:   oldest.Attempts,
			})
			q.updateDepthMetricsLocked()
			return nil
		}
		q.dropItemUnlocked(oldest)
		return nil
	default:
		return nil
	}
}

// spillEnqueueUnlocked writes item to the WAL and keeps only a reference to it.
//
// countEnqueue mirrors putMemoryUnlocked: a genuine arrival counts, a nacked
// item coming back does not. A retry is not an arrival, and counting it makes
// enqueued_total drift upward on every failed send.
func (q *bufferedQueue) spillEnqueueUnlocked(item Item, countEnqueue bool) error {
	if q.diskRefsFullUnlocked() {
		return fmt.Errorf("%w (reference cap)", ErrDiskFull)
	}
	segment, offset, err := q.disk.appendItem(item)
	if err != nil {
		return err
	}
	q.addDiskRefUnlocked(diskRef{
		ID:         item.ID,
		Segment:    segment,
		Offset:     offset,
		EnqueuedAt: item.EnqueuedAt,
		Attempts:   item.Attempts,
	})
	if countEnqueue {
		q.metrics.IncEnqueued()
	}
	q.updateDepthMetricsLocked()
	return nil
}

// diskRefsFullUnlocked reports whether the reference slice has reached the
// item count the disk budget allows.
//
// References are small but not free (~100 bytes each), and this is the bound
// the old design was missing entirely: diskHead had no accounting of any
// kind, so max_disk_bytes silently became a heap ceiling. diskSlotCapacity
// already existed but gated nothing; it gates this.
//
// With no disk tier there is no reference slice to fill, so this reports not
// full. Every call site today only reaches this with q.disk already non-nil;
// the check exists so the answer stays correct if that ever stops being true,
// rather than silently refusing callers that have nothing to be full of.
func (q *bufferedQueue) diskRefsFullUnlocked() bool {
	if q.disk == nil {
		return false
	}
	maxDisk := q.cfg.MaxDiskBytes
	if maxDisk <= 0 {
		maxDisk = 512 << 20
	}
	return int64(len(q.diskRefs)) >= diskSlotCapacity(maxDisk)
}

// addDiskRefUnlocked appends a reference and charges it to its segment.
func (q *bufferedQueue) addDiskRefUnlocked(ref diskRef) {
	q.diskRefs = append(q.diskRefs, ref)
	q.diskCount++
	q.refsPerSegment[ref.Segment]++
}

// releaseDiskRefUnlocked drops a segment's claim on one reference. Called
// only when the item is permanently gone: acked or dropped. Dequeue must not
// call this — the item is still live in inflight.
//
// A release of a claim that was never held means some path decremented twice,
// which is exactly the accounting error that lets compaction delete a segment
// whose records are still needed. Decrementing blindly would park that at -1
// and then erase the evidence when the key is deleted, so it is counted,
// logged, and clamped at zero instead of being allowed to propagate.
func (q *bufferedQueue) releaseDiskRefUnlocked(segment int) {
	count, held := q.refsPerSegment[segment]
	if !held || count <= 0 {
		q.refUnderflows++
		q.log.Error("disk ref release underflow",
			logger.F("segment", segment),
			logger.F("count", count),
			logger.F("clamped_to", 0),
			logger.F("underflows", q.refUnderflows),
		)
		delete(q.refsPerSegment, segment)
		return
	}
	count--
	if count == 0 {
		delete(q.refsPerSegment, segment)
		return
	}
	q.refsPerSegment[segment] = count
}

// compactUnlocked deletes the contiguous run of oldest segments that no live
// reference points into.
//
// Prefix only, never a hole. An ack record is always written at or after its
// item's own segment, so removing a prefix always removes acks together with
// the items they refer to. Punching a hole can delete an ack whose item is
// still on disk further back, and the next restart would replay that item as
// pending and deliver it a second time — silently, with nothing logged
// anywhere. This is why the loop stops at the first segment that still holds a
// claim instead of scanning on for other empty ones.
//
// A zero count is necessary but not sufficient: the active write segment
// normally carries no claims at all and must never be unlinked, which is what
// the `< active` bound is for. removeSegment refuses it a second time.
//
// A claim is released only on ack or drop, never on dequeue, so an item sitting
// in inflight still pins its segment and everything behind it.
//
// lowestSegment advances monotonically and is never moved past a segment that
// was not actually removed, so this is amortized O(1) per call rather than a
// scan, and a failed removal is retried on the next call instead of skipped.
func (q *bufferedQueue) compactUnlocked() {
	if q.disk == nil {
		return
	}

	active := q.disk.activeSegment()
	for q.lowestSegment < active && q.refsPerSegment[q.lowestSegment] == 0 {
		if err := q.disk.removeSegment(q.lowestSegment); err != nil {
			q.noteCompactionFailureUnlocked(q.lowestSegment, err)
			return
		}
		// A no-op today: a claim's key is deleted the moment its count
		// reaches zero, so the loop condition above already implies the key
		// is absent. It stays as map hygiene -- a stale zero entry from any
		// future path would otherwise accumulate one key per retired segment
		// for the life of the process.
		delete(q.refsPerSegment, q.lowestSegment)
		q.lowestSegment++
		q.noteCompactionSuccessUnlocked()
	}
}

// noteCompactionFailureUnlocked reports a segment that would not delete,
// logging the transition rather than the occurrence.
//
// This runs from both reclaim points, so on a spill directory that has gone
// read-only it would otherwise fire on every ack and every dequeue -- hundreds
// of megabytes a day of agent log, flowing into the same collection pipeline as
// the real diagnostics and rotating them out. The agent's own telemetry
// becoming the outage is a failure mode this project has hit before. One line
// when reclamation starts failing, one when it recovers, and a count in both.
//
// A closed store is not a failure: compaction races Close by design.
func (q *bufferedQueue) noteCompactionFailureUnlocked(segment int, err error) {
	if errors.Is(err, errStoreClosed) {
		return
	}
	q.compactFailures++
	if q.compactFailing {
		return
	}
	q.compactFailing = true
	q.log.Error("WAL segment reclamation is failing; the spill directory will "+
		"grow to max_disk_bytes and stay there",
		logger.F("segment", segment),
		logger.F("error", err.Error()),
	)
}

func (q *bufferedQueue) noteCompactionSuccessUnlocked() {
	if !q.compactFailing {
		return
	}
	q.compactFailing = false
	q.log.Info("WAL segment reclamation recovered",
		logger.F("failed_attempts", q.compactFailures),
	)
}

func (q *bufferedQueue) dropOldestUnlocked() {
	select {
	case oldest := <-q.memCh:
		q.memBytes -= q.itemSize(oldest)
		q.memCount--
		q.dropItemUnlocked(oldest)
	default:
		if len(q.diskRefs) > 0 {
			ref := q.diskRefs[0]
			q.diskRefs = q.diskRefs[1:]
			q.diskCount--
			q.releaseDiskRefUnlocked(ref.Segment)
			q.settleDroppedRefUnlocked(ref)
			q.dropItemUnlocked(Item{ID: ref.ID})
		}
	}
}

// settleDroppedRefUnlocked records an ack for an item the queue is dropping,
// so recovery treats it as finished.
//
// Without this the WAL record survives the drop: the next process start
// replays it, the "dropped" item comes back, and dropped_total describes
// something that did not happen. The ack is best effort — if the WAL cannot
// take it the item does resurrect, which is the pre-existing behaviour, but it
// is no longer silent.
func (q *bufferedQueue) settleDroppedRefUnlocked(ref diskRef) {
	if q.disk == nil {
		return
	}
	if err := q.disk.appendAck(ref.ID); err != nil {
		q.log.Error("could not record ack for dropped spilled item; it will "+
			"return after a restart",
			logger.F("item_id", ref.ID),
			logger.F("segment", ref.Segment),
			logger.F("error", err.Error()),
		)
	}
}

func (q *bufferedQueue) dropItemUnlocked(item Item) {
	q.dropped.Add(1)
	q.metrics.IncDropped()
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
		item Item
		ref  *diskRef
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
			item, ref, ok, err := q.dequeueOneUnlocked(ctx)
			if err != nil {
				// Anything already pulled has left diskRefs and the memory
				// channel and is not yet in inflight, so returning the error
				// here would lose it: a memory item outright, a disk item
				// until the next process restart replays the WAL. Stop
				// pulling, register what we have, and hand it back with no
				// error — the caller sees the cancellation on its next call.
				if len(pulledItems) > 0 {
					break
				}
				q.mu.Unlock()
				return nil, err
			}
			if !ok {
				break
			}
			pulledItems = append(pulledItems, pulled{item: item, ref: ref})
		}

		// The second reclaim point. Ack is not the only path that releases the
		// last claim on a segment: readRefUnlocked drops an unreadable record
		// inside the pull loop above and releases it there, and no ack for that
		// item will ever arrive. A queue whose every remaining record is corrupt
		// would otherwise sit on a full disk forever. Cheap when there is
		// nothing to reclaim — one map lookup, and no directory read unless a
		// file was actually unlinked.
		//
		// Items already pulled here still hold their segment claims (a claim is
		// released on ack or drop, never on dequeue), so nothing below can be
		// deleted out from under a reference that is still live.
		q.compactUnlocked()

		if len(pulledItems) > 0 {
			for _, p := range pulledItems {
				entry := &inflightEntry{}
				if p.ref != nil {
					// Disk-sourced: keep the reference, not the payload.
					entry.ref = p.ref
				} else {
					entry.item = p.item
				}
				q.inflight[p.item.ID] = entry
				q.metrics.IncDequeued()
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

// dequeueOneUnlocked returns the next item, preferring the disk tier.
//
// Spilled items are by definition older than resident ones — they were
// evicted to make room for what is in memory now — so reading memory first
// starves them, and a disk tier that never drains is a disk tier whose
// segments can never be compacted.
func (q *bufferedQueue) dequeueOneUnlocked(ctx context.Context) (Item, *diskRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return Item{}, nil, false, err
	}

	for len(q.diskRefs) > 0 {
		ref := q.diskRefs[0]
		q.diskRefs = q.diskRefs[1:]
		q.diskCount--
		q.updateDepthMetricsLocked()

		item, ok := q.readRefUnlocked(ref)
		if !ok {
			continue
		}
		item.Attempts = ref.Attempts
		return item, &ref, true, nil
	}

	select {
	case item := <-q.memCh:
		q.memBytes -= q.itemSize(item)
		q.memCount--
		q.updateDepthMetricsLocked()
		return item, nil, true, nil
	default:
	}

	return Item{}, nil, false, nil
}

// readRefUnlocked fetches a spilled payload. An unreadable record is counted
// as dropped and skipped: a corrupt byte range must not wedge the queue
// behind an item that can never be delivered.
func (q *bufferedQueue) readRefUnlocked(ref diskRef) (Item, bool) {
	start := time.Now()
	item, err := q.disk.readItem(ref.Segment, ref.Offset)
	q.metrics.ObserveDiskRead(time.Since(start).Seconds())
	if err != nil {
		q.log.Error("drop unreadable spilled item",
			logger.F("item_id", ref.ID),
			logger.F("segment", ref.Segment),
			logger.F("offset", ref.Offset),
			logger.F("error", err.Error()),
		)
		q.metrics.IncDiskReadError()
		q.releaseDiskRefUnlocked(ref.Segment)
		q.settleDroppedRefUnlocked(ref)
		q.dropItemUnlocked(Item{ID: ref.ID})
		return Item{}, false
	}
	return item, true
}

// Ack permanently removes delivered items.
func (q *bufferedQueue) Ack(ids []string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return errors.New("queue is closed")
	}

	// An ack is only recorded once it is durable. The inflight entry is what
	// justifies holding the segment claim, so removing the entry while the
	// claim stays held -- which is what happens if the WAL write fails
	// midway -- pins that segment and the entire prefix behind it for the
	// lifetime of the process, with nothing left that could ever release it.
	//
	// The alternative shape, releasing the claim anyway, is worse: the WAL
	// record survives an ack that was never written, so the item comes back
	// on the next restart, and its segment may have been compacted away in
	// the meantime. Leaving the entry inflight keeps the queue's own rule --
	// a claim is held exactly as long as something live can release it --
	// and leaves the caller free to retry the ack or nack the item back into
	// the queue, both of which already work.
	var ackErr error
	for _, id := range ids {
		entry, ok := q.inflight[id]
		if !ok {
			continue
		}
		if entry.ref != nil && q.disk != nil {
			if err := q.disk.appendAck(id); err != nil {
				ackErr = fmt.Errorf("persist ack for %q: %w", id, err)
				break
			}
			q.releaseDiskRefUnlocked(entry.ref.Segment)
		}
		delete(q.inflight, id)
		q.metrics.IncAck()
	}
	// The main reclaim point: acks are what retire WAL records, and without a
	// reclaim the log only ever grew until appendRecord returned ErrDiskFull
	// permanently. Compaction runs before the broadcast so an enqueuer parked
	// on ErrDiskFull wakes up to space that already exists.
	// Runs even when the ack failed above: a failing ack usually means a full
	// disk, and compaction is the only thing that can make room. Returning
	// early here would skip both the reclaim and the broadcast, leaving an
	// enqueuer parked on ErrDiskFull with nothing left to wake it.
	q.compactUnlocked()
	q.signalWaitersLocked()
	return ackErr
}

// Nack returns items to the front of the queue for immediate retry.
func (q *bufferedQueue) Nack(ids []string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return errors.New("queue is closed")
	}

	var frontMemory []Item
	var frontRefs []diskRef
	for _, id := range ids {
		entry, ok := q.inflight[id]
		if !ok {
			continue
		}
		delete(q.inflight, id)
		q.metrics.IncNack(1)
		if entry.ref != nil {
			ref := *entry.ref
			ref.Attempts++
			frontRefs = append(frontRefs, ref)
			continue
		}
		item := entry.item
		item.Attempts++
		frontMemory = append(frontMemory, item)
	}

	// References first so they end up ahead of everything, preserving the
	// disk-before-memory order the dequeue path relies on. No WAL write here:
	// the record is already there, and the segment claim was never released.
	for i := len(frontRefs) - 1; i >= 0; i-- {
		q.diskRefs = append([]diskRef{frontRefs[i]}, q.diskRefs...)
		q.diskCount++
	}

	for i := len(frontMemory) - 1; i >= 0; i-- {
		item := frontMemory[i]
		size := q.itemSize(item)
		if q.canAcceptInMemory(size) {
			if err := q.enqueueFrontUnlocked(item, size); err != nil {
				return err
			}
			continue
		}
		if q.disk != nil {
			// countEnqueue=false, matching the memory branch above: this item
			// already counted as an enqueue when it first arrived.
			if err := q.spillEnqueueUnlocked(item, false); err != nil {
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
		var closeDropped int
		for {
			select {
			case item := <-q.memCh:
				q.memBytes -= q.itemSize(item)
				q.memCount--
				if q.diskRefsFullUnlocked() {
					// Nothing was ever written for this item -- it never got a
					// WAL record or a ref -- so there is nothing to ack. It is
					// simply lost, which is why it is counted.
					q.dropped.Add(1)
					q.metrics.IncDropped()
					closeDropped++
					continue
				}
				segment, offset, err := q.disk.appendItem(item)
				if err != nil {
					return fmt.Errorf("flush item on close: %w", err)
				}
				q.addDiskRefUnlocked(diskRef{
					ID:         item.ID,
					Segment:    segment,
					Offset:     offset,
					EnqueuedAt: item.EnqueuedAt,
					Attempts:   item.Attempts,
				})
			default:
				goto closeDisk
			}
		}
	closeDisk:
		if closeDropped > 0 {
			q.log.Error("dropped items at shutdown flush: reference cap reached",
				logger.F("dropped", closeDropped),
			)
		}
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
	q.metrics.SetDepth("memory", float64(q.memCount))
	q.metrics.SetDepth("disk", float64(q.diskCount))
}
