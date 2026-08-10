// Package ingestrules holds the ingest rules the gateway pushes, and the
// agent's own defaults for whatever it does not push.
//
// Before these rules existed the agent hardcoded a 256 KB line cap that
// matched Loki's max_line_size default by coincidence rather than by
// agreement. Anything the gateway does not state is still a guess -- these
// defaults are that guess, kept explicit and in one place.
package ingestrules

import (
	"sync/atomic"
	"time"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// DefaultMaxLineBytes is Loki's own max_line_size default and the value this
// agent hardcoded before the gateway started stating one.
const DefaultMaxLineBytes = 256 * 1024

// Rules is the resolved rule set. A zero duration or a zero rate means the
// rule is OFF, not that the limit is zero -- except MaxLineBytes, which is
// never zero because a zero cap would truncate every line to nothing.
type Rules struct {
	MaxLineBytes   int
	MaxSampleAge   time.Duration
	MaxFutureSkew  time.Duration
	PerStreamRate  int64
	PerStreamBurst int64
}

// Defaults is what the agent uses when the gateway pushes nothing. Only the
// line cap has one: age filtering and per-stream shaping were not happening
// before this feature and must not start happening on their own.
func Defaults() Rules {
	return Rules{MaxLineBytes: DefaultMaxLineBytes}
}

// FromProto resolves a pushed message against the defaults. Every zero field
// means "no rule pushed" -- identical to a nil message, which is what a
// gateway too old to send one produces.
func FromProto(p *agentv1.IngestRules) Rules {
	out := Defaults()
	if p == nil {
		return out
	}
	if v := p.GetMaxLineBytes(); v > 0 {
		out.MaxLineBytes = int(v)
	}
	if v := p.GetMaxSampleAgeMs(); v > 0 {
		out.MaxSampleAge = time.Duration(v) * time.Millisecond
	}
	if v := p.GetMaxFutureSkewMs(); v > 0 {
		out.MaxFutureSkew = time.Duration(v) * time.Millisecond
	}
	if v := p.GetPerStreamRateBytes(); v > 0 {
		out.PerStreamRate = v
	}
	if v := p.GetPerStreamBurstBytes(); v > 0 {
		out.PerStreamBurst = v
	}
	return out
}

// Store holds the current rules for concurrent readers: the log collector on
// the collection path and the stream manager on the send path.
type Store struct{ v atomic.Pointer[Rules] }

// NewStore returns a store answering the defaults until Set is called.
func NewStore() *Store { return &Store{} }

// Set replaces the rules.
func (s *Store) Set(r Rules) { s.v.Store(&r) }

// Get returns the current rules, or the defaults when none were ever set. A
// nil receiver also answers the defaults, so a component wired without a store
// behaves like one that was never pushed to.
func (s *Store) Get() Rules {
	if s == nil {
		return Defaults()
	}
	if p := s.v.Load(); p != nil {
		return *p
	}
	return Defaults()
}
