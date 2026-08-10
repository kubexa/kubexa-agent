package stream

import (
	"testing"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

// TestIngestRulesZeroMeansUnset pins the one rule the whole feature rests on:
// a zero field is "no rule pushed", not "a limit of zero". A generated struct
// cannot enforce that, so this test exists to make the contract visible at the
// place someone would change it.
func TestIngestRulesZeroMeansUnset(t *testing.T) {
	snap := &agentv1.ConfigSnapshot{}
	if snap.GetIngestRules() != nil {
		t.Fatalf("an absent ingest_rules must read as nil, got %+v", snap.GetIngestRules())
	}
	rules := &agentv1.IngestRules{MaxLineBytes: 4096}
	if got := rules.GetMaxSampleAgeMs(); got != 0 {
		t.Fatalf("an unset field must read as 0, got %d", got)
	}
	if got := rules.GetMaxLineBytes(); got != 4096 {
		t.Fatalf("max_line_bytes: want 4096, got %d", got)
	}
}

// TestConfigSnapshotIngestRulesRoundTrip proves field 7 survives the wire, so a
// gateway and an agent built from the same proto agree on it.
func TestConfigSnapshotIngestRulesRoundTrip(t *testing.T) {
	in := &agentv1.ConfigSnapshot{
		Watchers:    []*agentv1.WatcherConfig{{Id: "w1"}},
		IngestRules: &agentv1.IngestRules{MaxLineBytes: 1024, MaxSampleAgeMs: 3600000},
	}
	raw, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out agentv1.ConfigSnapshot
	if err := proto.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.GetIngestRules().GetMaxLineBytes() != 1024 {
		t.Fatalf("max_line_bytes lost: %+v", out.GetIngestRules())
	}
	if out.GetIngestRules().GetMaxSampleAgeMs() != 3600000 {
		t.Fatalf("max_sample_age_ms lost: %+v", out.GetIngestRules())
	}
	if len(out.GetWatchers()) != 1 {
		t.Fatalf("watchers lost: %+v", out.GetWatchers())
	}
}

// TestAgentHealthCountersRoundTrip pins the four heartbeat counters. They are
// cumulative since process start, and the gateway advances its own Prometheus
// counters by the difference -- so losing one on the wire would not merely
// lose a sample, it would make the next delta wrong.
func TestAgentHealthCountersRoundTrip(t *testing.T) {
	in := &agentv1.Heartbeat{
		TimestampUnixMs: 1_700_000_000_000,
		Health: &agentv1.AgentHealth{
			QueueDepth:         7,
			Status:             "healthy",
			TruncatedLines:     1,
			DroppedTooOld:      2,
			DroppedFuture:      3,
			DroppedRateLimited: 4,
		},
	}
	raw, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out agentv1.Heartbeat
	if err := proto.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	h := out.GetHealth()
	if h.GetTruncatedLines() != 1 || h.GetDroppedTooOld() != 2 ||
		h.GetDroppedFuture() != 3 || h.GetDroppedRateLimited() != 4 {
		t.Fatalf("counters lost in transit: %+v", h)
	}
}

// TestLogEntryTruncatedRoundTrip: the flag is what tells a reader a 256 KB
// line was cut rather than merely long, and kubexa-consumer turns it into a
// structured-metadata key.
func TestLogEntryTruncatedRoundTrip(t *testing.T) {
	raw, err := proto.Marshal(&agentv1.LogEntry{Message: "x", Truncated: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out agentv1.LogEntry
	if err := proto.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.GetTruncated() {
		t.Fatal("truncated flag lost")
	}
}
