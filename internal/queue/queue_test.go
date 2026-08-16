package queue

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
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

func TestDiskRefCapAppliesToBothSpillPaths(t *testing.T) {
	t.Parallel()

	// max_disk_bytes / avgItemSizeEstimate == 2 references.
	cfg := &config.BufferConfig{
		MaxMemoryBytes: 200,
		SpillDir:       t.TempDir(),
		MaxDiskBytes:   2 * avgItemSizeEstimate,
		BatchSize:      10,
	}
	q := newTestQueue(t, cfg)
	bq, ok := q.(*bufferedQueue)
	if !ok {
		t.Fatalf("newTestQueue returned %T, want *bufferedQueue", q)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Fill past the cap. Once the cap is hit, Enqueue must apply backpressure
	// (block until ctx expires) rather than growing diskRefs without bound.
	var lastErr error
	for i := 0; i < 8; i++ {
		lastErr = q.Enqueue(ctx, item(fmt.Sprintf("i-%d", i), make([]byte, 100)))
		if lastErr != nil {
			break
		}
	}
	if lastErr == nil {
		t.Fatal("Enqueue never applied backpressure past the reference cap")
	}

	bq.mu.Lock()
	refs := len(bq.diskRefs)
	bq.mu.Unlock()
	if int64(refs) > diskSlotCapacity(cfg.MaxDiskBytes) {
		t.Errorf("diskRefs grew to %d, past the cap of %d",
			refs, diskSlotCapacity(cfg.MaxDiskBytes))
	}
}

// TestDiskRefCapAppliesToDirectSpillPath isolates the guard in
// spillEnqueueUnlocked from the one in evictOldestMemoryUnlocked.
//
// With MaxMemoryBytes big enough to hold one item (as in
// TestDiskRefCapAppliesToBothSpillPaths above), every Enqueue past the first
// routes through evictOldestMemoryUnlocked before it ever reaches
// spillEnqueueUnlocked, so that test alone proves only the eviction-path
// guard. Here MaxMemoryBytes is smaller than any item, so memory never
// accepts anything and every Enqueue goes straight to spillEnqueueUnlocked --
// proving its guard holds independently of eviction ever running.
func TestDiskRefCapAppliesToDirectSpillPath(t *testing.T) {
	t.Parallel()

	// max_disk_bytes / avgItemSizeEstimate == 2 references.
	cfg := &config.BufferConfig{
		MaxMemoryBytes: 1,
		SpillDir:       t.TempDir(),
		MaxDiskBytes:   2 * avgItemSizeEstimate,
		BatchSize:      10,
	}
	q := newTestQueue(t, cfg)
	bq, ok := q.(*bufferedQueue)
	if !ok {
		t.Fatalf("newTestQueue returned %T, want *bufferedQueue", q)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var lastErr error
	for i := 0; i < 8; i++ {
		lastErr = q.Enqueue(ctx, item(fmt.Sprintf("d-%d", i), make([]byte, 100)))
		if lastErr != nil {
			break
		}
	}
	if lastErr == nil {
		t.Fatal("Enqueue never applied backpressure past the reference cap")
	}

	bq.mu.Lock()
	memCount := bq.memCount
	refs := len(bq.diskRefs)
	bq.mu.Unlock()
	if memCount != 0 {
		t.Fatalf("memCount = %d, want 0 -- an item reached memory, so this run "+
			"did not isolate the direct-spill path", memCount)
	}
	if int64(refs) > diskSlotCapacity(cfg.MaxDiskBytes) {
		t.Errorf("diskRefs grew to %d, past the cap of %d",
			refs, diskSlotCapacity(cfg.MaxDiskBytes))
	}
}

// TestRecoveryCapsDiskRefsKeepingOldest seeds a spill directory with more
// pending WAL records than the reference cap allows -- the shape a config
// change, or simply a very small average item size, can produce independent
// of anything bufferedQueue itself ever wrote -- and asserts New() caps what
// it loads into memory rather than mirroring the whole WAL as refs.
func TestRecoveryCapsDiskRefsKeepingOldest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	reg := prometheus.NewRegistry()
	seedMetrics := newTestQueueMetrics(t, reg)
	log := logger.New("queue-test")

	// maxBytes=0 means "use the default 512 MiB cap" over in newDiskStore, so
	// seeding is not itself constrained by the tiny cap the queue will be
	// opened with below.
	ds, err := newDiskStore(dir, 0, log, seedMetrics)
	if err != nil {
		t.Fatalf("newDiskStore() error = %v", err)
	}

	const seeded = 5
	base := time.Now().UTC()
	for i := 0; i < seeded; i++ {
		it := Item{
			ID:         fmt.Sprintf("r-%d", i),
			Payload:    make([]byte, 50),
			EnqueuedAt: base.Add(time.Duration(i) * time.Millisecond),
		}
		if _, _, err := ds.appendItem(it); err != nil {
			t.Fatalf("seed appendItem(%d) error = %v", i, err)
		}
	}
	if err := ds.close(); err != nil {
		t.Fatalf("seed ds.close() error = %v", err)
	}

	// max_disk_bytes / avgItemSizeEstimate == 2 references: 3 of the 5 seeded
	// records must be dropped at recovery.
	cfg := &config.BufferConfig{
		MaxMemoryBytes: 64 << 10,
		SpillDir:       dir,
		MaxDiskBytes:   2 * avgItemSizeEstimate,
		BatchSize:      10,
	}
	q := newTestBufferedQueue(t, cfg, nil)

	q.mu.Lock()
	refs := append([]diskRef(nil), q.diskRefs...)
	diskCount := q.diskCount
	q.mu.Unlock()

	if len(refs) != 2 {
		t.Fatalf("recovered refs = %d, want 2 (capped)", len(refs))
	}
	if diskCount != 2 {
		t.Fatalf("diskCount = %d, want 2", diskCount)
	}
	kept := map[string]bool{refs[0].ID: true, refs[1].ID: true}
	if !kept["r-0"] || !kept["r-1"] {
		t.Fatalf("kept refs = %v, want the two oldest (r-0, r-1)", refs)
	}
	if got := q.DroppedTotal(); got != 3 {
		t.Fatalf("DroppedTotal() = %d, want 3", got)
	}

	if err := q.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// A second New() over the same directory must not resurrect the refs
	// dropped by the cap: the drop must have acked them.
	q2 := newTestBufferedQueue(t, cfg, nil)
	q2.mu.Lock()
	refs2 := len(q2.diskRefs)
	q2.mu.Unlock()
	if refs2 != 2 {
		t.Fatalf("second New() recovered %d refs, want 2 (dropped refs resurrected)", refs2)
	}
}

// TestCloseFlushRespectsRefCap fills memory with more items than the disk
// reference cap can hold and never spills any of them along the way, so
// Close's own flush is the first and only thing that tries to write them to
// disk. It must cap there too instead of overshooting by len(memCh).
func TestCloseFlushRespectsRefCap(t *testing.T) {
	t.Parallel()

	cfg := &config.BufferConfig{
		// Large enough that all 5 items enqueued below fit in memory without
		// ever spilling -- Close's flush is the only producer this test
		// exercises.
		MaxMemoryBytes: 100_000,
		SpillDir:       t.TempDir(),
		MaxDiskBytes:   1 * avgItemSizeEstimate, // diskSlotCapacity == 1
		BatchSize:      10,
	}
	q := newTestBufferedQueue(t, cfg, nil)
	ctx := context.Background()

	const enqueued = 5
	for i := 0; i < enqueued; i++ {
		if err := q.Enqueue(ctx, item(fmt.Sprintf("c-%d", i), make([]byte, 50))); err != nil {
			t.Fatalf("Enqueue(%d) error = %v", i, err)
		}
	}

	q.mu.Lock()
	memCount := q.memCount
	diskRefsBefore := len(q.diskRefs)
	q.mu.Unlock()
	if memCount != enqueued || diskRefsBefore != 0 {
		t.Fatalf("setup: memCount=%d diskRefs=%d, want %d resident items and 0 refs "+
			"before Close (test must isolate the flush path)", memCount, diskRefsBefore, enqueued)
	}

	if err := q.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	q.mu.Lock()
	refs := len(q.diskRefs)
	q.mu.Unlock()
	wantCap := diskSlotCapacity(cfg.MaxDiskBytes)
	if int64(refs) > wantCap {
		t.Errorf("diskRefs after Close = %d, past the cap of %d", refs, wantCap)
	}
	wantDropped := int64(enqueued) - wantCap
	if got := q.DroppedTotal(); got != wantDropped {
		t.Errorf("DroppedTotal() = %d, want %d", got, wantDropped)
	}
}

// segmentFileNums lists the WAL segment numbers present in dir, ascending.
func segmentFileNums(t *testing.T, dir string) []int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read spill dir: %v", err)
	}
	var nums []int
	for _, e := range entries {
		if n, ok := parseSegmentName(e.Name()); ok {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	return nums
}

func segmentFileCount(t *testing.T, dir string) int {
	t.Helper()
	return len(segmentFileNums(t, dir))
}

// bigSpillConfig forces every item straight to disk and gives the WAL room for
// several segments.
func bigSpillConfig(dir string) *config.BufferConfig {
	return &config.BufferConfig{
		MaxMemoryBytes: 1 << 10,
		SpillDir:       dir,
		MaxDiskBytes:   512 << 20,
		BatchSize:      64,
	}
}

// drainAndAck dequeues and acks until the queue is empty.
func drainAndAck(t *testing.T, q Queue) {
	t.Helper()
	ctx := context.Background()
	for {
		batch, err := q.DequeueBatch(ctx, 64)
		if err != nil {
			t.Fatalf("DequeueBatch() error = %v", err)
		}
		if len(batch) == 0 {
			break
		}
		ids := make([]string, len(batch))
		for i, it := range batch {
			ids[i] = it.ID
		}
		if err := q.Ack(ids); err != nil {
			t.Fatalf("Ack() error = %v", err)
		}
		if q.Depth() == 0 {
			break
		}
	}
}

// Fill several segments, ack everything, and the closed segments must go.
func TestCompactionRemovesFullyAckedSegments(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	q := newTestQueue(t, bigSpillConfig(dir))
	ctx := context.Background()

	payload := make([]byte, segmentMaxBytes/4)
	for i := 0; i < 8; i++ {
		if err := q.Enqueue(ctx, item(fmt.Sprintf("big-%d", i), payload)); err != nil {
			t.Fatalf("Enqueue(%d) error = %v", i, err)
		}
	}
	if got := segmentFileCount(t, dir); got < 2 {
		t.Fatalf("only %d segments; test needs rotation to have happened", got)
	}

	drainAndAck(t, q)

	if got := segmentFileCount(t, dir); got != 1 {
		t.Errorf("%d segment files remain after acking everything, want 1 "+
			"(only the active write segment)", got)
	}
}

// One unacked item in the lowest segment must pin the whole prefix, even when
// every later segment is fully acked. Deleting a hole would delete an ack
// whose item survives, resurrecting a delivered item on the next restart.
func TestCompactionKeepsThePrefixPinnedByOneItem(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	q := newTestQueue(t, bigSpillConfig(dir))
	ctx := context.Background()

	payload := make([]byte, segmentMaxBytes/4)
	for i := 0; i < 8; i++ {
		if err := q.Enqueue(ctx, item(fmt.Sprintf("big-%d", i), payload)); err != nil {
			t.Fatalf("Enqueue(%d) error = %v", i, err)
		}
	}
	before := segmentFileCount(t, dir)
	if before < 2 {
		t.Fatalf("only %d segments; test needs rotation to have happened", before)
	}

	// Take the oldest item and never ack it; ack everything after it.
	first, err := q.DequeueBatch(ctx, 1)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("got %d items, want 1", len(first))
	}
	drainAndAck(t, q)

	if got := segmentFileCount(t, dir); got != before {
		t.Errorf("%d segment files remain, want all %d: one unacked item in the "+
			"lowest segment must pin the entire prefix", got, before)
	}
}

// A pinned segment in the middle must stop compaction dead. Everything after
// it is fully acked and not the active segment, so an "unreferenced means
// deletable" reading would punch a hole and delete the acks belonging to the
// items still pinned behind it.
func TestCompactionPunchesNoHolePastAPinnedSegment(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	q := newTestBufferedQueue(t, bigSpillConfig(dir), nil)
	ctx := context.Background()

	payload := make([]byte, segmentMaxBytes/4)
	for i := 0; i < 10; i++ {
		if err := q.Enqueue(ctx, item(fmt.Sprintf("big-%d", i), payload)); err != nil {
			t.Fatalf("Enqueue(%d) error = %v", i, err)
		}
	}
	before := segmentFileNums(t, dir)
	if len(before) < 4 {
		t.Fatalf("segments on disk = %v, want at least 4 so a pinned middle "+
			"segment has fully-acked segments both before and after it", before)
	}

	// Pick the first item that landed in segment 1 and never ack it.
	q.mu.Lock()
	var pinnedID string
	for _, ref := range q.diskRefs {
		if ref.Segment == 1 {
			pinnedID = ref.ID
			break
		}
	}
	q.mu.Unlock()
	if pinnedID == "" {
		t.Fatalf("no reference landed in segment 1; refs = %v", before)
	}

	for {
		batch, err := q.DequeueBatch(ctx, 64)
		if err != nil {
			t.Fatalf("DequeueBatch() error = %v", err)
		}
		if len(batch) == 0 {
			break
		}
		ids := make([]string, 0, len(batch))
		for _, it := range batch {
			if it.ID == pinnedID {
				continue
			}
			ids = append(ids, it.ID)
		}
		if err := q.Ack(ids); err != nil {
			t.Fatalf("Ack() error = %v", err)
		}
		if q.Depth() == 0 {
			break
		}
	}

	got := segmentFileNums(t, dir)
	want := before[1:] // only segment 0, the prefix below the pin, may go
	if len(got) != len(want) {
		t.Fatalf("segments on disk = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("segments on disk = %v, want %v: compaction must stop at "+
				"the pinned segment and never skip past it", got, want)
		}
	}
}

// The active write segment is being appended to right now; its absence from
// refsPerSegment says nothing about whether it is needed.
func TestRemoveSegmentRefusesTheActiveWriteSegment(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	q := newTestBufferedQueue(t, bigSpillConfig(dir), nil)
	ctx := context.Background()

	payload := make([]byte, segmentMaxBytes/4)
	for i := 0; i < 5; i++ {
		if err := q.Enqueue(ctx, item(fmt.Sprintf("big-%d", i), payload)); err != nil {
			t.Fatalf("Enqueue(%d) error = %v", i, err)
		}
	}

	active := q.disk.activeSegment()
	if active == 0 {
		t.Fatalf("no rotation happened; active segment is still 0")
	}
	if err := q.disk.removeSegment(active); err == nil {
		t.Fatalf("removeSegment(active=%d) returned nil, want a refusal", active)
	}
	nums := segmentFileNums(t, dir)
	if len(nums) == 0 || nums[len(nums)-1] != active {
		t.Fatalf("active segment %d is gone from %v", active, nums)
	}
}

// After compaction and a restart, unacked items must still be recoverable and
// acked items must stay gone.
func TestCompactionSurvivesRestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := bigSpillConfig(dir)
	reg := prometheus.NewRegistry()
	q, err := New(cfg, logger.New("queue-test"), newTestQueueMetrics(t, reg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx := context.Background()

	payload := make([]byte, segmentMaxBytes/4)
	for i := 0; i < 8; i++ {
		if err := q.Enqueue(ctx, item(fmt.Sprintf("big-%d", i), payload)); err != nil {
			t.Fatalf("Enqueue(%d) error = %v", i, err)
		}
	}

	// Ack the first four, leave the rest.
	batch, err := q.DequeueBatch(ctx, 4)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	acked := map[string]bool{}
	ids := make([]string, len(batch))
	for i, it := range batch {
		ids[i] = it.ID
		acked[it.ID] = true
	}
	if err := q.Ack(ids); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reg2 := prometheus.NewRegistry()
	q2, err := New(cfg, logger.New("queue-test"), newTestQueueMetrics(t, reg2))
	if err != nil {
		t.Fatalf("reopen New() error = %v", err)
	}
	t.Cleanup(func() { _ = q2.Close() })

	seen := map[string]bool{}
	for {
		b, err := q2.DequeueBatch(ctx, 64)
		if err != nil {
			t.Fatalf("DequeueBatch() error = %v", err)
		}
		if len(b) == 0 {
			break
		}
		for _, it := range b {
			if acked[it.ID] {
				t.Errorf("acked item %q came back after restart", it.ID)
			}
			seen[it.ID] = true
		}
		if q2.Depth() == 0 {
			break
		}
	}
	if len(seen) != 4 {
		t.Errorf("recovered %d unacked items, want 4", len(seen))
	}
}

// writeSpentWAL lays down segments the way a pre-compaction binary left them:
// several rotated segments in which every record has already been acked, and
// nothing was ever removed. It returns the ids it wrote.
func writeSpentWAL(t *testing.T, dir string, maxBytes int64, n int) []string {
	t.Helper()
	ds, err := newDiskStore(dir, maxBytes, logger.New("queue-test"), nil)
	if err != nil {
		t.Fatalf("newDiskStore() error = %v", err)
	}
	payload := make([]byte, segmentMaxBytes/4)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("old-%d", i)
		if _, _, err := ds.appendItem(item(id, payload)); err != nil {
			t.Fatalf("appendItem(%s) error = %v", id, err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		if err := ds.appendAck(id); err != nil {
			t.Fatalf("appendAck(%s) error = %v", id, err)
		}
	}
	if err := ds.close(); err != nil {
		t.Fatalf("close() error = %v", err)
	}
	return ids
}

// Segments already on disk when the process starts must be collectable even
// when recovery finds nothing pending in them. Seeding the compaction cursor
// from the active segment alone strands every one of them for the lifetime of
// the process: the queue is empty, the disk is full, and no ack can ever move
// a cursor that already sits above them.
func TestCompactionReclaimsSegmentsWhenRecoveryFindsNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := bigSpillConfig(dir)
	writeSpentWAL(t, dir, cfg.MaxDiskBytes, 10)

	before := segmentFileNums(t, dir)
	if len(before) < 3 {
		t.Fatalf("segments on disk = %v, want at least 3 to have a prefix worth reclaiming", before)
	}

	q := newTestBufferedQueue(t, cfg, nil)
	ctx := context.Background()
	if got := q.Depth(); got != 0 {
		t.Fatalf("Depth() = %d, want 0: this test covers the empty-recovery case", got)
	}

	// One real item through the public API is all the agent needs to do for
	// the spent prefix to go.
	if err := q.Enqueue(ctx, item("new-0", make([]byte, segmentMaxBytes/4))); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	batch, err := q.DequeueBatch(ctx, 1)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("got %d items, want 1", len(batch))
	}
	if err := q.Ack([]string{batch[0].ID}); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}

	active := q.disk.activeSegment()
	got := segmentFileNums(t, dir)
	if len(got) != 1 || got[0] != active {
		t.Errorf("segments on disk = %v, want just the active segment [%d]: the "+
			"fully-acked prefix written before this process must be reclaimed", got, active)
	}
}

// A failed ack must not strand the claim it was about to release. The claim is
// what pins the segment and the whole prefix behind it, so an ack that removes
// the inflight entry but leaves the claim held pins that prefix for the
// lifetime of the process -- nothing is left that could ever release it.
func TestFailedAckLeavesTheSegmentReclaimable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	q := newTestBufferedQueue(t, bigSpillConfig(dir), nil)
	ctx := context.Background()

	payload := make([]byte, segmentMaxBytes/4)
	for i := 0; i < 8; i++ {
		if err := q.Enqueue(ctx, item(fmt.Sprintf("big-%d", i), payload)); err != nil {
			t.Fatalf("Enqueue(%d) error = %v", i, err)
		}
	}
	batch, err := q.DequeueBatch(ctx, 64)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if len(batch) != 8 {
		t.Fatalf("got %d items, want all 8", len(batch))
	}
	ids := make([]string, len(batch))
	for i, it := range batch {
		ids[i] = it.ID
	}

	// Jam the WAL exactly the way a full disk does: the next append, ack
	// records included, exceeds the byte cap.
	q.disk.mu.Lock()
	restore := q.disk.maxBytes
	q.disk.maxBytes = q.disk.totalBytes
	q.disk.mu.Unlock()

	if err := q.Ack(ids); err == nil {
		t.Fatalf("Ack() with a full WAL returned nil, want an error")
	}

	q.mu.Lock()
	inflight := len(q.inflight)
	claims := int64(0)
	for _, n := range q.refsPerSegment {
		claims += n
	}
	underflows := q.refUnderflows
	q.mu.Unlock()
	if inflight == 0 {
		t.Fatalf("inflight is empty after a failed ack: the items were dropped " +
			"from the queue's view while their claims stayed held")
	}
	if int64(inflight) != claims {
		t.Errorf("inflight = %d but %d claims are held; every held claim needs a "+
			"live entry that can still release it", inflight, claims)
	}

	// Unjam and retry the same ids, as a caller that got an error would.
	q.disk.mu.Lock()
	q.disk.maxBytes = restore
	q.disk.mu.Unlock()

	if err := q.Ack(ids); err != nil {
		t.Fatalf("retried Ack() error = %v", err)
	}
	if underflows != 0 {
		t.Errorf("refUnderflows = %d, want 0", underflows)
	}

	active := q.disk.activeSegment()
	got := segmentFileNums(t, dir)
	if len(got) != 1 || got[0] != active {
		t.Errorf("segments on disk = %v, want just the active segment [%d]: a "+
			"failed ack must not pin the prefix permanently", got, active)
	}
}

// The dequeue path is the second reclaim point and the only one that runs
// without an ack: an unreadable record releases its claim there, and no ack for
// it will ever arrive. Without compaction on this path a queue whose remaining
// records are all corrupt sits on a full disk forever.
func TestCompactionRunsOnTheDequeueDropPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	q := newTestBufferedQueue(t, bigSpillConfig(dir), nil)
	ctx := context.Background()

	payload := make([]byte, segmentMaxBytes/4)
	for i := 0; i < 8; i++ {
		if err := q.Enqueue(ctx, item(fmt.Sprintf("big-%d", i), payload)); err != nil {
			t.Fatalf("Enqueue(%d) error = %v", i, err)
		}
	}
	before := segmentFileNums(t, dir)
	if len(before) < 3 {
		t.Fatalf("segments on disk = %v, want at least 3", before)
	}

	// Every record in the lowest segment becomes unreadable.
	if err := os.Truncate(dir+"/"+segmentFilename(0), int64(len(walMagic)+1)); err != nil {
		t.Fatalf("truncate segment 0: %v", err)
	}

	if _, err := q.DequeueBatch(ctx, 64); err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}

	got := segmentFileNums(t, dir)
	for _, n := range got {
		if n == 0 {
			t.Fatalf("segments on disk = %v: segment 0 holds nothing but "+
				"unreadable records, and no ack will ever arrive for them", got)
		}
	}
	if q.DroppedTotal() == 0 {
		t.Fatalf("DroppedTotal() = 0, want the unreadable records counted")
	}
}

func queueGauge(t *testing.T, reg *prometheus.Registry, name string) float64 {
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
			return m.GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %q not found", name)
	return 0
}

// The segments gauge has to be right from the moment the process starts. An
// agent that has not yet had anything to reclaim is exactly the agent whose
// disk is filling, and a gauge that reads zero until the first compaction
// cannot alert on it.
func TestSegmentsGaugeTracksTheDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := bigSpillConfig(dir)
	writeSpentWAL(t, dir, cfg.MaxDiskBytes, 10)
	onDisk := float64(len(segmentFileNums(t, dir)))

	reg := prometheus.NewRegistry()
	q, err := New(cfg, logger.New("queue-test"), newTestQueueMetrics(t, reg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })

	if got := queueGauge(t, reg, "kubexa_queue_segments"); got != onDisk {
		t.Errorf("segments gauge at construction = %v, want %v (before any "+
			"compaction has run)", got, onDisk)
	}

	ctx := context.Background()
	if err := q.Enqueue(ctx, item("new-0", make([]byte, segmentMaxBytes/4))); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	batch, err := q.DequeueBatch(ctx, 1)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if err := q.Ack([]string{batch[0].ID}); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}

	// Derived from the setup, not read back out of the directory afterwards:
	// everything written above is acked, so the only segment that may survive
	// is the one being written to. Taking the expectation from the state under
	// test would let a broken gauge agree with a broken directory.
	const wantAfter = 1
	if got := segmentFileNums(t, dir); len(got) != wantAfter {
		t.Fatalf("segments on disk = %v, want %d after acking everything", got, wantAfter)
	}
	if got := queueGauge(t, reg, "kubexa_queue_segments"); got != wantAfter {
		t.Errorf("segments gauge after compaction = %v, want %v", got, float64(wantAfter))
	}
}

// syncBuffer collects log output for assertions.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const (
	compactFailMsg     = "WAL segment reclamation is failing"
	compactRecoveryMsg = "WAL segment reclamation recovered"
)

// Removal failure is announced on the transition, not on every attempt. The
// warning runs from both reclaim points, so on a spill directory that has gone
// read-only, one line per attempt is a line per ack and per dequeue -- hundreds
// of megabytes a day into the same pipeline that carries the real diagnostics.
//
// The unlink is failed through diskStore's removeFile seam rather than through
// directory permissions: a chmod does not stop root, so a permissions-based
// version of this test passes vacuously wherever CI runs as root, which is the
// environment that matters. The assertions are on the emitted log lines, not on
// the internal flag, so making the warning unconditional again fails this test.
func TestCompactionFailureWarnsOnceAndAnnouncesRecovery(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logs := &syncBuffer{}
	q, err := New(bigSpillConfig(dir), logger.New("queue-test", logger.WithWriter(logs)),
		newTestQueueMetrics(t, prometheus.NewRegistry()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	bq, ok := q.(*bufferedQueue)
	if !ok {
		t.Fatalf("New() returned %T, want *bufferedQueue", q)
	}
	ctx := context.Background()

	payload := make([]byte, segmentMaxBytes/4)
	for i := 0; i < 8; i++ {
		if err := q.Enqueue(ctx, item(fmt.Sprintf("big-%d", i), payload)); err != nil {
			t.Fatalf("Enqueue(%d) error = %v", i, err)
		}
	}
	before := segmentFileNums(t, dir)
	if len(before) < 2 {
		t.Fatalf("segments on disk = %v, want at least 2", before)
	}
	batch, err := q.DequeueBatch(ctx, 64)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	ids := make([]string, len(batch))
	for i, it := range batch {
		ids[i] = it.ID
	}

	bq.disk.mu.Lock()
	bq.disk.removeFile = func(string) error { return fmt.Errorf("injected unlink failure") }
	bq.disk.mu.Unlock()

	// The ack itself succeeds; only the reclamation behind it fails.
	if err := q.Ack(ids); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	if got := segmentFileNums(t, dir); len(got) != len(before) {
		t.Fatalf("segments on disk = %v, want all %d still there: the injected "+
			"failure did not take effect, so this test proves nothing", got, len(before))
	}
	if n := strings.Count(logs.String(), compactFailMsg); n != 1 {
		t.Fatalf("failure announced %d times after the first failing compaction, want 1", n)
	}

	// Acking ids that are no longer inflight is a no-op for the ack loop and
	// still runs compaction, which is the retry under test.
	for i := 0; i < 3; i++ {
		if err := q.Ack(ids); err != nil {
			t.Fatalf("repeat Ack() error = %v", err)
		}
	}
	bq.mu.Lock()
	failures := bq.compactFailures
	bq.mu.Unlock()
	if failures < 4 {
		t.Fatalf("compactFailures = %d after 4 failing compactions, want at least 4: "+
			"the retries did not happen", failures)
	}
	if n := strings.Count(logs.String(), compactFailMsg); n != 1 {
		t.Errorf("failure announced %d times across %d failed attempts, want 1",
			n, failures)
	}
	if n := strings.Count(logs.String(), compactRecoveryMsg); n != 0 {
		t.Errorf("recovery announced %d times while removal is still failing, want 0", n)
	}

	bq.disk.mu.Lock()
	bq.disk.removeFile = nil
	bq.disk.mu.Unlock()

	if err := q.Ack(ids); err != nil {
		t.Fatalf("Ack() after recovery error = %v", err)
	}
	if got := segmentFileNums(t, dir); len(got) != 1 {
		t.Fatalf("segments on disk = %v, want just the active one once removal works", got)
	}
	if n := strings.Count(logs.String(), compactRecoveryMsg); n != 1 {
		t.Errorf("recovery announced %d times, want exactly 1", n)
	}
	if n := strings.Count(logs.String(), compactFailMsg); n != 1 {
		t.Errorf("failure announced %d times in total, want 1", n)
	}
}

// A spill directory that cannot be read is not a spill directory with no
// segments. Folding the two together makes a transient failure reopen segment 0
// underneath existing files, which disables compaction for the lifetime of the
// process with nothing logged anywhere.
func TestSegmentListingDistinguishesUnreadableFromEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	reg := prometheus.NewRegistry()
	ds, err := newDiskStore(dir, 512<<20, logger.New("queue-test"), newTestQueueMetrics(t, reg))
	if err != nil {
		t.Fatalf("newDiskStore() error = %v", err)
	}

	// An empty directory: no segments, no error. (createSegment has made one.)
	nums, err := ds.segmentNums()
	if err != nil {
		t.Fatalf("segmentNums() on a readable dir error = %v", err)
	}
	if len(nums) != 1 {
		t.Fatalf("segmentNums() = %v, want the one segment newDiskStore created", nums)
	}
	seeded := queueGauge(t, reg, "kubexa_queue_segments")
	if seeded != 1 {
		t.Fatalf("segments gauge = %v, want 1", seeded)
	}

	// Now make it unreadable for any user, root included.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove spill dir: %v", err)
	}

	if _, err := ds.segmentNums(); err == nil {
		t.Errorf("segmentNums() on an unreadable dir returned nil error")
	}
	if _, ok, err := ds.lowestSegmentOnDisk(); err == nil {
		t.Errorf("lowestSegmentOnDisk() returned (ok=%v, nil error) on an "+
			"unreadable dir; reporting \"no segments\" here seeds the compaction "+
			"cursor above every file in the directory", ok)
	}

	// The gauge must keep its last good value rather than report the healthiest
	// possible number at the moment the store lost sight of its directory.
	ds.updateSegmentsGauge()
	if got := queueGauge(t, reg, "kubexa_queue_segments"); got != seeded {
		t.Errorf("segments gauge = %v after the dir became unreadable, want it "+
			"left at %v", got, seeded)
	}
}

// A clean shutdown must not look like data loss. Items still referenced when
// Close runs are durable and unacked -- the next start replays them -- so a
// DequeueBatch after Close must not count them as unreadable drops, and must
// not block waiting for a store that will never answer.
func TestDequeueAfterCloseIsNotDataLoss(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	reg := prometheus.NewRegistry()
	q, err := New(bigSpillConfig(dir), logger.New("queue-test"), newTestQueueMetrics(t, reg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx := context.Background()

	payload := make([]byte, segmentMaxBytes/4)
	for i := 0; i < 4; i++ {
		if err := q.Enqueue(ctx, item(fmt.Sprintf("big-%d", i), payload)); err != nil {
			t.Fatalf("Enqueue(%d) error = %v", i, err)
		}
	}
	if q.Depth() == 0 {
		t.Fatalf("Depth() = 0, want the spilled items pending")
	}
	if err := q.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// A regression here hangs rather than fails, so bound it.
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	batch, err := q.DequeueBatch(deadline, 64)
	if err != nil {
		t.Fatalf("DequeueBatch() after Close error = %v, want a clean empty result", err)
	}
	if len(batch) != 0 {
		t.Fatalf("DequeueBatch() after Close returned %d items", len(batch))
	}
	if got := q.DroppedTotal(); got != 0 {
		t.Errorf("DroppedTotal() = %d after a clean shutdown, want 0", got)
	}
	assertCounter(t, reg, "kubexa_queue_disk_read_errors_total", 0)
}

// An item that keeps coming back must eventually stop coming back. Attempts
// was incremented on every nack and read by nobody, so a caller that nacked
// whatever it could not deliver -- which is what the drain loop now does when
// an ack cannot be persisted -- redelivered the same item forever: duplicate
// traffic to the gateway, a pegged core, and a log line per round.
//
// The cap belongs here rather than in the caller because Nack is the single
// choke point every redelivery passes through.
func TestNackRetiresAnItemPastTheAttemptCap(t *testing.T) {
	t.Parallel()

	// MaxMemoryBytes below any item size sends everything straight to the
	// spill path, so the retired item is disk-sourced and its segment claim
	// has to be handed back too.
	cfg := &config.BufferConfig{
		MaxMemoryBytes: 1,
		SpillDir:       t.TempDir(),
		MaxDiskBytes:   64 << 20,
		BatchSize:      10,
	}
	bq := newTestBufferedQueue(t, cfg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := bq.Enqueue(ctx, item("poison", []byte("payload"))); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	// The loop bound is deliberately far above any sane cap: the assertion is
	// that redelivery stops, not that it stops on a particular round.
	const maxRounds = 50
	rounds := 0
	for ; rounds < maxRounds; rounds++ {
		batch, err := bq.DequeueBatch(ctx, 1)
		if err != nil {
			t.Fatalf("DequeueBatch() error = %v", err)
		}
		if len(batch) == 0 {
			break
		}
		if err := bq.Nack([]string{batch[0].ID}); err != nil {
			t.Fatalf("Nack() error = %v", err)
		}
		if bq.Depth() == 0 {
			rounds++
			break
		}
	}
	if rounds >= maxRounds {
		t.Fatalf("item was still being redelivered after %d nacks; there is no attempt cap", rounds)
	}

	if got := bq.Depth(); got != 0 {
		t.Errorf("Depth() = %d, want 0 after the attempt cap retired the item", got)
	}
	if got := bq.InflightLen(); got != 0 {
		t.Errorf("InflightLen() = %d, want 0", got)
	}
	if got := bq.SegmentClaims(); got != 0 {
		t.Errorf("SegmentClaims() = %d, want 0 -- a retired item must hand its claim back", got)
	}
	// SegmentClaims reading 0 does not prove the claim was released once:
	// releaseDiskRefUnlocked clamps at zero, so a double release reads exactly
	// the same. This counter is the only evidence, and a double release is what
	// lets compaction unlink a segment whose records are still live.
	if got := bq.RefUnderflows(); got != 0 {
		t.Errorf("RefUnderflows() = %d, want 0 -- the claim was released more than once", got)
	}
	if got := bq.DroppedTotal(); got != 1 {
		t.Errorf("DroppedTotal() = %d, want exactly 1", got)
	}
	if got := bq.DeliveredUnrecordedTotal(); got != 0 {
		t.Errorf("DeliveredUnrecordedTotal() = %d, want 0 -- this item never reached the gateway", got)
	}
}

// Nack returned on the first requeue failure, so every id after it in the same
// call was neither requeued nor counted: silently gone, with dropped_total
// unmoved. Reachable now that a failed ack falls back to a nack -- on a full
// disk that is exactly this path.
func TestNackSettlesEveryIDWhenARequeueFails(t *testing.T) {
	t.Parallel()

	// Two memory slots, so exactly one nacked item can be requeued once one
	// slot is occupied.
	cfg := &config.BufferConfig{
		MaxMemoryBytes: 2 * avgItemSizeEstimate,
		SpillDir:       t.TempDir(),
		MaxDiskBytes:   64 << 20,
		BatchSize:      10,
	}
	bq := newTestBufferedQueue(t, cfg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, id := range []string{"a", "b"} {
		if err := bq.Enqueue(ctx, item(id, []byte("payload"))); err != nil {
			t.Fatalf("Enqueue(%s) error = %v", id, err)
		}
	}
	batch, err := bq.DequeueBatch(ctx, 2)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if len(batch) != 2 {
		t.Fatalf("dequeued %d items, want 2", len(batch))
	}

	// Occupy one of the two slots, then take the spill path away: a closed
	// store fails every write, the same way a full disk does.
	if err := bq.Enqueue(ctx, item("c", []byte("payload"))); err != nil {
		t.Fatalf("Enqueue(c) error = %v", err)
	}
	if err := bq.disk.close(); err != nil {
		t.Fatalf("close disk store: %v", err)
	}

	// Nack processes the ids back-to-front to preserve order, so "b" takes the
	// free slot and "a" has nowhere to go.
	if err := bq.Nack([]string{"a", "b"}); err == nil {
		t.Fatal("Nack() error = nil, want the requeue failure reported")
	}

	if got := bq.InflightLen(); got != 0 {
		t.Errorf("InflightLen() = %d, want 0", got)
	}
	if got := bq.Depth(); got != 2 {
		t.Errorf("Depth() = %d, want 2 (the occupant plus the one item that fit)", got)
	}
	if got := bq.DroppedTotal(); got != 1 {
		t.Errorf("DroppedTotal() = %d, want 1 -- the id that could not be requeued must be counted, not abandoned", got)
	}
}

// An item retired at the cap after a SUCCESSFUL send is not data loss: the
// gateway has it, and only the agent's record of the delivery failed. Counting
// it in dropped_total tells an operator they lost telemetry they did not lose,
// in the one metric they would use to decide exactly that.
func TestRetirementAfterDeliveryIsNotCountedAsLoss(t *testing.T) {
	t.Parallel()

	cfg := &config.BufferConfig{
		MaxMemoryBytes: 1,
		SpillDir:       t.TempDir(),
		MaxDiskBytes:   64 << 20,
		BatchSize:      10,
	}
	bq := newTestBufferedQueue(t, cfg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := bq.Enqueue(ctx, item("delivered", []byte("payload"))); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	const maxRounds = 50
	rounds := 0
	for ; rounds < maxRounds; rounds++ {
		batch, err := bq.DequeueBatch(ctx, 1)
		if err != nil {
			t.Fatalf("DequeueBatch() error = %v", err)
		}
		if len(batch) == 0 {
			break
		}
		// Every round: the send succeeded, the ack could not be written.
		if err := bq.NackDelivered([]string{batch[0].ID}); err != nil {
			t.Fatalf("NackDelivered() error = %v", err)
		}
		if bq.Depth() == 0 {
			rounds++
			break
		}
	}
	if rounds >= maxRounds {
		t.Fatalf("still redelivering after %d attempts", rounds)
	}

	if got := bq.DroppedTotal(); got != 0 {
		t.Errorf("DroppedTotal() = %d, want 0 -- the gateway has this data", got)
	}
	if got := bq.DeliveredUnrecordedTotal(); got != 1 {
		t.Errorf("DeliveredUnrecordedTotal() = %d, want 1", got)
	}
	if got := bq.InflightLen(); got != 0 {
		t.Errorf("InflightLen() = %d, want 0", got)
	}
	if got := bq.SegmentClaims(); got != 0 {
		t.Errorf("SegmentClaims() = %d, want 0", got)
	}
	if got := bq.RefUnderflows(); got != 0 {
		t.Errorf("RefUnderflows() = %d, want 0 -- a claim was released more than once", got)
	}
}

// An attempt count must record attempts that actually happened. An item that
// was never handed to the wire -- everything queued behind the one item whose
// send failed -- has used none of its six, and charging it one retires healthy
// items on somebody else's failure: with a poison item at the head of a batch,
// the whole batch was gone by round six.
func TestNackUntriedChargesNoAttempt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *config.BufferConfig
	}{
		{
			// Everything spills, so the attempt count lives on the diskRef.
			name: "disk",
			cfg: &config.BufferConfig{
				MaxMemoryBytes: 1,
				SpillDir:       t.TempDir(),
				MaxDiskBytes:   64 << 20,
				BatchSize:      10,
			},
		},
		{
			// Resident item: the attempt count lives on the Item itself, and
			// nothing in the disk path is exercised.
			name: "memory",
			cfg:  testBufferConfig(t, "", 64<<10),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bq := newTestBufferedQueue(t, tc.cfg, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			if err := bq.Enqueue(ctx, item("untried", []byte("payload"))); err != nil {
				t.Fatalf("Enqueue() error = %v", err)
			}

			// Well past the cap: an untried item may come round forever, because
			// nothing about it has been tried yet.
			const rounds = 3 * (maxDeliveryAttempts + 1)
			for i := 0; i < rounds; i++ {
				batch, err := bq.DequeueBatch(ctx, 1)
				if err != nil {
					t.Fatalf("DequeueBatch() round %d error = %v", i, err)
				}
				if len(batch) != 1 || batch[0].ID != "untried" {
					t.Fatalf("round %d: got %v, want the item back -- it was retired on attempts it never used", i, batch)
				}
				if batch[0].Attempts != 0 {
					t.Fatalf("round %d: Attempts = %d, want 0 -- the item was never sent", i, batch[0].Attempts)
				}
				if err := bq.NackUntried([]string{batch[0].ID}); err != nil {
					t.Fatalf("NackUntried() round %d error = %v", i, err)
				}
			}

			if got := bq.DroppedTotal(); got != 0 {
				t.Errorf("DroppedTotal() = %d, want 0 -- nothing was ever attempted", got)
			}
			if got := bq.Depth(); got != 1 {
				t.Errorf("Depth() = %d, want 1", got)
			}
			if got := bq.InflightLen(); got != 0 {
				t.Errorf("InflightLen() = %d, want 0", got)
			}
			if got := bq.RefUnderflows(); got != 0 {
				t.Errorf("RefUnderflows() = %d, want 0", got)
			}

			// The cap still exists for this item: once its sends really are
			// attempted, it retires like any other poison record.
			for i := 0; i <= maxDeliveryAttempts; i++ {
				batch, err := bq.DequeueBatch(ctx, 1)
				if err != nil {
					t.Fatalf("DequeueBatch() attempt %d error = %v", i, err)
				}
				if len(batch) == 0 {
					t.Fatalf("item retired after %d real attempts, want %d", i, maxDeliveryAttempts+1)
				}
				if err := bq.Nack([]string{batch[0].ID}); err != nil {
					t.Fatalf("Nack() attempt %d error = %v", i, err)
				}
			}
			if got := bq.Depth(); got != 0 {
				t.Errorf("Depth() = %d, want 0 -- the attempt cap must still retire a genuinely poison item", got)
			}
			if got := bq.DroppedTotal(); got != 1 {
				t.Errorf("DroppedTotal() = %d, want 1", got)
			}
		})
	}
}

// nack_total is documented as "negative-acknowledged (requeued) items" and is
// read during exactly the incident it exists for. An item retired at the
// attempt cap is not requeued -- it is gone, and dropped_total already says so.
func TestRetirementIsNotCountedAsANack(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	cfg := &config.BufferConfig{
		MaxMemoryBytes: 1,
		SpillDir:       t.TempDir(),
		MaxDiskBytes:   64 << 20,
		BatchSize:      10,
	}
	bq := newTestBufferedQueue(t, cfg, reg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := bq.Enqueue(ctx, item("poison", []byte("payload"))); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	for i := 0; i <= maxDeliveryAttempts; i++ {
		batch, err := bq.DequeueBatch(ctx, 1)
		if err != nil {
			t.Fatalf("DequeueBatch() round %d error = %v", i, err)
		}
		if len(batch) == 0 {
			t.Fatalf("item retired after %d rounds, want %d", i, maxDeliveryAttempts+1)
		}
		if err := bq.Nack([]string{batch[0].ID}); err != nil {
			t.Fatalf("Nack() round %d error = %v", i, err)
		}
	}

	if got := bq.DroppedTotal(); got != 1 {
		t.Fatalf("DroppedTotal() = %d, want 1 -- the item should be retired by now", got)
	}
	// Six requeues, then a retirement: the retirement belongs to dropped_total
	// and nowhere else.
	assertCounter(t, reg, "kubexa_queue_nack_total", maxDeliveryAttempts)
	assertCounter(t, reg, "kubexa_queue_dropped_total", 1)
}

// A diskRef is a byte location. If one ever drifts -- a torn write, a bad
// recovery, a future refactor -- the read still succeeds and hands back
// whatever record happens to sit there: the wrong payload delivered under a
// borrowed identity, and an ack that retires an id nobody sent. One comparison
// turns it into an ordinary unreadable-record drop.
func TestRefPointingAtAnotherRecordIsDroppedNotDelivered(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	cfg := &config.BufferConfig{
		MaxMemoryBytes: 1,
		SpillDir:       t.TempDir(),
		MaxDiskBytes:   64 << 20,
		BatchSize:      10,
	}
	bq := newTestBufferedQueue(t, cfg, reg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, id := range []string{"a", "b"} {
		if err := bq.Enqueue(ctx, item(id, []byte("payload-"+id))); err != nil {
			t.Fatalf("Enqueue(%s) error = %v", id, err)
		}
	}

	// Point a's reference at b's record: a perfectly valid item record, just
	// not the one that was asked for.
	bq.mu.Lock()
	if len(bq.diskRefs) != 2 {
		bq.mu.Unlock()
		t.Fatalf("diskRefs = %d, want 2 -- both items must have spilled", len(bq.diskRefs))
	}
	bq.diskRefs[0].Segment = bq.diskRefs[1].Segment
	bq.diskRefs[0].Offset = bq.diskRefs[1].Offset
	bq.mu.Unlock()

	batch, err := bq.DequeueBatch(ctx, 2)
	if err != nil {
		t.Fatalf("DequeueBatch() error = %v", err)
	}
	if len(batch) != 1 || batch[0].ID != "b" {
		t.Fatalf("got %v, want just %q -- a reference that reads back a different item must not be delivered", batch, "b")
	}
	if got := string(batch[0].Payload); got != "payload-b" {
		t.Errorf("payload = %q, want %q", got, "payload-b")
	}
	if got := bq.DroppedTotal(); got != 1 {
		t.Errorf("DroppedTotal() = %d, want 1 -- the mismatched reference must be counted, not silently delivered", got)
	}
	assertCounter(t, reg, "kubexa_queue_disk_read_errors_total", 1)
	if got := bq.RefUnderflows(); got != 0 {
		t.Errorf("RefUnderflows() = %d, want 0", got)
	}
}
