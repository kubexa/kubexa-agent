package capability

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

type captureWriter struct {
	mu   sync.Mutex
	msgs []*agentv1.AgentMessage
}

func (w *captureWriter) Write(_ context.Context, msg *agentv1.AgentMessage) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.msgs = append(w.msgs, msg)
	return nil
}

func (w *captureWriter) all() []*agentv1.AgentMessage {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*agentv1.AgentMessage{}, w.msgs...)
}

type failingWriter struct{ calls int }

func (w *failingWriter) Write(context.Context, *agentv1.AgentMessage) error {
	w.calls++
	return errors.New("queue full")
}

func caps(n int) []Capability {
	out := make([]Capability, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Capability{
			GVR:     GVR{Group: "apps", Version: "v1", Resource: "r" + string(rune('a'+i))},
			CanList: true,
		})
	}
	return out
}

// buildCatalog is the pure part of the reporter: everything about turning a
// probe result into a wire message, with no scheduling involved.
func TestBuildCatalogCarriesMillisecondsAndFailedGroups(t *testing.T) {
	at := time.Date(2026, 7, 30, 10, 7, 56, 0, time.UTC)

	cat := buildCatalog(caps(2), []string{"metrics.k8s.io/v1beta1"}, "sha256:x", at)

	if cat.GetCollectedAt() != at.UnixMilli() {
		t.Fatalf("collectedAt = %d, want %d (unix MILLIseconds, per catalog.proto)",
			cat.GetCollectedAt(), at.UnixMilli())
	}
	if cat.GetFingerprint() != "sha256:x" {
		t.Fatalf("fingerprint = %q, want sha256:x", cat.GetFingerprint())
	}
	if len(cat.GetEntries()) != 2 {
		t.Fatalf("entries = %d, want 2", len(cat.GetEntries()))
	}
	if len(cat.GetFailedGroups()) != 1 {
		t.Fatalf("failedGroups = %v, want one entry", cat.GetFailedGroups())
	}
}

// The expensive sweep must not run on every discovery tick. This is the whole
// reason the fingerprint exists.
func TestNeedsSweepSkipsWhenFingerprintUnchanged(t *testing.T) {
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	s := sweepState{lastFingerprint: "sha256:a", lastSweep: now, ran: true}

	if s.needsSweep("sha256:a", now.Add(5*time.Minute), time.Hour) {
		t.Fatal("needsSweep = true for an unchanged fingerprint inside the safety interval")
	}
}

func TestNeedsSweepOnFingerprintChange(t *testing.T) {
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	s := sweepState{lastFingerprint: "sha256:a", lastSweep: now}

	if !s.needsSweep("sha256:b", now.Add(time.Minute), time.Hour) {
		t.Fatal("needsSweep = false after the GVR set changed")
	}
}

// RBAC changes are invisible to discovery: widening a ClusterRole does not
// change which GVRs exist. Without the time-based sweep a new grant would
// never surface.
func TestNeedsSweepAfterSafetyInterval(t *testing.T) {
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	s := sweepState{lastFingerprint: "sha256:a", lastSweep: now}

	if !s.needsSweep("sha256:a", now.Add(61*time.Minute), time.Hour) {
		t.Fatal("needsSweep = false past the safety interval with unchanged discovery")
	}
}

// A never-run reporter always sweeps, whatever the fingerprint.
func TestNeedsSweepOnFirstRun(t *testing.T) {
	var s sweepState
	if !s.needsSweep("sha256:a", time.Now(), time.Hour) {
		t.Fatal("needsSweep = false on the first run")
	}
}

// A catalog that never reached the queue is not a completed sweep. Recording
// one would mean no catalog at all until the hourly safety sweep -- an hour of
// an empty type list because the queue was briefly full at startup. refresh
// only advances r.state after r.publish succeeds, so drive that path directly
// through the extracted publish step rather than re-simulating it.
func TestFailedPublishLeavesSweepStateUnset(t *testing.T) {
	w := &failingWriter{}
	r := &Reporter{writer: w}

	err := r.publish(context.Background(), caps(1), nil, "sha256:a", time.Now())
	if err == nil {
		t.Fatal("expected publish to fail")
	}
	if w.calls != 1 {
		t.Fatalf("writer calls = %d, want 1", w.calls)
	}
	if r.state.ran {
		t.Fatal("state.ran = true after a failed publish; the next tick would not retry")
	}
	if !r.state.needsSweep("sha256:a", time.Now(), time.Hour) {
		t.Fatal("needsSweep = false after a failed publish; the catalog would be stuck until the safety sweep")
	}
}
