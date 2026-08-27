package metrics

import (
	"errors"
	"net"
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
