package ingestrules

import "sync/atomic"

// Counters are the process-lifetime totals the heartbeat reports.
//
// CUMULATIVE, never deltas. A dropped heartbeat loses data permanently under a
// delta model and loses nothing under a cumulative one; the gateway advances
// its own Prometheus counters by the difference and reads a decrease as an
// agent restart.
//
// One instance is shared by the log collector (truncation, rate limiting) and
// the stream manager (age drops), so the heartbeat reports one number per
// reason rather than two half-counts. Every method is nil-safe: a component
// wired without counters records nothing instead of panicking.
type Counters struct {
	truncated   atomic.Int64
	tooOld      atomic.Int64
	future      atomic.Int64
	rateLimited atomic.Int64
}

// NewCounters returns a zeroed set.
func NewCounters() *Counters { return &Counters{} }

// IncTruncated records one line cut at max_line_bytes.
func (c *Counters) IncTruncated() {
	if c != nil {
		c.truncated.Add(1)
	}
}

// IncTooOld records one record dropped for being older than max_sample_age.
func (c *Counters) IncTooOld() {
	if c != nil {
		c.tooOld.Add(1)
	}
}

// IncFuture records one record dropped for sitting further ahead than
// max_future_skew.
func (c *Counters) IncFuture() {
	if c != nil {
		c.future.Add(1)
	}
}

// IncRateLimited records one line dropped by the per-stream limiter.
func (c *Counters) IncRateLimited() {
	if c != nil {
		c.rateLimited.Add(1)
	}
}

// Snapshot reads all four at once. The four loads are not atomic as a group,
// which is fine: the gateway treats these as monotonic totals and a reader
// that catches one mid-increment simply sees it on the next heartbeat.
func (c *Counters) Snapshot() (truncated, tooOld, future, rateLimited int64) {
	if c == nil {
		return 0, 0, 0, 0
	}
	return c.truncated.Load(), c.tooOld.Load(), c.future.Load(), c.rateLimited.Load()
}
