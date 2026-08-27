package metrics

import (
	"errors"
	"net"
	"sort"
	"sync"
	"syscall"
	"time"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
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
	LastSuccess time.Time
	// SamplesLastScrape is the SUM over this kind's targets of what each one
	// published on its own last scrape. Last-writer-wins would make forty
	// cAdvisor nodes report whichever node finished last, so one node whose
	// allowlist matches nothing would render the whole kind as zero while the
	// other thirty-nine shipped thousands -- a measured nonzero displayed as
	// a zero, which is the collapse this registry exists to prevent.
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
	failing     map[string]TargetState
	lastSuccess time.Time
	// samplesLastScrape is keyed by TARGET name for the same reason failing
	// is: a kind-wide scalar is last-writer-wins across every target of the
	// kind. Evicted by ClearTarget, symmetrically with failing.
	samplesLastScrape  map[string]int64
	droppedCardinality int64
	notInstalled       bool
	// discoveryFailed is a condition of the whole KIND, not of any target, so
	// it is a flag rather than an entry in failing. As an entry it was counted
	// by TargetsFailing, which produced two false renders: `1 of 0 failing`
	// for an RBAC-refused start -- the "more failing than exist" inversion the
	// Kind field was introduced to eliminate -- and `1 of 40 failing` after a
	// good listing, a specific claim about one node while all forty scrape
	// fine. A discovery failure has no target to be attributed to.
	discoveryFailed bool
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
		k = &kindState{
			failing:           make(map[string]TargetState),
			samplesLastScrape: make(map[string]int64),
		}
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
	k.samplesLastScrape[target] = int64(samples)
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

// RecordDiscoveryFailure records that the agent could not work out which
// targets of this kind exist -- a failed node LIST, a failed presence probe.
//
// It is deliberately not RecordFailure, on two counts. The classification
// there describes how a SCRAPE failed, and a kind whose discovery is broken
// has not scraped anything: StateFailing is the honest answer, because the
// kubelets may be perfectly reachable and it is the agent's view of them that
// is broken. And it is not recorded against a target at all -- see
// kindState.discoveryFailed for why a pseudo-target inverted the very count
// this registry exists to keep straight.
//
// Reporting the kind as absent instead is what this whole registry exists to
// prevent: an agent whose ClusterRole is too narrow would emit no entry at
// all, and the screen would read a totally broken integration as "this agent
// does not report scrape health".
func (h *ScrapeHealth) RecordDiscoveryFailure(kind string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.kind(kind).discoveryFailed = true
}

// ClearDiscoveryFailure clears a standing discovery failure for kind. A
// discovery that starts working again must stop being reported: nothing else
// would ever clear this flag, because no scrape is ever attributed to it.
func (h *ScrapeHealth) ClearDiscoveryFailure(kind string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	k, ok := h.kinds[kind]
	if !ok {
		return
	}
	k.discoveryFailed = false
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
	// The figures belong to the installation that is now gone. Left standing
	// they read on the screen as "not installed" next to a sample count and a
	// success timestamp, which describes no state that ever existed.
	k.samplesLastScrape = make(map[string]int64)
	k.lastSuccess = time.Time{}
	// The probe ANSWERED -- absence is only ever recorded from a definite
	// IsNotFound -- so any standing "could not ask" is superseded by it.
	k.discoveryFailed = false
}

// MarkInstalled clears kind's absence determination and touches nothing else
// -- not failing, not lastSuccess, not samplesLastScrape, not the
// cardinality counter, not total. RecordSuccess already clears
// notInstalled for every kind that scrapes on its own schedule, but
// kube-state-metrics does not: while notInstalled is set its scrape loop
// never runs at all, so nothing would ever call RecordSuccess to clear the
// flag once the Service reappears. The evidence that clears it is the
// presence probe finding the Service again, not a scrape's outcome -- a
// freshly-installed, still-failing target must read as failing, not as
// not-installed.
//
// An unknown kind is handled the same way MarkNotInstalled handles one: it
// is harmless, not a panic.
func (h *ScrapeHealth) MarkInstalled(kind string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.kind(kind).notInstalled = false
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
	delete(k.samplesLastScrape, target)
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
		samples := int64(0)
		for _, n := range k.samplesLastScrape {
			samples += n
		}
		entry := KindHealth{
			Kind:               name,
			TargetsTotal:       k.total,
			TargetsFailing:     len(k.failing),
			LastSuccess:        k.lastSuccess,
			SamplesLastScrape:  samples,
			DroppedCardinality: k.droppedCardinality,
		}
		switch {
		// A discovery failure OUTRANKS not_installed. Absence is recorded only
		// from a definite IsNotFound; a later probe error means the agent no
		// longer knows whether the component is there, so continuing to assert
		// "not installed" claims knowledge that was just lost. "Failing to
		// determine" is both the honest state and the actionable one -- it
		// points the operator at the probe rather than at an install.
		case k.discoveryFailed:
			entry.State = StateFailing
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

// HealthProto renders a scrape health snapshot for the heartbeat.
//
// A zero LastSuccess becomes 0, not the Unix epoch: "never succeeded" and
// "succeeded in 1970" are different claims and only one of them is true.
func HealthProto(h *ScrapeHealth) []*agentv1.ScrapeTargetHealth {
	snapshot := h.Snapshot()
	out := make([]*agentv1.ScrapeTargetHealth, 0, len(snapshot))
	for _, k := range snapshot {
		var lastSuccess int64
		if !k.LastSuccess.IsZero() {
			lastSuccess = k.LastSuccess.UnixMilli()
		}
		out = append(out, &agentv1.ScrapeTargetHealth{
			Kind:               k.Kind,
			State:              string(k.State),
			TargetsTotal:       int32(k.TargetsTotal),
			TargetsFailing:     int32(k.TargetsFailing),
			LastSuccessUnixMs:  lastSuccess,
			SamplesLastScrape:  k.SamplesLastScrape,
			DroppedCardinality: k.DroppedCardinality,
		})
	}
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
