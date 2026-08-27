package metrics

import (
	"errors"
	"net"
	"reflect"
	"testing"
	"time"
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
