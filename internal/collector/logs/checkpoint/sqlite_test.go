package checkpoint

import (
	"context"
	"database/sql"
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

	pos := Position{
		LastProcessed: ts,
		LastRead:      ts.Add(time.Millisecond),
		Recent:        []Delivered{{ReadTime: ts.Add(time.Millisecond), Digest: "abc123"}},
	}

	if err := s1.Save(ctx, key, pos, meta); err != nil {
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
	if !got.LastProcessed.Equal(ts) {
		t.Fatalf("LastProcessed = %v, want %v", got.LastProcessed, ts)
	}
	// Both halves of a position have to survive together: a resume that comes
	// back without the record of what it will replay re-delivers it.
	if !got.LastRead.Equal(ts.Add(time.Millisecond)) {
		t.Fatalf("LastRead = %v, want %v", got.LastRead, ts.Add(time.Millisecond))
	}
	if len(got.Recent) != 1 || got.Recent[0].Digest != "abc123" || !got.Recent[0].ReadTime.Equal(ts.Add(time.Millisecond)) {
		t.Fatalf("Recent = %+v, want one entry matching what was saved", got.Recent)
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

	if err := s.Save(ctx, key, Position{LastProcessed: newer, LastRead: newer}, Metadata{}); err != nil {
		t.Fatalf("Save(newer) error = %v", err)
	}
	if err := s.Save(ctx, key, Position{LastProcessed: older, LastRead: older}, Metadata{}); err != nil {
		t.Fatalf("Save(older) error = %v", err)
	}

	got, ok, err := s.Load(ctx, key)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !ok || !got.LastProcessed.Equal(newer) {
		t.Fatalf("Load() = %v ok=%v, want %v", got.LastProcessed, ok, newer)
	}
}

// TestSQLiteStoreAdmitsProgressInTheReadFrameAlone guards the half of the
// monotonic rule that is easy to lose.
//
// A workload's own clock can stand still or run backwards -- it is a string
// in its log line. The read frame cannot. Guarding only on the record clock
// would freeze the whole row, de-duplication set included, at the moment the
// workload's clock stopped advancing, and the resume would go back to
// replaying records nothing remembers sending.
func TestSQLiteStoreAdmitsProgressInTheReadFrameAlone(t *testing.T) {
	t.Parallel()

	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	key := Key{PodUID: "uid-stuck", Container: "c"}
	frozen := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	readFirst := time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)
	readLater := readFirst.Add(time.Minute)

	if err := s.Save(ctx, key, Position{LastProcessed: frozen, LastRead: readFirst}, Metadata{}); err != nil {
		t.Fatalf("Save(first) error = %v", err)
	}
	if err := s.Save(ctx, key, Position{
		LastProcessed: frozen,
		LastRead:      readLater,
		Recent:        []Delivered{{ReadTime: readLater, Digest: "deadbeef"}},
	}, Metadata{}); err != nil {
		t.Fatalf("Save(later) error = %v", err)
	}

	got, ok, err := s.Load(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Load() err=%v ok=%v", err, ok)
	}
	if !got.LastRead.Equal(readLater) {
		t.Fatalf("LastRead = %v, want %v: a row whose record clock is frozen must still advance in the read frame", got.LastRead, readLater)
	}
	if len(got.Recent) != 1 || got.Recent[0].Digest != "deadbeef" {
		t.Fatalf("Recent = %+v, want the set written with the advancing read", got.Recent)
	}
}

// TestSQLiteStoreUpgradesAnOlderDatabase is the case a PVC guarantees: the
// file outlives the agent that wrote it, so the first upgrade past these
// columns opens a table that has neither. It must open, keep the position
// already there, and start recording the new fields.
func TestSQLiteStoreUpgradesAnOlderDatabase(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ctx := context.Background()
	key := Key{PodUID: "uid-old", Container: "app"}
	ts := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

	// The schema exactly as it shipped before read times were stored.
	db, err := sql.Open(sqliteDriverName, filepath.Join(dir, dbFileName))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`
CREATE TABLE log_stream_checkpoints (
  pod_uid           TEXT NOT NULL,
  container_name    TEXT NOT NULL,
  namespace         TEXT NOT NULL DEFAULT '',
  pod_name          TEXT NOT NULL DEFAULT '',
  last_processed_ns INTEGER NOT NULL,
  updated_at_ns     INTEGER NOT NULL,
  PRIMARY KEY (pod_uid, container_name)
);`); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO log_stream_checkpoints (pod_uid, container_name, last_processed_ns, updated_at_ns) VALUES (?, ?, ?, ?)`,
		key.PodUID, key.Container, ts.UnixNano(), ts.UnixNano(),
	); err != nil {
		t.Fatalf("seed old row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() on an older database error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	got, ok, err := s.Load(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Load() err=%v ok=%v", err, ok)
	}
	if !got.LastProcessed.Equal(ts) {
		t.Fatalf("LastProcessed = %v, want %v", got.LastProcessed, ts)
	}
	// No read time was ever recorded for this row, and inventing one would
	// resume in a frame the row never measured. The cursor falls back to the
	// record clock, exactly as it did before.
	if !got.LastRead.IsZero() {
		t.Fatalf("LastRead = %v, want zero on a row written before it existed", got.LastRead)
	}

	later := ts.Add(time.Hour)
	if err := s.Save(ctx, key, Position{
		LastProcessed: later,
		LastRead:      later,
		Recent:        []Delivered{{ReadTime: later, Digest: "feed"}},
	}, Metadata{}); err != nil {
		t.Fatalf("Save() after upgrade error = %v", err)
	}
	got, _, err = s.Load(ctx, key)
	if err != nil {
		t.Fatalf("Load() after upgrade error = %v", err)
	}
	if !got.LastRead.Equal(later) || len(got.Recent) != 1 {
		t.Fatalf("after upgrade LastRead=%v Recent=%+v, want the new fields recorded", got.LastRead, got.Recent)
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
