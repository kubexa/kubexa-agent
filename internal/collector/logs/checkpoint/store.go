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

// Store persists and loads stream read positions.
type Store interface {
	// Load returns the last processed log timestamp for key, or false when absent.
	Load(ctx context.Context, key Key) (time.Time, bool, error)
	// Save records the last processed log timestamp for key.
	Save(ctx context.Context, key Key, lastProcessed time.Time, meta Metadata) error
	// Flush ensures pending writes are durable.
	Flush(ctx context.Context) error
	// Close releases store resources.
	Close() error
}
