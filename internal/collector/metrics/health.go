package metrics

import (
	"errors"
	"net"
	"sort"
	"sync"
	"syscall"
	"time"
)

// TargetState is what an operator is told about one kind of scrape target.
//
// The four values are deliberately not collapsible. "Not installed" and
// "unreachable" and "failing" send an operator to three different places, and
// an empty graph with no state at all sends them nowhere.
type TargetState string

const (
	// StateOK: every target of this kind answered its last scrape. A scrape
	// that returned zero samples is OK -- that is a measured zero.
	StateOK TargetState = "ok"
	// StateFailing: the target exists and answered badly (a 5xx, a parse
	// error, a timeout).
	StateFailing TargetState = "failing"
	// StateUnreachable: the connection was refused or the name did not
	// resolve. Nothing is listening.
	StateUnreachable TargetState = "unreachable"
	// StateNotInstalled: the component this kind scrapes is absent from the
	// cluster by the agent's own determination, not by a failed scrape.
	StateNotInstalled TargetState = "not_installed"
)

// KindHealth is the health of one kind of scrape target, aggregated over every
// target of that kind.
type KindHealth struct {
	Kind           string
	State          TargetState
	TargetsTotal   int
	TargetsFailing int
	// LastSuccess is zero when this kind has never had a successful scrape,
	// which is a different statement from "its last scrape failed".
	LastSuccess       time.Time
	SamplesLastScrape int64
	// DroppedCardinality is CUMULATIVE since process start, matching every
	// other counter AgentHealth carries.
	DroppedCardinality int64
}

type kindState struct {
	total int
	// failing is keyed by TARGET name, not by kind. One entry per kind would
	// cap TargetsFailing at 1 regardless of how many targets of that kind are
	// actually down, and a single target's success would have to wipe the
	// whole map -- erasing every other target's still-live failure along with
	// it. Keyed by target, a success only ever removes its own entry.
	failing            map[string]TargetState
	lastSuccess        time.Time
	samplesLastScrape  int64
	droppedCardinality int64
	notInstalled       bool
}

// ScrapeHealth is the collector's live view of its own scraping.
type ScrapeHealth struct {
	mu    sync.Mutex
	kinds map[string]*kindState
	now   func() time.Time
}

func newScrapeHealth() *ScrapeHealth {
	return &ScrapeHealth{kinds: make(map[string]*kindState), now: time.Now}
}

func (h *ScrapeHealth) kind(name string) *kindState {
	k, ok := h.kinds[name]
	if !ok {
		k = &kindState{failing: make(map[string]TargetState)}
		h.kinds[name] = k
	}
	return k
}

// SetTargetCount records how many targets of this kind are configured or
// generated. It is the denominator "3 of 40 failing" needs; without it a
// failure count is unreadable.
func (h *ScrapeHealth) SetTargetCount(kind string, total int) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.kind(kind).total = total
}

// RecordSuccess clears target's failure, if any, and stamps the kind's
// success.
//
// samples may legitimately be 0 -- an endpoint that is up and currently
// exposes nothing. That is not recorded as a failure and not reported as one.
// Only target's own entry is removed from the failing set: a kind with other
// targets still down must keep reporting them failing after this one target
// recovers.
func (h *ScrapeHealth) RecordSuccess(kind, target string, samples int) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	k := h.kind(kind)
	k.notInstalled = false
	k.lastSuccess = h.now()
	k.samplesLastScrape = int64(samples)
	delete(k.failing, target)
}

// RecordFailure classifies one failed scrape and records it against target.
func (h *ScrapeHealth) RecordFailure(kind, target string, err error) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	k := h.kind(kind)
	k.failing[target] = classifyScrapeError(err)
}

// RecordCardinalityDrop adds to this kind's cumulative dropped-sample total.
func (h *ScrapeHealth) RecordCardinalityDrop(kind string, samples int64) {
	if h == nil || samples <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.kind(kind).droppedCardinality += samples
}

// MarkNotInstalled records that the component this kind scrapes is absent.
// It outranks every other state: the agent determined absence directly, so
// reporting the resulting connection failures on top of it would say the same
// thing twice in worse words.
func (h *ScrapeHealth) MarkNotInstalled(kind string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	k := h.kind(kind)
	k.notInstalled = true
	k.total = 0
	k.failing = make(map[string]TargetState)
}

// ClearTarget removes target's failure entry for kind, if any, and touches
// nothing else -- not lastSuccess, not samplesLastScrape, not the
// cardinality counter, not the target count, not notInstalled.
//
// A torn-down target never scrapes again, so nothing else would ever call
// RecordSuccess/RecordFailure for it to self-heal its own failing entry. A
// caller that stops scraping a target must call this or the entry outlives
// the target: TargetsFailing keeps counting a node that no longer exists,
// forever, and repeated churn accumulates one stale entry per node that
// happened to be failing at the moment it was removed.
//
// A kind ClearTarget has never heard of, or a target not in that kind's
// failing set, is a no-op -- it must not create a kindState for either.
func (h *ScrapeHealth) ClearTarget(kind, target string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	k, ok := h.kinds[kind]
	if !ok {
		return
	}
	delete(k.failing, target)
}

// Snapshot returns one entry per kind, ordered by kind so two consecutive
// heartbeats from the same agent do not differ only in map iteration order.
func (h *ScrapeHealth) Snapshot() []KindHealth {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]KindHealth, 0, len(h.kinds))
	for name, k := range h.kinds {
		entry := KindHealth{
			Kind:               name,
			TargetsTotal:       k.total,
			TargetsFailing:     len(k.failing),
			LastSuccess:        k.lastSuccess,
			SamplesLastScrape:  k.samplesLastScrape,
			DroppedCardinality: k.droppedCardinality,
		}
		switch {
		case k.notInstalled:
			entry.State = StateNotInstalled
		case len(k.failing) == 0:
			entry.State = StateOK
		default:
			entry.State = StateFailing
			for _, s := range k.failing {
				if s == StateUnreachable {
					entry.State = StateUnreachable
					break
				}
			}
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// classifyScrapeError separates "nothing is listening" from "it answered
// badly". A refused connection and an unresolvable name are the first; a
// timeout is the second, because something accepted the connection.
//
// The registry never stores err's text -- only this classification. A scrape
// error can carry the target URL, and a target URL can carry a token in its
// query string; keeping only a TargetState keeps that secret off every
// heartbeat, log line and audit row this registry ever feeds.
func classifyScrapeError(err error) TargetState {
	if err == nil {
		return StateFailing
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) {
		return StateUnreachable
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return StateUnreachable
	}
	// net.OpError wrapping a plain error is what a fake in a test produces and
	// what some platforms produce for a refused dial; match on the text only
	// after the typed checks above have had their chance.
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return StateUnreachable
	}
	return StateFailing
}
