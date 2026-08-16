package queue

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/kubexa/kubexa-agent/internal/logger"
	agentmetrics "github.com/kubexa/kubexa-agent/internal/metrics"
	"github.com/kubexa/kubexa-agent/pkg/config"
)

func testBufferConfig(t *testing.T, spillDir string, maxMemory int64) *config.BufferConfig {
	t.Helper()
	cfg := &config.BufferConfig{
		MaxMemoryBytes: maxMemory,
		SpillDir:       spillDir,
		MaxDiskBytes:   64 << 20,
		BatchSize:      10,
	}
	return cfg
}

func newTestQueueMetrics(t *testing.T, reg *prometheus.Registry) *agentmetrics.QueueMetrics {
	t.Helper()
	m, err := agentmetrics.New(reg, "test", "cluster", "agent")
	if err != nil {
		t.Fatalf("metrics.New() error = %v", err)
	}
	return m.Queue()
}

func newTestQueue(t *testing.T, cfg *config.BufferConfig) Queue {
	t.Helper()
	reg := prometheus.NewRegistry()
	q, err := New(cfg, logger.New("queue-test"), newTestQueueMetrics(t, reg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

// newTestBufferedQueue returns the concrete queue so tests can inspect the
// reference accounting the Queue interface deliberately does not expose. Pass
// a nil registry when the test does not read metrics.
func newTestBufferedQueue(t *testing.T, cfg *config.BufferConfig, reg *prometheus.Registry) *bufferedQueue {
	t.Helper()
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	q, err := New(cfg, logger.New("queue-test"), newTestQueueMetrics(t, reg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	bq, ok := q.(*bufferedQueue)
	if !ok {
		t.Fatalf("New() returned %T, want *bufferedQueue", q)
	}
	return bq
}

// segmentRefCount reads one segment's live reference count under the queue lock.
func segmentRefCount(t *testing.T, q *bufferedQueue, segment int) int64 {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.refsPerSegment[segment]
}

// spillOneItem enqueues item id, then a second item that forces id out to disk.
// It returns the queue with exactly one reference in segment 0 and one resident
// item in memory.
func spillOneItem(t *testing.T, q *bufferedQueue, id string, payload []byte) {
	t.Helper()
	ctx := context.Background()
	if err := q.Enqueue(ctx, item(id, payload)); err != nil {
		t.Fatalf("Enqueue(%s) error = %v", id, err)
	}
	if err := q.Enqueue(ctx, item("resident", make([]byte, 100))); err != nil {
		t.Fatalf("Enqueue(resident) error = %v", err)
	}
	if got := segmentRefCount(t, q, 0); got != 1 {
		t.Fatalf("refsPerSegment[0] after spill = %d, want 1", got)
	}
}

func item(id string, payload []byte) Item {
	return Item{
		ID:         id,
		Payload:    payload,
		EnqueuedAt: time.Now().UTC(),
	}
}

func TestEnqueueDequeueHappyPath(t *testing.T) {
	t.Parallel()

	q := newTestQueue(t, testBufferConfig(t, "", 64<<10))
	ctx := context.Background()

	want := item("a", []byte("payload-a"))
	if err := q.Enqueue(ctx, want); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	got, err := q.DequeueBatch(ctx, 10)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(batch) = %d, want 1", len(got))
	}
	if got[0].ID != want.ID || string(got[0].Payload) != string(want.Payload) {
		t.Fatalf("dequeued item = %+v, want %+v", got[0], want)
	}
}

func TestEnqueueGeneratesID(t *testing.T) {
	t.Parallel()

	q := newTestQueue(t, testBufferConfig(t, "", 64<<10))
	ctx := context.Background()

	if err := q.Enqueue(ctx, Item{Payload: []byte("x")}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	batch, err := q.DequeueBatch(ctx, 1)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if batch[0].ID == "" {
		t.Fatal("expected generated item ID")
	}
}

func TestAckRemovesPermanently(t *testing.T) {
	t.Parallel()

	q := newTestQueue(t, testBufferConfig(t, "", 64<<10))
	ctx := context.Background()

	it := item("ack-me", []byte("data"))
	if err := q.Enqueue(ctx, it); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	batch, err := q.DequeueBatch(ctx, 1)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if err := q.Ack([]string{batch[0].ID}); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	if q.Depth() != 0 {
		t.Fatalf("Depth() after ack = %d, want 0", q.Depth())
	}

	ctxShort, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = q.DequeueBatch(ctxShort, 1)
	if err == nil {
		t.Fatal("expected timeout waiting on empty queue")
	}
}

func TestNackRequeuesWithAttempts(t *testing.T) {
	t.Parallel()

	q := newTestQueue(t, testBufferConfig(t, "", 64<<10))
	ctx := context.Background()

	it := item("nack-me", []byte("retry"))
	if err := q.Enqueue(ctx, it); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	batch, err := q.DequeueBatch(ctx, 1)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if err := q.Nack([]string{batch[0].ID}); err != nil {
		t.Fatalf("Nack() error = %v", err)
	}
	if q.Depth() != 1 {
		t.Fatalf("Depth() after nack = %d, want 1", q.Depth())
	}

	retry, err := q.DequeueBatch(ctx, 1)
	if err != nil {
		t.Fatalf("DequeueBatch() retry error = %v", err)
	}
	if retry[0].Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", retry[0].Attempts)
	}
	if retry[0].ID != it.ID {
		t.Fatalf("retried ID = %q, want %q", retry[0].ID, it.ID)
	}
}

func TestMemoryFullDropsOldest(t *testing.T) {
	t.Parallel()

	// One slot in channel (4096 bytes estimate → 4096 max memory = 1 slot).
	cfg := testBufferConfig(t, "", 4096)
	q := newTestQueue(t, cfg)
	ctx := context.Background()

	payload := []byte("12345678901234567890") // pushes byte accounting
	if err := q.Enqueue(ctx, item("first", payload)); err != nil {
		t.Fatalf("Enqueue(first) error = %v", err)
	}
	if err := q.Enqueue(ctx, item("second", payload)); err != nil {
		t.Fatalf("Enqueue(second) error = %v", err)
	}

	if q.DroppedTotal() < 1 {
		t.Fatalf("DroppedTotal() = %d, want >= 1", q.DroppedTotal())
	}

	batch, err := q.DequeueBatch(ctx, 10)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("len(batch) = %d, want 1", len(batch))
	}
	if batch[0].ID != "second" {
		t.Fatalf("remaining item ID = %q, want second", batch[0].ID)
	}
}

func TestDiskSpillAndRecovery(t *testing.T) {
	spillDir := t.TempDir()
	cfg := testBufferConfig(t, spillDir, 4096)

	ctx := context.Background()
	it := item("persist", []byte("wal-payload-0123456789"))

	{
		q, err := New(cfg, logger.New("queue-test"), newTestQueueMetrics(t, prometheus.NewRegistry()))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if err := q.Enqueue(ctx, item("mem-only", []byte("small"))); err != nil {
			t.Fatalf("Enqueue(mem) error = %v", err)
		}
		// Fill memory and force spill of subsequent items.
		if err := q.Enqueue(ctx, it); err != nil {
			t.Fatalf("Enqueue(persist) error = %v", err)
		}
		if err := q.Enqueue(ctx, item("also-disk", []byte("wal-payload-0123456789"))); err != nil {
			t.Fatalf("Enqueue(also-disk) error = %v", err)
		}
		if err := q.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}

	q2, err := New(cfg, logger.New("queue-test"), newTestQueueMetrics(t, prometheus.NewRegistry()))
	if err != nil {
		t.Fatalf("New(recover) error = %v", err)
	}
	t.Cleanup(func() { _ = q2.Close() })

	if q2.Depth() < 1 {
		t.Fatalf("Depth() after recovery = %d, want >= 1", q2.Depth())
	}

	var found bool
	for {
		ctxBatch, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		batch, err := q2.DequeueBatch(ctxBatch, 10)
		cancel()
		if err != nil {
			break
		}
		for _, b := range batch {
			if b.ID == "persist" {
				found = true
			}
			_ = q2.Ack([]string{b.ID})
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatal("recovered queue did not contain spilled item id=persist")
	}
}

func TestConcurrentEnqueueDequeue(t *testing.T) {
	t.Parallel()

	q := newTestQueue(t, testBufferConfig(t, "", 8<<20))
	ctx := context.Background()

	const producers = 8
	const perProducer = 50
	var wg sync.WaitGroup
	wg.Add(producers)

	enqueueErr := make(chan error, producers)
	for p := 0; p < producers; p++ {
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				id := fmt.Sprintf("p%d-%d", p, i)
				if err := q.Enqueue(ctx, item(id, []byte("x"))); err != nil {
					enqueueErr <- err
					return
				}
			}
		}(p)
	}

	dequeued := make(chan struct{})
	go func() {
		total := 0
		for total < producers*perProducer {
			batch, err := q.DequeueBatch(ctx, 20)
			if err != nil {
				enqueueErr <- err
				return
			}
			total += len(batch)
			ids := make([]string, len(batch))
			for i, it := range batch {
				ids[i] = it.ID
			}
			if err := q.Ack(ids); err != nil {
				enqueueErr <- err
				return
			}
		}
		close(dequeued)
	}()

	wg.Wait()
	select {
	case err := <-enqueueErr:
		t.Fatalf("concurrent error: %v", err)
	default:
	}
	<-dequeued
}

func TestMetricsCorrectness(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	cfg := testBufferConfig(t, "", 64<<10)
	q, err := New(cfg, logger.New("queue-test"), newTestQueueMetrics(t, reg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })

	ctx := context.Background()
	if err := q.Enqueue(ctx, item("m1", []byte("a"))); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if err := q.Enqueue(ctx, item("m2", []byte("b"))); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	batch, err := q.DequeueBatch(ctx, 1)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if err := q.Ack([]string{batch[0].ID}); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	batch2, err := q.DequeueBatch(ctx, 1)
	if err != nil {
		t.Fatalf("DequeueBatch() second error = %v", err)
	}
	if err := q.Nack([]string{batch2[0].ID}); err != nil {
		t.Fatalf("Nack() error = %v", err)
	}

	assertCounter(t, reg, "kubexa_queue_enqueued_total", 2)
	assertCounter(t, reg, "kubexa_queue_dequeued_total", 2)
	assertCounter(t, reg, "kubexa_queue_ack_total", 1)
	assertCounter(t, reg, "kubexa_queue_nack_total", 1)
}

func assertCounter(t *testing.T, reg *prometheus.Registry, name string, want float64) {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if m.GetCounter().GetValue() != want {
				t.Fatalf("metric %s = %v, want %v", name, m.GetCounter().GetValue(), want)
			}
			return
		}
	}
	t.Fatalf("metric %q not found", name)
}

func TestDepthGauge(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	cfg := testBufferConfig(t, "", 64<<10)
	q, err := New(cfg, logger.New("queue-test"), newTestQueueMetrics(t, reg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })

	ctx := context.Background()
	_ = q.Enqueue(ctx, item("d1", []byte("x")))
	_ = q.Enqueue(ctx, item("d2", []byte("y")))

	if q.Depth() != 2 {
		t.Fatalf("Depth() = %d, want 2", q.Depth())
	}

	gauge := gatherGauge(t, reg, "kubexa_queue_depth", "memory")
	if gauge != 2 {
		t.Fatalf("memory depth gauge = %v, want 2", gauge)
	}
}

func gatherGauge(t *testing.T, reg *prometheus.Registry, name, label string) float64 {
	t.Helper()
	var mfs []*dto.MetricFamily
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "tier" && lp.GetValue() == label {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatalf("gauge %s tier=%s not found", name, label)
	return 0
}

func TestValidateConfig(t *testing.T) {
	t.Parallel()

	_, err := New(&config.BufferConfig{MaxMemoryBytes: 0}, nil, nil)
	if err == nil {
		t.Fatal("expected error for invalid max memory")
	}
}

func TestDiskItemsDequeueBeforeMemoryItems(t *testing.T) {
	t.Parallel()

	// A memory budget of one small item forces the second enqueue to spill.
	cfg := testBufferConfig(t, t.TempDir(), 200)
	q := newTestQueue(t, cfg)
	ctx := context.Background()

	// "old" spills to disk when "new" needs the memory.
	if err := q.Enqueue(ctx, item("old", make([]byte, 100))); err != nil {
		t.Fatalf("Enqueue(old) error = %v", err)
	}
	if err := q.Enqueue(ctx, item("new", make([]byte, 100))); err != nil {
		t.Fatalf("Enqueue(new) error = %v", err)
	}

	batch, err := q.DequeueBatch(ctx, 1)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("got %d items, want 1", len(batch))
	}
	if batch[0].ID != "old" {
		t.Errorf("first dequeued item = %q, want %q: spilled items are older and "+
			"must come out first", batch[0].ID, "old")
	}
}

func TestSpilledPayloadSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()

	cfg := testBufferConfig(t, t.TempDir(), 200)
	q := newTestQueue(t, cfg)
	ctx := context.Background()

	payload := []byte("the payload that must come back byte for byte")
	if err := q.Enqueue(ctx, item("spilled", payload)); err != nil {
		t.Fatalf("Enqueue(spilled) error = %v", err)
	}
	if err := q.Enqueue(ctx, item("resident", make([]byte, 100))); err != nil {
		t.Fatalf("Enqueue(resident) error = %v", err)
	}

	batch, err := q.DequeueBatch(ctx, 1)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if batch[0].ID != "spilled" {
		t.Fatalf("dequeued %q, want %q", batch[0].ID, "spilled")
	}
	if string(batch[0].Payload) != string(payload) {
		t.Errorf("payload = %q, want %q", batch[0].Payload, payload)
	}
}

func TestUnreadableSpilledItemIsDroppedNotStuck(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := testBufferConfig(t, dir, 200)
	q := newTestQueue(t, cfg)
	ctx := context.Background()

	if err := q.Enqueue(ctx, item("corrupt", make([]byte, 100))); err != nil {
		t.Fatalf("Enqueue(corrupt) error = %v", err)
	}
	if err := q.Enqueue(ctx, item("good", make([]byte, 100))); err != nil {
		t.Fatalf("Enqueue(good) error = %v", err)
	}

	// Truncate the segment so the spilled record cannot be read back.
	path := dir + "/" + segmentFilename(0)
	if err := os.Truncate(path, int64(len(walMagic)+1)); err != nil {
		t.Fatalf("truncate segment: %v", err)
	}

	batch, err := q.DequeueBatch(ctx, 2)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if len(batch) != 1 || batch[0].ID != "good" {
		t.Fatalf("got %v, want just the readable item %q", batch, "good")
	}
	if q.DroppedTotal() != 1 {
		t.Errorf("DroppedTotal() = %d, want 1", q.DroppedTotal())
	}
}

// TestSegmentClaimSurvivesDequeueAndNack pins the one invariant whose violation
// is silent: a segment's reference count may only fall when the item is gone
// for good. If releaseDiskRefUnlocked ever moves into the dequeue or the nack
// path, the count reaches zero while the item is still owed to the consumer,
// Task 7's compaction deletes the segment underneath it, and the item vanishes
// with no error anywhere. Nothing else in the suite reads refsPerSegment.
func TestSegmentClaimSurvivesDequeueAndNack(t *testing.T) {
	t.Parallel()

	q := newTestBufferedQueue(t, testBufferConfig(t, t.TempDir(), 200), nil)
	ctx := context.Background()

	payload := []byte("claim-me")
	spillOneItem(t, q, "spilled", payload)

	batch, err := q.DequeueBatch(ctx, 1)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if batch[0].ID != "spilled" {
		t.Fatalf("dequeued %q, want %q", batch[0].ID, "spilled")
	}
	if got := segmentRefCount(t, q, 0); got != 1 {
		t.Fatalf("refsPerSegment[0] after dequeue = %d, want 1: a dequeued item "+
			"is still owed to the consumer and still pins its segment", got)
	}

	if err := q.Nack([]string{"spilled"}); err != nil {
		t.Fatalf("Nack() error = %v", err)
	}
	if got := segmentRefCount(t, q, 0); got != 1 {
		t.Fatalf("refsPerSegment[0] after nack = %d, want 1: the reference went "+
			"back into the queue and no WAL record was rewritten", got)
	}

	retry, err := q.DequeueBatch(ctx, 1)
	if err != nil {
		t.Fatalf("DequeueBatch() retry error = %v", err)
	}
	if retry[0].ID != "spilled" || retry[0].Attempts != 1 {
		t.Fatalf("retry = %q attempts=%d, want %q attempts=1",
			retry[0].ID, retry[0].Attempts, "spilled")
	}
	if got := segmentRefCount(t, q, 0); got != 1 {
		t.Fatalf("refsPerSegment[0] after re-dequeue = %d, want 1", got)
	}

	if err := q.Ack([]string{"spilled"}); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	q.mu.Lock()
	count, present := q.refsPerSegment[0]
	underflows := q.refUnderflows
	q.mu.Unlock()
	if present || count != 0 {
		t.Errorf("refsPerSegment[0] after ack = %d (present=%v), want released", count, present)
	}
	if underflows != 0 {
		t.Errorf("refUnderflows = %d, want 0: the claim was released exactly once", underflows)
	}
}

// TestInflightDiskEntryHoldsNoPayload is the assertion that stops the in-memory
// mirror coming back. An inflight entry for a spilled item must cost a
// reference, not a payload.
func TestInflightDiskEntryHoldsNoPayload(t *testing.T) {
	t.Parallel()

	q := newTestBufferedQueue(t, testBufferConfig(t, t.TempDir(), 200), nil)
	ctx := context.Background()

	spillOneItem(t, q, "spilled", []byte("this payload must live only on disk"))

	batch, err := q.DequeueBatch(ctx, 1)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if batch[0].ID != "spilled" {
		t.Fatalf("dequeued %q, want %q", batch[0].ID, "spilled")
	}

	q.mu.Lock()
	entry, ok := q.inflight["spilled"]
	q.mu.Unlock()
	if !ok {
		t.Fatal("dequeued item is not in inflight")
	}
	if entry.ref == nil {
		t.Fatal("inflight entry has no diskRef: a disk-sourced item must be tracked by reference")
	}
	if entry.item.Payload != nil {
		t.Errorf("inflight entry retains a %d-byte payload, want none", len(entry.item.Payload))
	}
	if entry.item.ID != "" {
		t.Errorf("inflight entry retains item %+v, want the zero Item", entry.item)
	}
}

// TestNackedDiskItemComesBackFromDisk covers the disk-sourced nack round trip:
// the payload is re-read from the WAL, not from anything the queue kept.
func TestNackedDiskItemComesBackFromDisk(t *testing.T) {
	t.Parallel()

	q := newTestBufferedQueue(t, testBufferConfig(t, t.TempDir(), 200), nil)
	ctx := context.Background()

	payload := []byte("retry me byte for byte")
	spillOneItem(t, q, "spilled", payload)

	batch, err := q.DequeueBatch(ctx, 1)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if err := q.Nack([]string{batch[0].ID}); err != nil {
		t.Fatalf("Nack() error = %v", err)
	}

	retry, err := q.DequeueBatch(ctx, 1)
	if err != nil {
		t.Fatalf("DequeueBatch() retry error = %v", err)
	}
	if retry[0].ID != "spilled" {
		t.Fatalf("retry ID = %q, want %q", retry[0].ID, "spilled")
	}
	if retry[0].Attempts != 1 {
		t.Errorf("retry Attempts = %d, want 1", retry[0].Attempts)
	}
	if string(retry[0].Payload) != string(payload) {
		t.Errorf("retry payload = %q, want %q", retry[0].Payload, payload)
	}
}

func TestUnreadableSpilledItemCountsADiskReadError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	reg := prometheus.NewRegistry()
	q := newTestBufferedQueue(t, testBufferConfig(t, dir, 200), reg)
	ctx := context.Background()

	spillOneItem(t, q, "corrupt", make([]byte, 100))

	if err := os.Truncate(dir+"/"+segmentFilename(0), int64(len(walMagic)+1)); err != nil {
		t.Fatalf("truncate segment: %v", err)
	}

	if _, err := q.DequeueBatch(ctx, 2); err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	assertCounter(t, reg, "kubexa_queue_disk_read_errors_total", 1)
}

// errAfterNContext reports no error for the first n Err calls and a
// cancellation for every call after that, so a batch can be cancelled at a
// chosen iteration instead of a racy one.
type errAfterNContext struct {
	context.Context
	mu    sync.Mutex
	calls int
	n     int
}

func (c *errAfterNContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls > c.n {
		return context.Canceled
	}
	return nil
}

// TestCancelMidBatchKeepsPulledItems covers the loss window: an item pulled
// before the cancellation has already left diskRefs and the memory channel, so
// discarding the batch loses it outright (memory) or strands it until a restart
// (disk). Everything pulled must end up either in the returned batch or back in
// the queue.
func TestCancelMidBatchKeepsPulledItems(t *testing.T) {
	t.Parallel()

	q := newTestBufferedQueue(t, testBufferConfig(t, t.TempDir(), 200), nil)

	spillOneItem(t, q, "spilled", []byte("must not be discarded"))

	// One Err call for the outer loop check, one for the first pull; the
	// second pull sees the cancellation.
	ctx := &errAfterNContext{Context: context.Background(), n: 2}

	batch, err := q.DequeueBatch(ctx, 2)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v, want the already-pulled item returned", err)
	}
	if len(batch) != 1 || batch[0].ID != "spilled" {
		t.Fatalf("batch = %v, want just %q", batch, "spilled")
	}

	if q.DroppedTotal() != 0 {
		t.Errorf("DroppedTotal() = %d, want 0", q.DroppedTotal())
	}
	// The resident item never left the memory tier.
	if depth := q.Depth(); depth != 1 {
		t.Errorf("Depth() = %d, want 1", depth)
	}
	q.mu.Lock()
	_, inflight := q.inflight["spilled"]
	q.mu.Unlock()
	if !inflight {
		t.Error("returned item is not registered inflight: an unacked item that is in no tier is lost")
	}
}

// TestNackSpillDoesNotCountAsEnqueue: the memory branch of Nack deliberately
// passes countEnqueue=false. The spilling branch must agree — a retry is not an
// arrival, and counting it makes enqueued_total drift up on every failed send.
func TestNackSpillDoesNotCountAsEnqueue(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	q := newTestBufferedQueue(t, testBufferConfig(t, t.TempDir(), 200), reg)
	ctx := context.Background()

	if err := q.Enqueue(ctx, item("a", make([]byte, 100))); err != nil {
		t.Fatalf("Enqueue(a) error = %v", err)
	}
	batch, err := q.DequeueBatch(ctx, 1)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if batch[0].ID != "a" {
		t.Fatalf("dequeued %q, want %q", batch[0].ID, "a")
	}
	// Refill memory so the nacked item cannot go back into it.
	if err := q.Enqueue(ctx, item("b", make([]byte, 100))); err != nil {
		t.Fatalf("Enqueue(b) error = %v", err)
	}
	if err := q.Nack([]string{"a"}); err != nil {
		t.Fatalf("Nack() error = %v", err)
	}

	q.mu.Lock()
	refs := len(q.diskRefs)
	q.mu.Unlock()
	if refs != 1 {
		t.Fatalf("diskRefs = %d, want 1: the nacked item should have spilled", refs)
	}
	assertCounter(t, reg, "kubexa_queue_enqueued_total", 2)
}

// TestReleaseOfAnUnheldClaimIsLoud: an over-release is the exact accounting bug
// this counter exists to prevent, so it must not settle at -1 and then erase
// itself when the key is deleted.
func TestReleaseOfAnUnheldClaimIsLoud(t *testing.T) {
	t.Parallel()

	q := newTestBufferedQueue(t, testBufferConfig(t, t.TempDir(), 200), nil)

	spillOneItem(t, q, "spilled", []byte("x"))

	q.mu.Lock()
	q.releaseDiskRefUnlocked(0) // legitimate: takes the count to zero
	afterFirst := q.refUnderflows
	q.releaseDiskRefUnlocked(0) // over-release
	count, present := q.refsPerSegment[0]
	underflows := q.refUnderflows
	q.mu.Unlock()

	if afterFirst != 0 {
		t.Errorf("refUnderflows after a legitimate release = %d, want 0", afterFirst)
	}
	if underflows != 1 {
		t.Errorf("refUnderflows after over-release = %d, want 1", underflows)
	}
	if present || count != 0 {
		t.Errorf("refsPerSegment[0] = %d (present=%v), want clamped at zero and absent", count, present)
	}
}

// TestDroppedDiskItemStaysDroppedAcrossRestart: a pressure-drop that writes no
// ack record is not a drop at all — recovery replays the record and the item
// returns, while dropped_total insists it is gone.
func TestDroppedDiskItemStaysDroppedAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := testBufferConfig(t, dir, 200)
	ctx := context.Background()

	{
		q := newTestBufferedQueue(t, cfg, nil)
		spillOneItem(t, q, "pressure-dropped", make([]byte, 100))

		// Drain both tiers into inflight, then nack the disk item back so the
		// memory channel is empty and dropOldestUnlocked reaches its disk branch.
		batch, err := q.DequeueBatch(ctx, 2)
		if err != nil {
			t.Fatalf("DequeueBatch() error = %v", err)
		}
		if len(batch) != 2 {
			t.Fatalf("got %d items, want 2", len(batch))
		}
		if err := q.Nack([]string{"pressure-dropped"}); err != nil {
			t.Fatalf("Nack() error = %v", err)
		}
		if err := q.Ack([]string{"resident"}); err != nil {
			t.Fatalf("Ack() error = %v", err)
		}

		q.mu.Lock()
		q.dropOldestUnlocked()
		q.mu.Unlock()

		if q.DroppedTotal() != 1 {
			t.Fatalf("DroppedTotal() = %d, want 1", q.DroppedTotal())
		}
		if depth := q.Depth(); depth != 0 {
			t.Fatalf("Depth() after drop = %d, want 0", depth)
		}
		if err := q.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}

	q2 := newTestBufferedQueue(t, cfg, nil)
	if depth := q2.Depth(); depth != 0 {
		t.Errorf("Depth() after restart = %d, want 0: a dropped item must not "+
			"come back from the WAL", depth)
	}
	q2.mu.Lock()
	refs := len(q2.diskRefs)
	q2.mu.Unlock()
	if refs != 0 {
		t.Errorf("recovered %d refs, want 0", refs)
	}
}

func TestDequeueRespectsContext(t *testing.T) {
	t.Parallel()

	q := newTestQueue(t, testBufferConfig(t, "", 64<<10))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := q.DequeueBatch(ctx, 1)
	if err == nil {
		t.Fatal("expected context error on empty queue")
	}
}
