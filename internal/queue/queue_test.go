package queue

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/kubexa/kubexa-agent/internal/logger"
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

func newTestQueue(t *testing.T, cfg *config.BufferConfig) Queue {
	t.Helper()
	reg := prometheus.NewRegistry()
	q, err := New(cfg, logger.New("queue-test"), reg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
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
		q, err := New(cfg, logger.New("queue-test"), prometheus.NewRegistry())
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

	q2, err := New(cfg, logger.New("queue-test"), prometheus.NewRegistry())
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
	q, err := New(cfg, logger.New("queue-test"), reg)
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
	q, err := New(cfg, logger.New("queue-test"), reg)
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

	_, err := New(&config.BufferConfig{MaxMemoryBytes: 0}, nil, prometheus.NewRegistry())
	if err == nil {
		t.Fatal("expected error for invalid max memory")
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
