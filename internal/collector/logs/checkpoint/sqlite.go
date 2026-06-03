package checkpoint

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // register pure-Go SQLite driver
)

const (
	sqliteDriverName = "sqlite"
	dbFileName         = "streams.db"
	schemaDDL          = `
CREATE TABLE IF NOT EXISTS log_stream_checkpoints (
  pod_uid           TEXT NOT NULL,
  container_name    TEXT NOT NULL,
  namespace         TEXT NOT NULL DEFAULT '',
  pod_name          TEXT NOT NULL DEFAULT '',
  last_processed_ns INTEGER NOT NULL,
  updated_at_ns     INTEGER NOT NULL,
  PRIMARY KEY (pod_uid, container_name)
);`
)

// SQLiteStore implements Store using a local SQLite database file.
type SQLiteStore struct {
	mu   sync.Mutex
	db   *sql.DB
	path string
}

// Open creates or opens the checkpoint database under dir.
func Open(dir string) (*SQLiteStore, error) {
	dir = filepath.Clean(dir)
	if dir == "" || dir == "." {
		return nil, errors.New("checkpoint dir is required")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create checkpoint dir %q: %w", dir, err)
	}

	dbPath := filepath.Join(dir, dbFileName)
	db, err := sql.Open(sqliteDriverName, dbPath)
	if err != nil {
		return nil, fmt.Errorf("open checkpoint db: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &SQLiteStore{db: db, path: dbPath}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) init() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	}
	for _, p := range pragmas {
		if _, err := s.db.Exec(p); err != nil {
			return fmt.Errorf("checkpoint pragma %q: %w", p, err)
		}
	}
	if _, err := s.db.Exec(schemaDDL); err != nil {
		return fmt.Errorf("checkpoint schema: %w", err)
	}
	return nil
}

// Load returns the last processed log timestamp for key.
func (s *SQLiteStore) Load(ctx context.Context, key Key) (time.Time, bool, error) {
	if err := validateKey(key); err != nil {
		return time.Time{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var lastNS int64
	err := s.db.QueryRowContext(ctx,
		`SELECT last_processed_ns FROM log_stream_checkpoints WHERE pod_uid = ? AND container_name = ?`,
		key.PodUID, key.Container,
	).Scan(&lastNS)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("load checkpoint: %w", err)
	}
	return time.Unix(0, lastNS).UTC(), true, nil
}

// Save records the last processed log timestamp for key.
func (s *SQLiteStore) Save(ctx context.Context, key Key, lastProcessed time.Time, meta Metadata) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if lastProcessed.IsZero() {
		return errors.New("last processed timestamp is zero")
	}

	lastProcessed = lastProcessed.UTC()
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
INSERT INTO log_stream_checkpoints (
  pod_uid, container_name, namespace, pod_name, last_processed_ns, updated_at_ns
) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(pod_uid, container_name) DO UPDATE SET
  namespace = excluded.namespace,
  pod_name = excluded.pod_name,
  last_processed_ns = excluded.last_processed_ns,
  updated_at_ns = excluded.updated_at_ns
WHERE excluded.last_processed_ns > log_stream_checkpoints.last_processed_ns`,
		key.PodUID,
		key.Container,
		meta.Namespace,
		meta.PodName,
		lastProcessed.UnixNano(),
		now.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("save checkpoint: %w", err)
	}
	return nil
}

// Flush ensures pending writes are durable.
func (s *SQLiteStore) Flush(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint flush: %w", err)
	}
	return nil
}

// Close releases database resources.
func (s *SQLiteStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	if err != nil {
		return fmt.Errorf("close checkpoint db: %w", err)
	}
	return nil
}

func validateKey(key Key) error {
	if key.PodUID == "" {
		return errors.New("checkpoint key pod_uid is required")
	}
	if key.Container == "" {
		return errors.New("checkpoint key container is required")
	}
	return nil
}
