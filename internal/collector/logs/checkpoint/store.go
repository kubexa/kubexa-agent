// Package checkpoint persists per-stream log read positions for restart resume.
package checkpoint

import (
	"context"
	"time"
)

// Key uniquely identifies a pod container log stream.
type Key struct {
	PodUID    string
	Container string
}

// Metadata holds optional pod identity fields stored with a checkpoint.
type Metadata struct {
	Namespace string
	PodName   string
}

// Delivered identifies one record the stream has already sent.
//
// It is a pair rather than a timestamp because the de-duplication it serves
// may never drop a line the stream has not sent, and two distinct payloads
// can share one read time.
type Delivered struct {
	ReadTime time.Time `json:"t"`
	Digest   string    `json:"d"`
}

// Position is where a stream resumes AND what it must not send twice.
//
// The two halves are inseparable: a resume deliberately rewinds past the last
// line it handled, so a position without the record of what that rewind will
// replay is a position that re-delivers. Keeping them in one value is what
// stops a future caller from persisting one and forgetting the other.
type Position struct {
	// LastProcessed is the newest record timestamp delivered -- the record's
	// own clock, which a JSON "time" field in the payload may set.
	LastProcessed time.Time
	// LastRead is the newest Kubernetes log API read time delivered. This is
	// the resume point, because it is the frame the log API filters in.
	LastRead time.Time
	// Recent holds the records delivered inside the window the resume can
	// replay, newest last.
	Recent []Delivered
}

// Store persists and loads stream read positions.
type Store interface {
	// Load returns the stream's position, or false when absent.
	Load(ctx context.Context, key Key) (Position, bool, error)
	// Save records the stream's position for key.
	Save(ctx context.Context, key Key, pos Position, meta Metadata) error
	// Flush ensures pending writes are durable.
	Flush(ctx context.Context) error
	// Close releases store resources.
	Close() error
}
