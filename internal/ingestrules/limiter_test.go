package ingestrules_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/kubexa/kubexa-agent/internal/ingestrules"
)

func limitRules() ingestrules.Rules {
	return ingestrules.Rules{PerStreamRate: 1000, PerStreamBurst: 2000}
}

// TestLimiterAdmitsTheBurstThenThrottles: the burst is the bucket's capacity,
// the rate is its refill.
func TestLimiterAdmitsTheBurstThenThrottles(t *testing.T) {
	l := ingestrules.NewLimiter()
	at := time.Unix(0, 0)

	if !l.Allow("s", 2000, limitRules(), at) {
		t.Fatal("a full burst must be admitted")
	}
	if l.Allow("s", 1, limitRules(), at) {
		t.Fatal("the bucket is empty; one more byte must be refused")
	}
}

// TestLimiterRefillsOverTime pins the rate. One second at 1000 B/s buys 1000
// bytes, no more.
func TestLimiterRefillsOverTime(t *testing.T) {
	l := ingestrules.NewLimiter()
	at := time.Unix(0, 0)
	l.Allow("s", 2000, limitRules(), at)

	if !l.Allow("s", 1000, limitRules(), at.Add(time.Second)) {
		t.Fatal("one second must refill 1000 bytes")
	}
	if l.Allow("s", 1, limitRules(), at.Add(time.Second)) {
		t.Fatal("the refill must not exceed the rate")
	}
}

// TestLimiterRefillIsCappedAtTheBurst: an idle stream must not bank credit.
// Without the cap, a pod silent for an hour could then send an hour's worth of
// bytes in one instant -- which is exactly the burst Loki would reject.
func TestLimiterRefillIsCappedAtTheBurst(t *testing.T) {
	l := ingestrules.NewLimiter()
	at := time.Unix(0, 0)
	l.Allow("s", 2000, limitRules(), at)

	later := at.Add(time.Hour)
	if !l.Allow("s", 2000, limitRules(), later) {
		t.Fatal("a full burst must be available after a long idle period")
	}
	if l.Allow("s", 1, limitRules(), later) {
		t.Fatal("an idle stream must not bank more than one burst")
	}
}

// TestLimiterIsPerStream: two streams must not share a budget, or one noisy
// pod throttles a quiet one.
func TestLimiterIsPerStream(t *testing.T) {
	l := ingestrules.NewLimiter()
	at := time.Unix(0, 0)
	l.Allow("a", 2000, limitRules(), at)
	if !l.Allow("b", 2000, limitRules(), at) {
		t.Fatal("a second stream must have its own bucket")
	}
}

// TestLimiterWithNoRuleAdmitsEverything: an unresolved namespace yields zero
// per-stream limits, and zero means "no limit", never "a limit of zero".
func TestLimiterWithNoRuleAdmitsEverything(t *testing.T) {
	l := ingestrules.NewLimiter()
	for i := 0; i < 100; i++ {
		if !l.Allow("s", 1_000_000, ingestrules.Rules{}, time.Unix(0, 0)) {
			t.Fatal("no rule must mean no limit")
		}
	}
	if l.Tracked() != 0 {
		t.Errorf("no rule must allocate no bucket, got %d", l.Tracked())
	}
}

// TestLimiterAtTheEntryCapDropsNothing. Pods churn and the key includes the pod
// name, so the map must be bounded -- but the limiter must never become the
// reason for the data loss it was added to prevent.
func TestLimiterAtTheEntryCapDropsNothing(t *testing.T) {
	l := ingestrules.NewLimiter()
	at := time.Unix(0, 0)
	for i := 0; i < ingestrules.MaxLimiterEntries+50; i++ {
		if !l.Allow("stream-"+strconv.Itoa(i), 1, limitRules(), at) {
			t.Fatalf("stream %d was refused; past the cap the limiter must admit", i)
		}
	}
	if l.Tracked() > ingestrules.MaxLimiterEntries {
		t.Errorf("the map must stay bounded, got %d entries", l.Tracked())
	}
}

// TestLimiterEvictsIdleBuckets: without eviction the map grows for the life of
// the process, one entry per pod that ever ran.
func TestLimiterEvictsIdleBuckets(t *testing.T) {
	l := ingestrules.NewLimiter()
	at := time.Unix(0, 0)
	l.Allow("s", 2000, limitRules(), at)

	l.Evict(at.Add(time.Hour))
	if l.Tracked() != 0 {
		t.Fatalf("an idle bucket must be evicted, still tracking %d", l.Tracked())
	}

	// The bucket is gone, so a fresh one starts full and admits a whole burst.
	if !l.Allow("s", 2000, limitRules(), at.Add(time.Hour)) {
		t.Fatal("an evicted bucket must be recreated full")
	}
}

// TestEvictKeepsActiveBuckets is the other half: eviction must not hand a busy
// stream a fresh full bucket every sweep, which would make the limit
// unenforceable at any sweep interval.
func TestEvictKeepsActiveBuckets(t *testing.T) {
	l := ingestrules.NewLimiter()
	at := time.Unix(0, 0)
	l.Allow("s", 2000, limitRules(), at)

	l.Evict(at.Add(-time.Second))
	if l.Tracked() != 1 {
		t.Fatalf("a recently-used bucket must survive, tracking %d", l.Tracked())
	}
	if l.Allow("s", 2000, limitRules(), at) {
		t.Fatal("the surviving bucket must still be empty")
	}
}

// TestStreamKeyMatchesTheConsumersStreamIdentity is a contract note in
// executable form. The consumer builds a Loki stream from
// namespace/pod/container/level/workload/workload_kind (plus tenant_id and
// cluster_id, constant for one agent); if these buckets key on anything else
// they meter something that is not a stream.
func TestStreamKeyMatchesTheConsumersStreamIdentity(t *testing.T) {
	a := ingestrules.StreamKey("ns", "pod", "c", "info", "w", "Deployment")
	if a == ingestrules.StreamKey("ns", "pod", "c", "error", "w", "Deployment") {
		t.Error("level must be part of the key: it is a Loki stream label")
	}
	if a == ingestrules.StreamKey("ns", "pod", "c", "info", "w", "StatefulSet") {
		t.Error("workload_kind must be part of the key")
	}
	// A separator that could appear in a label value would let two different
	// identities collide into one bucket.
	if ingestrules.StreamKey("a", "b:c", "", "", "", "") == ingestrules.StreamKey("a:b", "c", "", "", "", "") {
		t.Error("the key separator must not be a character label values can contain")
	}
}
