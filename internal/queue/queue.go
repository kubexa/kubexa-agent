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

	// maxDeliveryAttempts caps how many times one item may be handed back for
	// redelivery before the queue retires it.
	//
	// Attempts was incremented on every nack and read by nobody, so an item
	// the gateway could never take -- or one whose ack could never be
	// persisted -- came back forever. The drain loop nacks whatever it cannot
	// settle, so "forever" is a tight loop: duplicate traffic, a pegged core,
	// and a log line per round, all of it flowing into the same pipeline as
	// the agent's real diagnostics.
	//
	// Six is chosen against what an attempt actually costs. Attempts do not
	// accumulate during an outage -- with no session there is no drain, and
	// the whole buffer waits -- so they only count real delivery failures
	// against a live gateway. A transient one clears well inside six tries
	// (the send path's own circuit breaker gives up far sooner), while a
	// poison item costs at most six re-sends before it is counted and gone.
	maxDeliveryAttempts = 6
)

// ErrClosed is returned by every operation once the queue is closed. Callers
// use it to tell "this queue can no longer settle anything" (shutdown, nothing
// to fix) apart from a failure worth reporting and retrying.
var ErrClosed = errors.New("queue is closed")

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
	// Items past the delivery-attempt cap are retired instead of returned.
	Nack(ids []string) error

	// NackDelivered returns items whose delivery succeeded but whose ack could
	// not be recorded. Identical to Nack except in the accounting: retiring one
	// of these at the attempt cap is not data loss, because the gateway has the
	// data -- only our record of it failed.
	NackDelivered(ids []string) error

	// NackUntried returns items that were dequeued but never handed to the
	// wire -- everything queued behind the one item whose send failed. They are
	// requeued without charging an attempt, because none was made: an attempt
	// count must record attempts that actually happened, and charging the
	// untried remainder let one poison item at the head of a batch retire up to
	// buffer.batch_size healthy items at the cap.
	NackUntried(ids []string) error

	// NackInflight returns every item that is dequeued but neither acked nor
	// nacked, and reports how many. Called when a session ends: once delivery
	// is settled by a gateway ack rather than by Send returning nil, items sit
	// inflight across the session boundary and nothing else would return them.
	//
	// Does not charge a delivery attempt. A session-end sweep means the
	// transport was cut, not that the gateway declined to ack -- it never got
	// the chance -- so this is not evidence against the item. Charging it
	// anyway would retire a healthy item caught in a routine transport cut
	// (Cloudflare cuts these streams at ~150s of wall clock) after about 17
	// minutes of an otherwise-fine gateway, which is the data loss this
	// branch exists to stop. NackInflightOlderThan is what still retires a
	// genuinely undeliverable item.
	NackInflight() (int, error)

	// NackInflightOlderThan returns inflight items dequeued more than d ago.
	// The backstop for an ack that never arrives, which would otherwise pin a
	// WAL segment for the lifetime of the process.
	//
	// Unlike NackInflight, this charges a delivery attempt: the gateway had a
	// full d to ack and did not, so -- unlike a session-end sweep -- the
	// missing ack is evidence against the item, not just against the
	// transport. This is what still retires a genuinely poison item, at
	// maxDeliveryAttempts+1 sweeps.
	NackInflightOlderThan(d time.Duration) (int, error)

	// Drop settles items without redelivering them, counting each as dropped.
	// It is for items the caller has judged undeliverable, where a nack would
	// replay them forever; it is on the interface because the caller has no
	// other way to release an item it must not retry.
	Drop(ids []string) error

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

// refReadResult says how a spilled payload read ended, so the dequeue path can
// tell a corrupt record (drop it, the queue must not wedge) from a closed store
// (keep it, this process is simply done).
type refReadResult int

const (
	refReadOK refReadResult = iota
	refReadDropped
	refReadStoreClosed
)

// inflightEntry holds one dequeued item awaiting ack or nack.
//
// For a disk-sourced item, ref is set and item is left zero: the payload
// stays on disk, and a nack simply puts the reference back. Retaining the
// payload here would reintroduce the very cost this change removes, one
// batch at a time.
type inflightEntry struct {
	item Item
	ref  *diskRef
	// since is when the item was dequeued. It exists because an item now waits
	// for a remote ack: an ack that never arrives must eventually be swept, and
	// an unacked item pins its WAL segment and the whole prefix behind it in
	// the meantime.
	since time.Time
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
	// unrecorded counts items retired at the attempt cap that had actually
	// been delivered. They are not losses and must not be reported as such:
	// dropped_total is what an operator reads to decide whether telemetry went
	// missing, and these arrived.
	unrecorded atomic.Int64
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
		oldest, ok, err := q.disk.lowestSegmentOnDisk()
		if err != nil {
			// Refusing to start is the same answer recover() gives to the same
			// failure two statements above. Carrying on would seed the cursor
			// from the active segment alone -- the exact bug this seeding
			// exists to fix -- and disable compaction silently.
			_ = ds.close()
			return nil, fmt.Errorf("scan spill segments: %w", err)
		}
		if ok && oldest < q.lowestSegment {
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
			return ErrClosed
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
		q.dropItemUnlocked(oldest, "no_disk_spill")
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
		q.dropItemUnlocked(oldest, "capacity")
	default:
		if len(q.diskRefs) > 0 {
			ref := q.diskRefs[0]
			q.diskRefs = q.diskRefs[1:]
			q.diskCount--
			q.releaseDiskRefUnlocked(ref.Segment)
			q.settleDroppedRefUnlocked(ref)
			q.dropItemUnlocked(Item{ID: ref.ID}, "capacity")
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

// dropItemUnlocked counts and logs one lost item. reason says which pressure
// lost it -- capacity eviction, an unreadable WAL record, a requeue with
// nowhere to go -- because "queue dropped oldest item" was already being
// logged by paths that had nothing to do with the oldest item.
func (q *bufferedQueue) dropItemUnlocked(item Item, reason string) {
	q.dropped.Add(1)
	q.metrics.IncDropped()
	depth := q.memCount + q.diskCount
	q.log.Warn("queue dropped item",
		logger.F("item_id", item.ID),
		logger.F("reason", reason),
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
		// Pending disk refs no longer keep a closed queue open once the store
		// itself is closed: they cannot be read in this process, and without
		// this the call would fall through to the pull loop, retrieve nothing,
		// and park in waitForCapacity until its context is canceled.
		if q.closed && q.memCount == 0 && len(q.memCh) == 0 &&
			(q.diskCount == 0 || (q.disk != nil && q.disk.isClosed())) {
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
				entry := &inflightEntry{since: time.Now()}
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

		item, res := q.readRefUnlocked(ref)
		switch res {
		case refReadDropped:
			continue
		case refReadStoreClosed:
			// Put it back rather than swallow it: nothing is wrong with the
			// record, and its claim was never released. The depth this
			// restores is the truth -- those items are pending on disk.
			q.diskRefs = append([]diskRef{ref}, q.diskRefs...)
			q.diskCount++
			q.updateDepthMetricsLocked()
			return Item{}, nil, false, nil
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
//
// A closed store is not an unreadable record. It means this process is finished
// with the WAL, and every ref still held is durable and unacked -- the next
// start replays it. Counting those as drops would make an ordinary shutdown
// report data loss in dropped_total and disk_read_errors, and log one "drop
// unreadable spilled item" line per pending item on the way out.
func (q *bufferedQueue) readRefUnlocked(ref diskRef) (Item, refReadResult) {
	start := time.Now()
	item, err := q.disk.readItem(ref.Segment, ref.Offset)
	q.metrics.ObserveDiskRead(time.Since(start).Seconds())
	if errors.Is(err, errStoreClosed) {
		return Item{}, refReadStoreClosed
	}
	// A reference is a byte location, and readItem validates the record type
	// but cannot know which item was asked for -- only this layer holds the
	// ref. Should an offset ever drift (a torn write, a bad recovery, a future
	// refactor), the read succeeds on a perfectly valid record belonging to
	// somebody else: the wrong payload goes to the gateway, and the ack that
	// follows retires an id that was never sent, with no error anywhere in the
	// sequence. Checked here so a mismatch becomes what it is -- an unreadable
	// record: counted, dropped, and never delivered.
	if err == nil && item.ID != ref.ID {
		err = fmt.Errorf("%w: wanted %q, record holds %q", errRecordIDMismatch, ref.ID, item.ID)
		item = Item{}
	}
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
		q.dropItemUnlocked(Item{ID: ref.ID}, "unreadable")
		return Item{}, refReadDropped
	}
	return item, refReadOK
}

// Ack permanently removes delivered items.
func (q *bufferedQueue) Ack(ids []string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return ErrClosed
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
	// Ack removes entries from q.inflight, so it is a mutation the oldest-
	// inflight-age gauge has to see. Without this, acking the last inflight
	// item leaves the gauge parked at its pre-ack value instead of dropping to
	// zero -- stale in exactly the direction that hides a real problem
	// clearing up.
	q.updateDepthMetricsLocked()
	q.signalWaitersLocked()
	return ackErr
}

// Nack returns items to the front of the queue for immediate retry.
//
// Every id it is given is settled: requeued, or retired through the counted
// drop path. Returning early on the first requeue failure left the rest of the
// call's ids neither requeued nor counted -- gone, with dropped_total unmoved
// and nothing logged. An id that reaches this function has already left the
// inflight map, so there is no other holder that could settle it later.
//
// An item past maxDeliveryAttempts is retired here rather than requeued. Nack
// is the one choke point every redelivery passes through; a cap in the caller
// would leave every other nack path uncapped.
func (q *bufferedQueue) Nack(ids []string) error {
	return q.nack(ids, nackFailed)
}

// NackInflight returns every unsettled item to the queue. Called from a
// session-end sweep, where the transport was cut out from under the item
// rather than the gateway declining to ack it -- so, unlike
// NackInflightOlderThan, this does not charge a delivery attempt. See
// nackSessionEnd for why.
func (q *bufferedQueue) NackInflight() (int, error) {
	return q.sweepInflight(0, nackSessionEnd)
}

// NackInflightOlderThan returns unsettled items dequeued more than d ago.
// d <= 0 sweeps everything. This is the deadline sweep: the gateway had a
// full d to ack and did not, so it charges a delivery attempt like any other
// failed send (nackFailed) -- the retire cap is what still catches a
// genuinely poison item.
func (q *bufferedQueue) NackInflightOlderThan(d time.Duration) (int, error) {
	return q.sweepInflight(d, nackFailed)
}

// sweepInflight collects unsettled items dequeued more than d ago (d <= 0
// sweeps everything) and nacks them as kind. Shared by NackInflight and
// NackInflightOlderThan so there is exactly one place that walks q.inflight
// for a sweep; only the nackKind -- and so whether the sweep charges a
// delivery attempt -- differs between the two callers.
func (q *bufferedQueue) sweepInflight(d time.Duration, kind nackKind) (int, error) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return 0, ErrClosed
	}
	cutoff := time.Now().Add(-d)
	ids := make([]string, 0, len(q.inflight))
	for id, entry := range q.inflight {
		if d > 0 && entry.since.After(cutoff) {
			continue
		}
		ids = append(ids, id)
	}
	q.mu.Unlock()

	if len(ids) == 0 {
		// A sweep that finds nothing stale still has to refresh the gauge. It
		// is otherwise set only on queue mutation (updateDepthMetricsLocked),
		// so an item stuck inflight on an otherwise quiet queue -- no new
		// enqueues, no acks arriving -- would report whatever age it had at
		// its last mutation forever, exactly the case with no other signal
		// that this gauge exists for. Called via the method, not the loop
		// inline, so this stays the one place that walks q.inflight for age.
		if q.metrics != nil {
			q.metrics.SetOldestInflightAge(q.OldestInflightAge().Seconds())
		}
		return 0, nil
	}
	// nack takes the lock itself and settles every id it is given, including
	// retiring any past the attempt cap through the counted drop path.
	if err := q.nack(ids, kind); err != nil {
		return len(ids), err
	}
	return len(ids), nil
}

// OldestInflightAge reports how long the oldest unsettled item has been
// waiting, or zero when nothing is inflight. An unacked item pins its WAL
// segment and the whole prefix behind it, so ack latency now gates compaction
// -- this is what makes that visible.
//
// Deliberately not on the Queue interface, same standing as InflightLen: an
// observation hook, not a capability callers depend on.
func (q *bufferedQueue) OldestInflightAge() time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	oldest := q.oldestInflightLocked()
	if oldest.IsZero() {
		return 0
	}
	return time.Since(oldest)
}

// oldestInflightLocked returns the earliest since among q.inflight, or the
// zero Time when nothing is inflight. Callers must hold q.mu. Shared by
// OldestInflightAge and updateDepthMetricsLocked so there is exactly one place
// that walks the inflight map for age, not two copies that can drift apart.
func (q *bufferedQueue) oldestInflightLocked() time.Time {
	var oldest time.Time
	for _, entry := range q.inflight {
		if oldest.IsZero() || entry.since.Before(oldest) {
			oldest = entry.since
		}
	}
	return oldest
}

// NackDelivered is Nack for items that reached the gateway but whose ack could
// not be persisted. The queue cannot tell the two cases apart on its own -- it
// never sees the wire -- so the caller says which it is, and the difference
// shows up only if the item is later retired at the attempt cap: a retirement
// after delivery counts as delivered-but-unrecorded, not as a drop.
//
// The flag describes the most recent attempt, which is the right question at
// retirement time: it decides whether the data the queue is giving up on ever
// left the agent.
func (q *bufferedQueue) NackDelivered(ids []string) error {
	return q.nack(ids, nackDelivered)
}

// NackUntried returns items that were dequeued but never sent, so they are
// requeued without charging an attempt.
//
// The drain hands back the item whose send failed together with everything
// queued behind it, and only the first of those was ever attempted. Charging
// the whole slice made one poison item at the head cost the entire batch:
// six rounds and all of it was in dropped_total, measured 10 items for 1. It
// also falsified the cap's own promise above -- a poison item costs at most
// six re-sends "and is counted and gone", not six re-sends of everything
// behind it.
//
// The untried items still keep their attempt history: an item that used five
// attempts in earlier rounds comes back with five, and the sixth real failure
// still retires it.
func (q *bufferedQueue) NackUntried(ids []string) error {
	return q.nack(ids, nackUntried)
}

// nackKind says what happened to the items being handed back. It decides two
// things: whether they are charged the delivery attempt, and how a retirement
// at the cap is counted.
type nackKind int

const (
	// nackFailed: the send was attempted and it failed. Charges the attempt;
	// a retirement is data loss.
	nackFailed nackKind = iota
	// nackDelivered: the item reached the gateway and only our record of it
	// failed. Charges the attempt; a retirement is not data loss.
	nackDelivered
	// nackUntried: the item never reached the wire. Charges nothing.
	nackUntried
	// nackSessionEnd: the item was inflight when a session ended (transport
	// cut, gateway restart, ...) and is swept back by NackInflight. The
	// gateway never got the ten minutes NackInflightOlderThan's deadline
	// sweep would give it -- the session ended out from under it -- so the
	// missing ack is not its fault. Charges nothing, same as nackUntried,
	// but is kept as its own kind rather than reusing nackUntried so metrics
	// can still tell "never sent" apart from "sent, cut before it could be
	// acked" (see IncNackUncharged).
	//
	// Charging this would be double jeopardy for a healthy item caught in a
	// transport cut: Cloudflare cuts these streams at ~150s of wall clock
	// however busy they are, so an item that is merely inflight at the cut
	// would be charged an attempt on every cut and retire as a counted drop
	// after about 17 minutes of a gateway that is simply slow to ack --
	// exactly the telemetry this branch exists to stop losing. The deadline
	// sweep (nackFailed) is what still retires a genuinely poison item, at
	// ~70 minutes (7 * the 10-minute deadline) instead.
	nackSessionEnd
)

func (q *bufferedQueue) nack(ids []string, kind nackKind) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return ErrClosed
	}

	delivered := kind == nackDelivered
	charge := kind != nackUntried && kind != nackSessionEnd
	uncharged := kind == nackSessionEnd

	var frontMemory []Item
	var frontRefs []diskRef
	for _, id := range ids {
		entry, ok := q.inflight[id]
		if !ok {
			continue
		}
		delete(q.inflight, id)
		if entry.ref != nil {
			ref := *entry.ref
			if charge {
				ref.Attempts++
			}
			if ref.Attempts > maxDeliveryAttempts {
				q.retireInflightUnlocked(id, entry, ref.Attempts, delivered)
				continue
			}
			// Counted here rather than at the top of the loop: nack_total is
			// documented as requeued items, and an item retired above is not
			// requeued -- it is gone, and dropped_total already carries it.
			// Counting both made the requeue rate unreadable in exactly the
			// incident it is read during.
			q.metrics.IncNack(1)
			if uncharged {
				q.metrics.IncNackUncharged(1)
			}
			frontRefs = append(frontRefs, ref)
			continue
		}
		item := entry.item
		if charge {
			item.Attempts++
		}
		if item.Attempts > maxDeliveryAttempts {
			q.retireInflightUnlocked(id, entry, item.Attempts, delivered)
			continue
		}
		q.metrics.IncNack(1)
		if uncharged {
			q.metrics.IncNackUncharged(1)
		}
		frontMemory = append(frontMemory, item)
	}

	// References first so they end up ahead of everything, preserving the
	// disk-before-memory order the dequeue path relies on. No WAL write here:
	// the record is already there, and the segment claim was never released.
	for i := len(frontRefs) - 1; i >= 0; i-- {
		q.diskRefs = append([]diskRef{frontRefs[i]}, q.diskRefs...)
		q.diskCount++
	}

	var requeueErr error
	for i := len(frontMemory) - 1; i >= 0; i-- {
		item := frontMemory[i]
		size := q.itemSize(item)
		if q.canAcceptInMemory(size) {
			err := q.enqueueFrontUnlocked(item, size)
			if err == nil {
				continue
			}
			if requeueErr == nil {
				requeueErr = err
			}
		}
		if q.disk != nil {
			// countEnqueue=false, matching the memory branch above: this item
			// already counted as an enqueue when it first arrived.
			err := q.spillEnqueueUnlocked(item, false)
			if err == nil {
				continue
			}
			if requeueErr == nil {
				requeueErr = err
			}
		}
		// Nowhere left to put it. The item is gone either way; counting and
		// logging it is the whole difference between a drop and a silent
		// disappearance. With no disk configured this is the designed
		// behaviour rather than a failure, so it reports no error.
		q.dropItemUnlocked(item, "requeue_failed")
	}

	// Compaction runs even when a requeue failed: those failures are what a
	// full disk looks like, and reclaiming a spent segment is the only thing
	// that can make room for the next one.
	q.compactUnlocked()
	q.updateDepthMetricsLocked()
	q.signalWaitersLocked()
	return requeueErr
}

// retireInflightUnlocked settles an inflight entry that will never be
// delivered: it has already left the inflight map, so this releases its WAL
// claim, records an ack so recovery does not resurrect it, and counts it once.
//
// Releasing the claim here is what keeps the "decrement only on ack or drop"
// rule intact -- this is the drop -- and it is the only reason compaction can
// ever move past a segment holding a poison record.
func (q *bufferedQueue) retireInflightUnlocked(id string, entry *inflightEntry, attempts int, delivered bool) {
	if entry.ref != nil {
		q.releaseDiskRefUnlocked(entry.ref.Segment)
		q.settleDroppedRefUnlocked(*entry.ref)
	}
	if delivered {
		// Not a loss. The gateway has this data; what failed was writing our
		// own record of the delivery, and giving up on the record is the whole
		// point of the cap. Counting it in dropped_total would tell an operator
		// they lost telemetry they did not lose.
		q.unrecorded.Add(1)
		q.metrics.IncDeliveredUnrecorded()
		q.log.Warn("retiring a delivered item whose ack could not be recorded",
			logger.F("item_id", id),
			logger.F("attempts", attempts),
			logger.F("max_attempts", maxDeliveryAttempts),
		)
		q.updateDepthMetricsLocked()
		return
	}
	q.dropped.Add(1)
	q.metrics.IncDropped()
	q.log.Warn("retiring an undeliverable queued item",
		logger.F("item_id", id),
		logger.F("attempts", attempts),
		logger.F("max_attempts", maxDeliveryAttempts),
	)
	q.updateDepthMetricsLocked()
}

// Drop settles ids without redelivering them: they leave the inflight map,
// their WAL claims are released, an ack record is written so recovery does not
// bring them back, and each counts once in dropped_total.
//
// It exists for the items a caller has judged undeliverable -- an unparseable
// payload, a record too old to be accepted, a stream the config has turned off.
// Those are acked in the normal path precisely because a nack would replay them
// forever; when the ack cannot be persisted, this is the settlement that keeps
// that promise instead of putting them back on the queue.
func (q *bufferedQueue) Drop(ids []string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return ErrClosed
	}

	for _, id := range ids {
		entry, ok := q.inflight[id]
		if !ok {
			continue
		}
		delete(q.inflight, id)
		attempts := entry.item.Attempts
		if entry.ref != nil {
			// A disk-sourced entry keeps its attempt count on the reference:
			// the item field is deliberately empty there.
			attempts = entry.ref.Attempts
		}
		// delivered=false: Drop is for items judged undeliverable here, so
		// they are genuine losses and belong in dropped_total.
		q.retireInflightUnlocked(id, entry, attempts, false)
	}

	// Retiring a disk-sourced item releases the last claim on its segment as
	// often as an ack does, so the same reclaim has to run here.
	q.compactUnlocked()
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

// InflightLen reports how many items are dequeued but neither acked nor
// nacked. Nothing in production reads it; it exists so a test can assert that
// a drain loop strands nothing, which is exactly the bug that produced
// permanently undeliverable items on a live agent -- and, since a claim is
// released only on ack or drop, a stranded item now pins its WAL segment and
// the whole prefix behind it for the lifetime of the process.
//
// Deliberately not on the Queue interface: it is an observation hook, not a
// capability callers get to depend on. Tests reach it by asserting on an
// anonymous interface.
func (q *bufferedQueue) InflightLen() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.inflight)
}

// SegmentClaims reports the total number of live WAL references, summed over
// every segment. A test hook with the same standing as InflightLen: nothing in
// production reads it, and it is not on the Queue interface.
//
// It is the other half of the leak check. InflightLen == 0 proves no entry was
// stranded; this proves the claims those entries held were handed back, which
// is what lets compaction ever unlink a segment -- and, in the other direction,
// that nothing released a claim twice, which would let compaction unlink a
// segment whose records are still needed.
func (q *bufferedQueue) SegmentClaims() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	var total int64
	for _, count := range q.refsPerSegment {
		total += count
	}
	return total
}

// RefUnderflows reports how many times a segment claim was released that was
// never held. A test hook, like InflightLen and SegmentClaims.
//
// SegmentClaims alone cannot see a double release: releaseDiskRefUnlocked
// clamps at zero, so a second decrement leaves the same reading as one. This
// counter is the only evidence, and a double release is precisely what lets
// compaction unlink a segment whose records are still needed.
func (q *bufferedQueue) RefUnderflows() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.refUnderflows
}

// DeliveredUnrecordedTotal returns the number of items retired at the attempt
// cap after a successful delivery -- items the gateway has and the agent could
// not record. Kept apart from DroppedTotal, which means data loss.
func (q *bufferedQueue) DeliveredUnrecordedTotal() int64 {
	return q.unrecorded.Load()
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

	// O(len(inflight)) on every queue mutation. inflight is bounded by
	// batch_size times the number of batches in flight -- hundreds, not
	// thousands -- so the scan is cheaper than tracking a running minimum
	// through every settle path would be.
	oldest := q.oldestInflightLocked()
	if oldest.IsZero() {
		q.metrics.SetOldestInflightAge(0)
	} else {
		q.metrics.SetOldestInflightAge(time.Since(oldest).Seconds())
	}
}
