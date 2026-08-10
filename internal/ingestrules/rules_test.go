package ingestrules_test

import (
	"testing"
	"time"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"

	"github.com/kubexa/kubexa-agent/internal/ingestrules"
)

// TestFromProtoZeroKeepsTheDefault is the contract the whole feature rests on.
// A gateway that pushes nothing and a gateway too old to push at all must
// produce identical behaviour, so there is one fallback path to reason about.
func TestFromProtoZeroKeepsTheDefault(t *testing.T) {
	got := ingestrules.FromProto(&agentv1.IngestRules{})
	want := ingestrules.Defaults()
	if got != want {
		t.Fatalf("an all-zero message must equal the defaults\n got: %+v\nwant: %+v", got, want)
	}
}

func TestFromProtoNilKeepsTheDefault(t *testing.T) {
	if got, want := ingestrules.FromProto(nil), ingestrules.Defaults(); got != want {
		t.Fatalf("nil must equal the defaults\n got: %+v\nwant: %+v", got, want)
	}
}

// TestDefaultsLeaveTheNewRulesOff. Age filtering and per-stream shaping were
// not happening before this feature; a default that turned them on would
// change behaviour for every agent whose gateway has not been upgraded.
func TestDefaultsLeaveTheNewRulesOff(t *testing.T) {
	d := ingestrules.Defaults()
	if d.MaxLineBytes != ingestrules.DefaultMaxLineBytes {
		t.Errorf("MaxLineBytes must keep its historical value, got %d", d.MaxLineBytes)
	}
	if d.MaxSampleAge != 0 || d.MaxFutureSkew != 0 {
		t.Errorf("age filtering must be off by default, got %+v", d)
	}
	if d.PerStreamRate != 0 || d.PerStreamBurst != 0 {
		t.Errorf("per-stream shaping must be off by default, got %+v", d)
	}
}

// TestFromProtoConvertsMilliseconds pins the unit boundary: the wire carries
// milliseconds, the agent works in time.Duration.
func TestFromProtoConvertsMilliseconds(t *testing.T) {
	got := ingestrules.FromProto(&agentv1.IngestRules{
		MaxLineBytes:        4096,
		MaxSampleAgeMs:      (168 * time.Hour).Milliseconds(),
		MaxFutureSkewMs:     (10 * time.Minute).Milliseconds(),
		PerStreamRateBytes:  3145728,
		PerStreamBurstBytes: 10485760,
	})
	if got.MaxLineBytes != 4096 {
		t.Errorf("MaxLineBytes: got %d", got.MaxLineBytes)
	}
	if got.MaxSampleAge != 168*time.Hour {
		t.Errorf("MaxSampleAge: got %s", got.MaxSampleAge)
	}
	if got.MaxFutureSkew != 10*time.Minute {
		t.Errorf("MaxFutureSkew: got %s", got.MaxFutureSkew)
	}
	if got.PerStreamRate != 3145728 || got.PerStreamBurst != 10485760 {
		t.Errorf("per-stream: got %d/%d", got.PerStreamRate, got.PerStreamBurst)
	}
}

// TestStoreDefaultsBeforeAnySet proves a reader never sees a zero rule set --
// a MaxLineBytes of 0 would truncate every line to nothing.
func TestStoreDefaultsBeforeAnySet(t *testing.T) {
	if got, want := ingestrules.NewStore().Get(), ingestrules.Defaults(); got != want {
		t.Fatalf("an unset store must answer the defaults, got %+v", got)
	}
}

// TestNilStoreAnswersDefaults: a component wired without a store must behave
// like one that was never pushed to, not panic.
func TestNilStoreAnswersDefaults(t *testing.T) {
	var s *ingestrules.Store
	if got, want := s.Get(), ingestrules.Defaults(); got != want {
		t.Fatalf("a nil store must answer the defaults, got %+v", got)
	}
}
