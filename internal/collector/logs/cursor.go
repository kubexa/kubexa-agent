package logs

import (
	"context"
	"encoding/hex"
	"hash/fnv"
	"sync"
	"time"

	"github.com/kubexa/kubexa-agent/internal/collector/logs/checkpoint"
)

const checkpointSaveDebounce = 1500 * time.Millisecond

const (
	// recentRetention is how far back the de-duplication set remembers a
	// record it delivered. It must cover everything a resume can replay.
	//
	// That is more than reconnectSinceOverlap, because the overlap is not the
	// only rewind. SinceTime reaches the Kubernetes log API as an RFC3339
	// query parameter at SECOND precision, so the API starts from the
	// beginning of the requested second -- up to a further second of lines
	// the stream has already sent. The extra two seconds cover that
	// truncation with margin; remembering a little too long costs a few
	// entries, remembering too little costs a duplicate.
	recentRetention = reconnectSinceOverlap + 2*time.Second

	// maxRecentDelivered caps the set regardless of how much a stream writes
	// inside recentRetention. Reconnects happen overwhelmingly on QUIET
	// streams -- kubelet closes a stream for being idle, not for being busy --
	// so in practice the window holds a handful of records and the cap never
	// binds. It exists so a burst cannot make this set unbounded, and its
	// failure mode is today's behaviour for the oldest records in the burst.
	maxRecentDelivered = 256
)

// streamCursor tracks where a stream resumes and what it has already sent.
//
// The second half is not an optimisation. A resume deliberately rewinds past
// the last line it handled, so without a record of what that rewind replays,
// every reconnect re-delivers. Measured on apexcrawl 2026-11-11, before this
// set existed: one cert-manager-webhook line stored seven times over, and 137
// of cert-manager-cainjector's 432 stored lines were copies.
type streamCursor struct {
	mu            sync.Mutex
	lastProcessed time.Time
	lastRead      time.Time
	recent        []checkpoint.Delivered // oldest first
	recentKeys    map[deliveredKey]struct{}

	ctx     context.Context
	store   checkpoint.Store
	key     checkpoint.Key
	meta    checkpoint.Metadata
	metrics *metrics

	saveTimer *time.Timer
	closed    bool
}

// deliveredKey is the identity of a delivered record: when the log API read
// it, and what it said. The payload is part of the key because two distinct
// lines can share one read time, and dropping one of them would be the very
// thing this set exists to prevent -- losing a line nobody sent twice.
type deliveredKey struct {
	readNS int64
	digest string
}

// recordDigest fingerprints the bytes that reach Loki. FNV-128a rather than a
// cryptographic hash: nothing here defends against a crafted collision, only
// against an accidental one among at most maxRecentDelivered live entries.
func recordDigest(raw []byte) string {
	h := fnv.New128a()
	_, _ = h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
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

	pos, ok, err := store.Load(ctx, key)
	if err != nil {
		if metrics != nil {
			metrics.incCheckpointError("load")
		}
		return cur
	}
	if ok {
		cur.restore(pos)
	}
	return cur
}

// restore loads a persisted position WITHOUT scheduling a save: nothing has
// changed yet, and a save here would rewrite the row on every stream start.
func (cur *streamCursor) restore(pos checkpoint.Position) {
	cur.mu.Lock()
	defer cur.mu.Unlock()

	if pos.LastProcessed.UTC().After(cur.lastProcessed) {
		cur.lastProcessed = pos.LastProcessed.UTC()
	}
	if pos.LastRead.UTC().After(cur.lastRead) {
		cur.lastRead = pos.LastRead.UTC()
	}
	for _, d := range pos.Recent {
		cur.rememberLocked(d.ReadTime.UTC(), d.Digest)
	}
	cur.pruneLocked()
}

// markDelivered records that one record reached the queue: its position in
// both clocks, and its identity, so a resume that replays it can tell.
func (cur *streamCursor) markDelivered(rec ParsedLine) {
	if cur == nil {
		return
	}

	cur.mu.Lock()
	updated := false
	if !rec.Timestamp.IsZero() && rec.Timestamp.UTC().After(cur.lastProcessed) {
		cur.lastProcessed = rec.Timestamp.UTC()
		updated = true
	}
	if !rec.ReadTime.IsZero() {
		read := rec.ReadTime.UTC()
		if read.After(cur.lastRead) {
			cur.lastRead = read
			updated = true
		}
		cur.rememberLocked(read, recordDigest(rec.Raw))
		cur.pruneLocked()
	}
	closed := cur.closed
	cur.mu.Unlock()

	if updated && cur.store != nil && !closed {
		cur.schedulePersist()
	}
}

// alreadyDelivered reports whether this exact record has already reached the
// queue inside the window a resume can replay.
//
// A record with no read time is never matched. Its position is unknown, a set
// keyed on a zero time would collapse every such record onto one key, and the
// cost of that mistake is a dropped log line -- strictly worse than the
// duplicate this set is here to avoid.
//
// The match is on the record the joiner rebuilt, so a multi-line record whose
// continuation lines straddle a reconnect is rebuilt differently, hashes
// differently, and is delivered again. That is the behaviour this package
// already had, it is bounded by the joiner's hold window, and it errs toward
// sending -- which is the direction to err in.
func (cur *streamCursor) alreadyDelivered(rec ParsedLine) bool {
	if cur == nil || rec.ReadTime.IsZero() {
		return false
	}
	k := deliveredKey{readNS: rec.ReadTime.UTC().UnixNano(), digest: recordDigest(rec.Raw)}

	cur.mu.Lock()
	defer cur.mu.Unlock()
	_, ok := cur.recentKeys[k]
	return ok
}

func (cur *streamCursor) rememberLocked(readTime time.Time, digest string) {
	if readTime.IsZero() || digest == "" {
		return
	}
	k := deliveredKey{readNS: readTime.UnixNano(), digest: digest}
	if cur.recentKeys == nil {
		cur.recentKeys = make(map[deliveredKey]struct{})
	}
	if _, dup := cur.recentKeys[k]; dup {
		return
	}
	cur.recentKeys[k] = struct{}{}
	cur.recent = append(cur.recent, checkpoint.Delivered{ReadTime: readTime, Digest: digest})
}

// pruneLocked drops what the resume can no longer replay, then enforces the
// cap. Both directions matter: the window alone is unbounded on a busy
// stream, and the cap alone would remember a quiet stream's records forever.
func (cur *streamCursor) pruneLocked() {
	floor := cur.lastRead.Add(-recentRetention)
	cut := 0
	for cut < len(cur.recent) && cur.recent[cut].ReadTime.Before(floor) {
		cut++
	}
	if over := len(cur.recent) - cut - maxRecentDelivered; over > 0 {
		cut += over
	}
	if cut == 0 {
		return
	}
	for _, d := range cur.recent[:cut] {
		delete(cur.recentKeys, deliveredKey{readNS: d.ReadTime.UnixNano(), digest: d.Digest})
	}
	cur.recent = append(cur.recent[:0], cur.recent[cut:]...)
}

func (cur *streamCursor) recentLen() int {
	cur.mu.Lock()
	defer cur.mu.Unlock()
	return len(cur.recent)
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
	pos := checkpoint.Position{
		LastProcessed: cur.lastProcessed,
		LastRead:      cur.lastRead,
		Recent:        append([]checkpoint.Delivered(nil), cur.recent...),
	}
	cur.mu.Unlock()

	if pos.LastProcessed.IsZero() {
		return
	}

	ctx := cur.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := cur.store.Save(ctx, cur.key, pos, cur.meta); err != nil {
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
//
// It answers in the log API's OWN frame whenever the stream has read a line
// with a prefix, because that is the frame the API filters in. Resuming from
// the record clock instead meant resuming from whatever the workload printed
// in its JSON "time" field: a workload running ahead of the node asked
// kubelet to start past lines it had not read yet, and the overlap covers
// only skew smaller than itself. The record clock remains the fallback for a
// position restored from a row written before read times were stored.
func (cur *streamCursor) sinceForReconnect(overlap time.Duration) (time.Time, bool) {
	if cur == nil {
		return time.Time{}, false
	}
	cur.mu.Lock()
	defer cur.mu.Unlock()

	from := cur.lastRead
	if from.IsZero() {
		from = cur.lastProcessed
	}
	if from.IsZero() {
		return time.Time{}, false
	}
	if overlap < 0 {
		overlap = 0
	}
	return from.Add(-overlap), true
}

func (k streamKey) checkpointKey() checkpoint.Key {
	return checkpoint.Key{
		PodUID:    k.podUID,
		Container: k.containerName,
	}
}
