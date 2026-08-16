package queue

import "time"

// diskRef locates one spilled item in the WAL without holding its payload.
//
// This type is the whole point of the disk tier: the queue keeps a slice of
// these instead of a slice of Item, and reads the payload back only when the
// item is actually dequeued. Holding Items here made max_disk_bytes an
// effective heap ceiling — 512 MiB of it, inside a 512Mi container.
type diskRef struct {
	// ID is the item's identity, needed to match acks and nacks without a read.
	ID string
	// Segment and Offset locate the item's record in the WAL.
	Segment int
	Offset  int64
	// EnqueuedAt orders recovered references and is needed before any read.
	EnqueuedAt time.Time
	// Attempts is tracked in memory only. Nack no longer rewrites the record,
	// so after a restart this resets to the value written when the item first
	// spilled. That costs retry accounting, never an item.
	Attempts int
}
