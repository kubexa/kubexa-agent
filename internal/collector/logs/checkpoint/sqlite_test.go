package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteStoreSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ctx := context.Background()

	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	key := Key{PodUID: "uid-1", Container: "app"}
	ts := time.Date(2024, 6, 1, 12, 0, 5, 0, time.UTC)
	meta := Metadata{Namespace: "default", PodName: "my-pod"}

	if err := s1.Save(ctx, key, ts, meta); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := s1.Flush(ctx); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	got, ok, err := s2.Load(ctx, key)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !ok {
		t.Fatal("Load() ok = false, want true")
	}
	if !got.Equal(ts) {
		t.Fatalf("Load() = %v, want %v", got, ts)
	}
}

func TestSQLiteStoreLoadMissing(t *testing.T) {
	t.Parallel()

	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	_, ok, err := s.Load(context.Background(), Key{PodUID: "missing", Container: "c"})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if ok {
		t.Fatal("Load() ok = true, want false")
	}
}

func TestSQLiteStoreSaveMonotonic(t *testing.T) {
	t.Parallel()

	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	key := Key{PodUID: "uid-2", Container: "c"}
	newer := time.Date(2024, 6, 1, 13, 0, 0, 0, time.UTC)
	older := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	if err := s.Save(ctx, key, newer, Metadata{}); err != nil {
		t.Fatalf("Save(newer) error = %v", err)
	}
	if err := s.Save(ctx, key, older, Metadata{}); err != nil {
		t.Fatalf("Save(older) error = %v", err)
	}

	got, ok, err := s.Load(ctx, key)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !ok || !got.Equal(newer) {
		t.Fatalf("Load() = %v ok=%v, want %v", got, ok, newer)
	}
}

func TestSQLiteStoreUsesStreamsDB(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	_ = s.Close()

	if _, err := os.Stat(filepath.Join(dir, dbFileName)); err != nil {
		t.Fatalf("db file missing: %v", err)
	}
}
