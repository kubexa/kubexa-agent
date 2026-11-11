package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // register pure-Go SQLite driver
)

const (
	sqliteDriverName = "sqlite"
	dbFileName       = "streams.db"
	schemaDDL        = `
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

var (
	// addedColumns are applied to databases created before they existed. A
	// PVC outlives the agent that wrote it, so the first upgrade past this
	// point opens a table with neither column; SQLite has no ADD COLUMN IF
	// NOT EXISTS, so a duplicate-column error from a newer file is expected
	// and ignored.
	addedColumns = []string{
		`ALTER TABLE log_stream_checkpoints ADD COLUMN last_read_ns INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE log_stream_checkpoints ADD COLUMN recent_json TEXT NOT NULL DEFAULT ''`,
	}
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
	for _, stmt := range addedColumns {
		if _, err := s.db.Exec(stmt); err != nil && !isDuplicateColumn(err) {
			return fmt.Errorf("checkpoint schema upgrade: %w", err)
		}
	}
	return nil
}

// isDuplicateColumn reports the one ALTER failure that means the column is
// already there. Matched on the message because modernc.org/sqlite's error
// does not expose the extended result code through a comparable sentinel.
func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate column name")
}

// Load returns the stream's position for key.
func (s *SQLiteStore) Load(ctx context.Context, key Key) (Position, bool, error) {
	if err := validateKey(key); err != nil {
		return Position{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var (
		lastNS     int64
		lastReadNS int64
		recentJSON string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT last_processed_ns, last_read_ns, recent_json
		   FROM log_stream_checkpoints WHERE pod_uid = ? AND container_name = ?`,
		key.PodUID, key.Container,
	).Scan(&lastNS, &lastReadNS, &recentJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return Position{}, false, nil
	}
	if err != nil {
		return Position{}, false, fmt.Errorf("load checkpoint: %w", err)
	}

	pos := Position{LastProcessed: time.Unix(0, lastNS).UTC()}
	if lastReadNS > 0 {
		pos.LastRead = time.Unix(0, lastReadNS).UTC()
	}
	// A row written before these columns existed, or one whose set was
	// unreadable, resumes with no de-duplication rather than no position:
	// one replayed record beats refusing to resume at all.
	if recentJSON != "" {
		var recent []Delivered
		if err := json.Unmarshal([]byte(recentJSON), &recent); err == nil {
			for i := range recent {
				recent[i].ReadTime = recent[i].ReadTime.UTC()
			}
			pos.Recent = recent
		}
	}
	return pos, true, nil
}

// Save records the stream's position for key.
func (s *SQLiteStore) Save(ctx context.Context, key Key, pos Position, meta Metadata) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if pos.LastProcessed.IsZero() {
		return errors.New("last processed timestamp is zero")
	}

	lastProcessed := pos.LastProcessed.UTC()
	var lastReadNS int64
	if !pos.LastRead.IsZero() {
		lastReadNS = pos.LastRead.UTC().UnixNano()
	}
	recentJSON := ""
	if len(pos.Recent) > 0 {
		if encoded, err := json.Marshal(pos.Recent); err == nil {
			recentJSON = string(encoded)
		}
	}
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	// The guard admits progress in EITHER frame. A workload whose own clock
	// stands still or runs backwards still reads forward, and guarding on the
	// record clock alone would freeze the de-duplication set at the moment
	// that clock stopped advancing -- leaving the resume replaying records
	// nothing remembers sending.
	_, err := s.db.ExecContext(ctx, `
INSERT INTO log_stream_checkpoints (
  pod_uid, container_name, namespace, pod_name, last_processed_ns,
  last_read_ns, recent_json, updated_at_ns
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(pod_uid, container_name) DO UPDATE SET
  namespace = excluded.namespace,
  pod_name = excluded.pod_name,
  last_processed_ns = excluded.last_processed_ns,
  last_read_ns = excluded.last_read_ns,
  recent_json = excluded.recent_json,
  updated_at_ns = excluded.updated_at_ns
WHERE excluded.last_processed_ns > log_stream_checkpoints.last_processed_ns
   OR excluded.last_read_ns > log_stream_checkpoints.last_read_ns`,
		key.PodUID,
		key.Container,
		meta.Namespace,
		meta.PodName,
		lastProcessed.UnixNano(),
		lastReadNS,
		recentJSON,
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
