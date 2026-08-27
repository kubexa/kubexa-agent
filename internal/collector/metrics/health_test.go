package metrics

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"
	"time"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

func TestSnapshotSeparatesFailingFromEmpty(t *testing.T) {
	h := newScrapeHealth()
	h.SetTargetCount("cadvisor", 3)

	// One target answered with zero samples. That is a MEASURED zero and the
	// kind is healthy; collapsing it into "failing" would send an operator
	// hunting an outage that is not there.
	h.RecordSuccess("cadvisor", "cadvisor/node-1", 0)
	h.RecordSuccess("cadvisor", "cadvisor/node-2", 42)
	h.RecordFailure("cadvisor", "cadvisor/node-3", errors.New("HTTP 500"))

	snap := byKind(h.Snapshot())
	got := snap["cadvisor"]

	if got.State != StateFailing {
		t.Errorf("State = %q, want %q: one of three targets is failing", got.State, StateFailing)
	}
	if got.TargetsTotal != 3 || got.TargetsFailing != 1 {
		t.Errorf("targets = %d/%d, want 1/3", got.TargetsFailing, got.TargetsTotal)
	}
	if got.SamplesLastScrape != 42 {
		t.Errorf("SamplesLastScrape = %d, want 42", got.SamplesLastScrape)
	}
	if got.LastSuccess.IsZero() {
		t.Error("LastSuccess is zero after a success")
	}
}

func TestASuccessClearsAPreviousFailureForThatKind(t *testing.T) {
	h := newScrapeHealth()
	h.SetTargetCount("cadvisor", 1)
	h.RecordFailure("cadvisor", "cadvisor/node-1", errors.New("HTTP 500"))
	h.RecordSuccess("cadvisor", "cadvisor/node-1", 7)

	got := byKind(h.Snapshot())["cadvisor"]
	if got.State != StateOK {
		t.Errorf("State = %q, want %q", got.State, StateOK)
	}
	if got.TargetsFailing != 0 {
		t.Errorf("TargetsFailing = %d, want 0", got.TargetsFailing)
	}
}

// TestTwoFailingTargetsOfSameKindBothCount pins the thing a kind-keyed failure
// map gets wrong: with one entry per KIND, TargetsFailing can never exceed 1
// no matter how many targets of that kind are actually down, and one target's
// success would wipe out every other target's still-live failure. Keying by
// target name is what lets "3 of 40 failing" ever say 3, and what lets one
// target recovering leave the others' failures standing.
func TestTwoFailingTargetsOfSameKindBothCount(t *testing.T) {
	h := newScrapeHealth()
	h.SetTargetCount("cadvisor", 2)
	h.RecordFailure("cadvisor", "cadvisor/node-1", errors.New("HTTP 500"))
	h.RecordFailure("cadvisor", "cadvisor/node-2", errors.New("HTTP 500"))

	got := byKind(h.Snapshot())["cadvisor"]
	if got.TargetsFailing != 2 {
		t.Fatalf("TargetsFailing = %d, want 2: both targets are down", got.TargetsFailing)
	}

	h.RecordSuccess("cadvisor", "cadvisor/node-1", 10)

	got = byKind(h.Snapshot())["cadvisor"]
	if got.TargetsFailing != 1 {
		t.Fatalf("TargetsFailing = %d, want 1: node-1 recovered, node-2 is still down", got.TargetsFailing)
	}
	if got.State != StateFailing {
		t.Errorf("State = %q, want %q: node-2 is still failing", got.State, StateFailing)
	}
}

// TestClearTargetRemovesOnlyThatEntry pins the fix for the eviction gap: a
// torn-down target's failing entry has no other code path that ever removes
// it, since nothing will scrape it again to call RecordSuccess.
func TestClearTargetRemovesOnlyThatEntry(t *testing.T) {
	h := newScrapeHealth()
	h.SetTargetCount("cadvisor", 2)
	h.RecordFailure("cadvisor", "cadvisor/node-1", errors.New("HTTP 500"))
	h.RecordFailure("cadvisor", "cadvisor/node-2", errors.New("HTTP 500"))

	got := byKind(h.Snapshot())["cadvisor"]
	if got.TargetsFailing != 2 {
		t.Fatalf("TargetsFailing = %d, want 2", got.TargetsFailing)
	}

	h.ClearTarget("cadvisor", "cadvisor/node-1")
	got = byKind(h.Snapshot())["cadvisor"]
	if got.TargetsFailing != 1 {
		t.Fatalf("TargetsFailing = %d, want 1 after clearing node-1", got.TargetsFailing)
	}
	if got.State != StateFailing {
		t.Errorf("State = %q, want %q: node-2 is still failing", got.State, StateFailing)
	}

	h.ClearTarget("cadvisor", "cadvisor/node-2")
	got = byKind(h.Snapshot())["cadvisor"]
	if got.TargetsFailing != 0 {
		t.Fatalf("TargetsFailing = %d, want 0 after clearing node-2", got.TargetsFailing)
	}
	if got.State != StateOK {
		t.Errorf("State = %q, want %q", got.State, StateOK)
	}
}

// TestClearTargetOnUnknownKindOrTargetIsANoOp asserts ClearTarget never
// panics and never fabricates a kindState for a kind or target it has never
// heard of.
func TestClearTargetOnUnknownKindOrTargetIsANoOp(t *testing.T) {
	h := newScrapeHealth()
	h.SetTargetCount("cadvisor", 1)
	h.RecordFailure("cadvisor", "cadvisor/node-1", errors.New("HTTP 500"))

	before := h.Snapshot()

	h.ClearTarget("kube_state_metrics", "kube_state_metrics/pod-1") // unknown kind
	h.ClearTarget("cadvisor", "cadvisor/node-99")                   // unknown target

	after := h.Snapshot()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("Snapshot changed after no-op ClearTarget calls: before=%+v after=%+v", before, after)
	}
}

func TestUnreachableIsItsOwnState(t *testing.T) {
	h := newScrapeHealth()
	h.SetTargetCount("kube_state_metrics", 1)
	// A refused connection or an unresolvable name means the thing is not
	// there. "Failing" means it is there and answering badly. The screen says
	// different words for the two, so the agent must not merge them.
	h.RecordFailure("kube_state_metrics", "kube_state_metrics/pod-1", &net.OpError{Op: "dial", Err: errors.New("connect: connection refused")})

	got := byKind(h.Snapshot())["kube_state_metrics"]
	if got.State != StateUnreachable {
		t.Errorf("State = %q, want %q", got.State, StateUnreachable)
	}
}

func TestMarkNotInstalledOutranksUnreachable(t *testing.T) {
	h := newScrapeHealth()
	h.MarkNotInstalled("kube_state_metrics")

	got := byKind(h.Snapshot())["kube_state_metrics"]
	if got.State != StateNotInstalled {
		t.Errorf("State = %q, want %q", got.State, StateNotInstalled)
	}
	if got.TargetsTotal != 0 {
		t.Errorf("TargetsTotal = %d, want 0", got.TargetsTotal)
	}
}

// MarkInstalled is the only thing that clears a standing MarkNotInstalled:
// a kube-state-metrics target does not scrape at all while notInstalled is
// set, so nothing else would ever call RecordSuccess to clear it once the
// Service reappears.
func TestMarkInstalledReturnsAKindToItsOrdinaryState(t *testing.T) {
	h := newScrapeHealth()
	h.MarkNotInstalled("kube_state_metrics")
	h.MarkInstalled("kube_state_metrics")

	got := byKind(h.Snapshot())["kube_state_metrics"]
	if got.State != StateOK {
		t.Errorf("State = %q, want %q", got.State, StateOK)
	}
}

func TestMarkInstalledOnUnknownKindIsAHarmlessNoOp(t *testing.T) {
	h := newScrapeHealth()
	h.SetTargetCount("cadvisor", 1)
	h.RecordFailure("cadvisor", "cadvisor/node-1", errors.New("HTTP 500"))

	before := byKind(h.Snapshot())["cadvisor"]
	h.MarkInstalled("kube_state_metrics") // never seen before

	after := byKind(h.Snapshot())["cadvisor"]
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("MarkInstalled on an unknown kind changed an unrelated kind's health: before=%+v after=%+v",
			before, after)
	}
	got := byKind(h.Snapshot())["kube_state_metrics"]
	if got.State != StateOK {
		t.Errorf("State = %q, want %q for a kind with no failures and no absence determination",
			got.State, StateOK)
	}
}

// This is the exact collapse the product must not make: a component that was
// reinstalled but is still failing (CrashLoopBackOff, still starting) must
// read as failing, not as absent. Absent tells the operator there is nothing
// to fix.
func TestMarkNotInstalledThenMarkInstalledThenFailureReportsFailingNotNotInstalled(t *testing.T) {
	h := newScrapeHealth()
	h.MarkNotInstalled("kube_state_metrics")

	h.MarkInstalled("kube_state_metrics")
	h.RecordFailure("kube_state_metrics", "kube-state-metrics", errors.New("HTTP 500"))

	got := byKind(h.Snapshot())["kube_state_metrics"]
	if got.State != StateFailing {
		t.Fatalf("State = %q, want %q -- a reinstalled-but-failing component must not read as absent",
			got.State, StateFailing)
	}
}

// MarkInstalled clears only the absence flag. A failing target from a
// different scrape and a recorded success must survive it untouched.
func TestMarkInstalledTouchesNothingButTheAbsenceFlag(t *testing.T) {
	h := newScrapeHealth()
	h.RecordSuccess("kube_state_metrics", "kube-state-metrics", 42)
	h.RecordFailure("kube_state_metrics", "kube-state-metrics-2", errors.New("HTTP 500"))
	before := byKind(h.Snapshot())["kube_state_metrics"]

	h.MarkInstalled("kube_state_metrics")

	after := byKind(h.Snapshot())["kube_state_metrics"]
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("MarkInstalled changed health beyond the absence flag: before=%+v after=%+v", before, after)
	}
}

func TestCardinalityDropsAccumulate(t *testing.T) {
	h := newScrapeHealth()
	h.SetTargetCount("cadvisor", 1)
	h.RecordCardinalityDrop("cadvisor", 120)
	h.RecordCardinalityDrop("cadvisor", 80)

	got := byKind(h.Snapshot())["cadvisor"]
	// Cumulative since process start, like every other counter the heartbeat
	// carries: a dropped heartbeat then loses nothing.
	if got.DroppedCardinality != 200 {
		t.Errorf("DroppedCardinality = %d, want 200", got.DroppedCardinality)
	}
}

func byKind(list []KindHealth) map[string]KindHealth {
	out := make(map[string]KindHealth, len(list))
	for _, k := range list {
		out[k.Kind] = k
	}
	return out
}

func TestSnapshotIsOrderedByKind(t *testing.T) {
	h := newScrapeHealth()
	h.SetTargetCount("kube_state_metrics", 1)
	h.SetTargetCount("cadvisor", 1)
	h.SetTargetCount("custom", 1)

	snap := h.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("len = %d, want 3", len(snap))
	}
	for i := 1; i < len(snap); i++ {
		if snap[i-1].Kind > snap[i].Kind {
			t.Fatalf("Snapshot is unordered: %q before %q", snap[i-1].Kind, snap[i].Kind)
		}
	}
	_ = time.Now
}

func TestSnapshotProtoCarriesEveryState(t *testing.T) {
	h := newScrapeHealth()
	h.SetTargetCount("cadvisor", 4)
	h.RecordSuccess("cadvisor", "cadvisor/node-1", 900)
	h.RecordCardinalityDrop("cadvisor", 15)
	h.MarkNotInstalled("kube_state_metrics")

	got := HealthProto(h)

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	byName := map[string]*agentv1.ScrapeTargetHealth{}
	for _, e := range got {
		byName[e.GetKind()] = e
	}

	ca := byName["cadvisor"]
	if ca.GetState() != string(StateOK) {
		t.Errorf("cadvisor state = %q", ca.GetState())
	}
	if ca.GetTargetsTotal() != 4 || ca.GetTargetsFailing() != 0 {
		t.Errorf("cadvisor targets = %d/%d", ca.GetTargetsFailing(), ca.GetTargetsTotal())
	}
	if ca.GetSamplesLastScrape() != 900 {
		t.Errorf("cadvisor samples = %d", ca.GetSamplesLastScrape())
	}
	if ca.GetDroppedCardinality() != 15 {
		t.Errorf("cadvisor dropped = %d", ca.GetDroppedCardinality())
	}
	if ca.GetLastSuccessUnixMs() == 0 {
		t.Error("cadvisor last_success_unix_ms = 0 after a success")
	}

	ks := byName["kube_state_metrics"]
	if ks.GetState() != string(StateNotInstalled) {
		t.Errorf("kube_state_metrics state = %q, want %q", ks.GetState(), StateNotInstalled)
	}
	// Never succeeded: 0, and the platform must read that as "never", not as
	// "at the epoch".
	if ks.GetLastSuccessUnixMs() != 0 {
		t.Errorf("kube_state_metrics last_success_unix_ms = %d, want 0", ks.GetLastSuccessUnixMs())
	}
}

// TestSamplesLastScrapeSumsEveryTargetOfTheKind pins the fix for a
// last-writer-wins scalar. Forty cAdvisor nodes are one row on the screen, and
// with a kind-wide scalar that row reported whichever node happened to finish
// last -- so a single node whose allowlist matched nothing rendered the whole
// kind as `samples_last_scrape: 0` while the other thirty-nine shipped
// thousands. That is a measured nonzero displayed as a zero, which is exactly
// the collapse this registry exists to prevent.
func TestSamplesLastScrapeSumsEveryTargetOfTheKind(t *testing.T) {
	h := newScrapeHealth()
	h.SetTargetCount(KindCAdvisor, 3)

	h.RecordSuccess(KindCAdvisor, "cadvisor/node-1", 1200)
	h.RecordSuccess(KindCAdvisor, "cadvisor/node-2", 1300)
	// The last writer measured a real zero. Under last-writer-wins the kind
	// would report 0 and the 2500 samples the other two published would be
	// invisible.
	h.RecordSuccess(KindCAdvisor, "cadvisor/node-3", 0)

	got := byKind(h.Snapshot())[KindCAdvisor]
	if got.SamplesLastScrape != 2500 {
		t.Fatalf("SamplesLastScrape = %d, want 2500 (1200+1300+0)", got.SamplesLastScrape)
	}

	// A target's next scrape REPLACES its own contribution rather than adding
	// to it -- this is a gauge of the last scrape, not a counter.
	h.RecordSuccess(KindCAdvisor, "cadvisor/node-1", 10)
	if got := byKind(h.Snapshot())[KindCAdvisor]; got.SamplesLastScrape != 1310 {
		t.Fatalf("SamplesLastScrape = %d, want 1310 after node-1 re-scraped", got.SamplesLastScrape)
	}
}

// A torn-down target must stop contributing to the sum, for the same reason it
// must stop contributing to TargetsFailing: nothing will ever scrape it again,
// so its last figure would otherwise be added into every future snapshot.
func TestClearTargetDropsThatTargetsSampleContribution(t *testing.T) {
	h := newScrapeHealth()
	h.SetTargetCount(KindCAdvisor, 2)
	h.RecordSuccess(KindCAdvisor, "cadvisor/node-1", 500)
	h.RecordSuccess(KindCAdvisor, "cadvisor/node-2", 700)

	h.ClearTarget(KindCAdvisor, "cadvisor/node-2")
	h.SetTargetCount(KindCAdvisor, 1)

	got := byKind(h.Snapshot())[KindCAdvisor]
	if got.SamplesLastScrape != 500 {
		t.Fatalf("SamplesLastScrape = %d, want 500: node-2 no longer exists", got.SamplesLastScrape)
	}
}

// A kind marked not-installed carries no figures from the installation that is
// gone. "not_installed" next to a sample count and a success timestamp
// describes no state that ever existed, and an operator reading the row cannot
// tell whether the numbers are current.
func TestMarkNotInstalledLeavesNoStaleFigures(t *testing.T) {
	h := newScrapeHealth()
	h.SetTargetCount(KindKubeState, 1)
	h.RecordSuccess(KindKubeState, "kube-state-metrics", 4200)

	if got := byKind(h.Snapshot())[KindKubeState]; got.SamplesLastScrape != 4200 {
		t.Fatalf("SamplesLastScrape = %d, want 4200 before the component is removed", got.SamplesLastScrape)
	}

	h.MarkNotInstalled(KindKubeState)

	got := byKind(h.Snapshot())[KindKubeState]
	if got.State != StateNotInstalled {
		t.Fatalf("State = %q, want %q", got.State, StateNotInstalled)
	}
	if got.SamplesLastScrape != 0 {
		t.Errorf("SamplesLastScrape = %d, want 0: nothing is being scraped", got.SamplesLastScrape)
	}
	if !got.LastSuccess.IsZero() {
		t.Errorf("LastSuccess = %v, want zero: the success belonged to an installation that is gone",
			got.LastSuccess)
	}
	// The cumulative drop counter is deliberately NOT reset: it is cumulative
	// since process start, like every other AgentHealth counter, and a
	// decrease there means "the agent restarted" to the gateway.
}

// A discovery failure is reported as failing, never as absence, and never as a
// scrape's classification. The kubelets may be perfectly reachable -- it is the
// agent's view of them that is broken -- so StateFailing is the honest answer
// regardless of what kind of error the API server returned.
func TestRecordDiscoveryFailureReportsFailingNotAbsence(t *testing.T) {
	h := newScrapeHealth()
	h.SetTargetCount(KindCAdvisor, 0)

	h.RecordDiscoveryFailure(KindCAdvisor)

	got := byKind(h.Snapshot())[KindCAdvisor]
	if got.State != StateFailing {
		t.Fatalf("State = %q, want %q", got.State, StateFailing)
	}
	// And it is NOT a failing target. targets_failing counts targets; a
	// discovery failure is a condition of the whole kind and has no target to
	// be attributed to.
	if got.TargetsFailing != 0 {
		t.Errorf("TargetsFailing = %d, want 0: a discovery failure is not a failing target",
			got.TargetsFailing)
	}

	// Nothing else would ever clear it -- no scrape is attributed to the
	// discovery flag -- so a recovered listing must clear it explicitly.
	h.ClearDiscoveryFailure(KindCAdvisor)
	if got := byKind(h.Snapshot())[KindCAdvisor]; got.State != StateOK {
		t.Fatalf("State = %q, want %q after discovery recovered", got.State, StateOK)
	}
}

// TestADiscoveryFailureIsNeverCountedAsAFailingTarget pins both false renders
// the pseudo-target produced, on the wire, where the consuming repo reads them.
func TestADiscoveryFailureIsNeverCountedAsAFailingTarget(t *testing.T) {
	t.Run("RBAC refused at start reports no failing target", func(t *testing.T) {
		// Start seeded the kind at 0 and the very first node LIST was refused.
		h := newScrapeHealth()
		h.SetTargetCount(KindCAdvisor, 0)
		h.RecordDiscoveryFailure(KindCAdvisor)

		got := protoByKind(HealthProto(h))[KindCAdvisor]
		if got.GetState() != string(StateFailing) {
			t.Fatalf("state = %q, want %q", got.GetState(), StateFailing)
		}
		// "1 of 0 failing" is the inversion the Kind field was introduced to
		// eliminate. Reintroducing it through the discovery entry would say
		// more targets are failing than exist.
		if got.GetTargetsFailing() > got.GetTargetsTotal() {
			t.Fatalf("targets = %d/%d -- more failing than exist",
				got.GetTargetsFailing(), got.GetTargetsTotal())
		}
		if got.GetTargetsFailing() != 0 {
			t.Fatalf("targets_failing = %d, want 0: no target is known to exist, let alone to fail",
				got.GetTargetsFailing())
		}
	})

	t.Run("a discovery failure after a healthy listing accuses no node", func(t *testing.T) {
		// Forty nodes discovered and every one of them scraping fine, then the
		// next refresh's LIST fails. Reporting "1 of 40 failing" is a specific
		// claim about one node, and it is false.
		h := newScrapeHealth()
		h.SetTargetCount(KindCAdvisor, 40)
		for i := 0; i < 40; i++ {
			h.RecordSuccess(KindCAdvisor, fmt.Sprintf("cadvisor/node-%d", i), 100)
		}
		h.RecordDiscoveryFailure(KindCAdvisor)

		got := protoByKind(HealthProto(h))[KindCAdvisor]
		if got.GetState() != string(StateFailing) {
			t.Fatalf("state = %q, want %q: the agent cannot refresh its target list",
				got.GetState(), StateFailing)
		}
		if got.GetTargetsFailing() != 0 {
			t.Fatalf("targets_failing = %d, want 0: all forty nodes are scraping fine",
				got.GetTargetsFailing())
		}
		if got.GetTargetsTotal() != 40 {
			t.Errorf("targets_total = %d, want the 40 last successfully discovered",
				got.GetTargetsTotal())
		}
	})

	t.Run("a real target failure still counts alongside it", func(t *testing.T) {
		h := newScrapeHealth()
		h.SetTargetCount(KindCAdvisor, 40)
		h.RecordFailure(KindCAdvisor, "cadvisor/node-3", errors.New("HTTP 500"))
		h.RecordDiscoveryFailure(KindCAdvisor)

		got := protoByKind(HealthProto(h))[KindCAdvisor]
		if got.GetTargetsFailing() != 1 {
			t.Fatalf("targets_failing = %d, want exactly the 1 real failing node",
				got.GetTargetsFailing())
		}

		// And clearing the discovery failure must leave that node's own
		// failure standing: the two are independent facts, and the recovered
		// listing says nothing about whether node-3 answered.
		h.ClearDiscoveryFailure(KindCAdvisor)
		got = protoByKind(HealthProto(h))[KindCAdvisor]
		if got.GetTargetsFailing() != 1 {
			t.Fatalf("targets_failing = %d after clearing discovery, want the real failure kept",
				got.GetTargetsFailing())
		}
		if got.GetState() != string(StateFailing) {
			t.Errorf("state = %q, want %q: node-3 is still down", got.GetState(), StateFailing)
		}
	})
}

// TestADiscoveryFailureOutranksNotInstalled pins the precedence.
//
// not_installed is only ever recorded from a definite IsNotFound. A later
// probe error means the agent no longer knows whether the Service is there, so
// continuing to assert absence claims knowledge that was just lost -- and it
// sends the operator to install something that may already be running, rather
// than to the probe that is actually broken.
func TestADiscoveryFailureOutranksNotInstalled(t *testing.T) {
	h := newScrapeHealth()
	h.SetTargetCount(KindKubeState, 0)

	// The probe answered: the Service is genuinely absent.
	h.MarkNotInstalled(KindKubeState)
	if got := byKind(h.Snapshot())[KindKubeState]; got.State != StateNotInstalled {
		t.Fatalf("State = %q, want %q on a definite NotFound", got.State, StateNotInstalled)
	}

	// The next probe could not ask at all. The agent has lost the knowledge
	// that justified the previous verdict.
	h.RecordDiscoveryFailure(KindKubeState)
	got := byKind(h.Snapshot())[KindKubeState]
	if got.State != StateFailing {
		t.Fatalf("State = %q, want %q: the agent no longer knows whether it is installed",
			got.State, StateFailing)
	}
	// And the two must never be published together -- a not_installed state
	// beside a nonzero failing count contradicts itself.
	if got.TargetsFailing != 0 {
		t.Errorf("TargetsFailing = %d, want 0", got.TargetsFailing)
	}

	// A probe that asks successfully and finds it absent again restores the
	// definite verdict.
	h.MarkNotInstalled(KindKubeState)
	if got := byKind(h.Snapshot())[KindKubeState]; got.State != StateNotInstalled {
		t.Fatalf("State = %q, want %q once the probe answers again", got.State, StateNotInstalled)
	}
}

func protoByKind(list []*agentv1.ScrapeTargetHealth) map[string]*agentv1.ScrapeTargetHealth {
	out := make(map[string]*agentv1.ScrapeTargetHealth, len(list))
	for _, k := range list {
		out[k.GetKind()] = k
	}
	return out
}
