package exec

import (
	"bytes"
	"testing"

	agentv1 "github.com/kubexa/kubexa-agent/proto/gen/go/agent/v1"
)

func TestRingReplaysAfterSeq(t *testing.T) {
	r := newRing(1 << 20)
	for i := 0; i < 5; i++ {
		r.push(agentv1.ExecChannel_EXEC_CHANNEL_STDOUT, []byte{byte('a' + i)})
	}
	frames, gap := r.since(2)
	if gap || len(frames) != 3 || frames[0].Seq != 3 || !bytes.Equal(frames[2].Chunk, []byte("e")) {
		t.Fatalf("since(2) = %v gap=%v", frames, gap)
	}
	if frames, gap := r.since(5); gap || len(frames) != 0 {
		t.Fatalf("since(last) must be empty, got %v gap=%v", frames, gap)
	}
	if r.last() != 5 || newRing(8).last() != 0 {
		t.Fatalf("last() = %d, want 5 (and 0 on an empty ring)", r.last())
	}
}

func TestRingEvictsOldestAndReportsGap(t *testing.T) {
	r := newRing(10) // bytes
	for i := 0; i < 6; i++ {
		r.push(agentv1.ExecChannel_EXEC_CHANNEL_STDOUT, []byte("xxx")) // 3 bytes each
	}
	// 6 frames * 3 bytes = 18 > 10: the oldest three are gone (9 bytes kept).
	frames, gap := r.since(0)
	if !gap || len(frames) != 3 || frames[0].Seq != 4 {
		t.Fatalf("expected gap with frames 4..6, got gap=%v %v", gap, frames)
	}
	if frames, gap := r.since(3); gap || len(frames) != 3 {
		t.Fatalf("since(3) must be a clean replay of 4..6, got gap=%v %v", gap, frames)
	}
}

func TestRingCopiesChunks(t *testing.T) {
	r := newRing(100)
	buf := []byte("abc")
	r.push(agentv1.ExecChannel_EXEC_CHANNEL_STDOUT, buf)
	buf[0] = 'z'
	frames, _ := r.since(0)
	if string(frames[0].Chunk) != "abc" {
		t.Fatal("ring must own its bytes; the caller reuses its read buffer")
	}
}
