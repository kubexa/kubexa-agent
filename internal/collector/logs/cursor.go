package logs

import (
	"context"
	"sync"
	"time"

	"github.com/kubexa/kubexa-agent/internal/collector/logs/checkpoint"
)

const checkpointSaveDebounce = 1500 * time.Millisecond

// streamCursor tracks the newest log timestamp successfully exported for a stream.
type streamCursor struct {
	mu            sync.Mutex
	lastProcessed time.Time

	ctx     context.Context
	store   checkpoint.Store
	key     checkpoint.Key
	meta    checkpoint.Metadata
	metrics *metrics

	saveTimer *time.Timer
	closed    bool
}

func newStreamCursor(
	ctx context.Context,
	store checkpoint.Store,
	key checkpoint.Key,
	meta checkpoint.Metadata,
	metrics *metrics,
) *streamCursor {
	cur := &streamCursor{
		ctx:     ctx,
		store:   store,
		key:     key,
		meta:    meta,
		metrics: metrics,
	}
	if store == nil {
		return cur
	}

	ts, ok, err := store.Load(ctx, key)
	if err != nil {
		if metrics != nil {
			metrics.incCheckpointError("load")
		}
		return cur
	}
	if ok {
		cur.markProcessedMemory(ts)
	}
	return cur
}

func (cur *streamCursor) markProcessed(ts time.Time) {
	if cur == nil || ts.IsZero() {
		return
	}
	ts = ts.UTC()

	cur.mu.Lock()
	updated := ts.After(cur.lastProcessed)
	if updated {
		cur.lastProcessed = ts
	}
	closed := cur.closed
	cur.mu.Unlock()

	if updated && cur.store != nil && !closed {
		cur.schedulePersist()
	}
}

func (cur *streamCursor) markProcessedMemory(ts time.Time) {
	if cur == nil || ts.IsZero() {
		return
	}
	cur.mu.Lock()
	defer cur.mu.Unlock()
	if ts.UTC().After(cur.lastProcessed) {
		cur.lastProcessed = ts.UTC()
	}
}

func (cur *streamCursor) schedulePersist() {
	cur.mu.Lock()
	defer cur.mu.Unlock()
	if cur.closed || cur.store == nil {
		return
	}
	if cur.saveTimer == nil {
		cur.saveTimer = time.AfterFunc(checkpointSaveDebounce, cur.flush)
		return
	}
	cur.saveTimer.Reset(checkpointSaveDebounce)
}

func (cur *streamCursor) flush() {
	cur.mu.Lock()
	if cur.store == nil {
		cur.mu.Unlock()
		return
	}
	ts := cur.lastProcessed
	cur.mu.Unlock()

	if ts.IsZero() {
		return
	}

	ctx := cur.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := cur.store.Save(ctx, cur.key, ts, cur.meta); err != nil {
		if cur.metrics != nil {
			cur.metrics.incCheckpointError("save")
		}
		return
	}
	if cur.metrics != nil {
		cur.metrics.incCheckpointWrite()
	}
}

// close stops debounced saves and flushes the latest checkpoint to disk.
func (cur *streamCursor) close() {
	if cur == nil {
		return
	}

	cur.mu.Lock()
	if cur.closed {
		cur.mu.Unlock()
		return
	}
	if cur.saveTimer != nil {
		cur.saveTimer.Stop()
		cur.saveTimer = nil
	}
	cur.mu.Unlock()

	cur.flush()

	cur.mu.Lock()
	cur.closed = true
	cur.mu.Unlock()
}

// sinceForReconnect returns a SinceTime for the Kubernetes log API when resuming a stream.
func (cur *streamCursor) sinceForReconnect(overlap time.Duration) (time.Time, bool) {
	if cur == nil {
		return time.Time{}, false
	}
	cur.mu.Lock()
	defer cur.mu.Unlock()
	if cur.lastProcessed.IsZero() {
		return time.Time{}, false
	}
	if overlap < 0 {
		overlap = 0
	}
	return cur.lastProcessed.Add(-overlap), true
}

func (k streamKey) checkpointKey() checkpoint.Key {
	return checkpoint.Key{
		PodUID:    k.podUID,
		Container: k.containerName,
	}
}
