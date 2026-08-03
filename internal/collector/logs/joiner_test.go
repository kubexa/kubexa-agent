package logs

import (
	"testing"
	"time"
)

func plain(msg string, ts time.Time) ParsedLine {
	return ParsedLine{Message: msg, Raw: []byte(msg), Timestamp: ts}
}

// A stack trace is one event. Emitted line by line it is 30 records with other
// containers' lines interleaved between them, and no query can put it back.
func TestJoinerFoldsContinuationLines(t *testing.T) {
	now := time.Now()
	j := newJoiner(64*1024, time.Second)

	if out := j.Add(plain("panic: boom", now), now); len(out) != 0 {
		t.Fatalf("emitted %d lines while the record was still open", len(out))
	}
	if out := j.Add(plain("\tat main.go:12", now), now); len(out) != 0 {
		t.Fatalf("continuation emitted on its own")
	}
	out := j.Add(plain("next record", now), now)
	if len(out) != 1 || string(out[0].Raw) != "panic: boom\n\tat main.go:12" {
		t.Fatalf("joined = %q", out)
	}
}

// JSON lines are complete by construction. Folding the next line into one would
// corrupt a record that `| json` must be able to parse.
func TestJoinerNeverJoinsJSONLines(t *testing.T) {
	now := time.Now()
	j := newJoiner(64*1024, time.Second)
	j.Add(plain(`{"msg":"a"}`, now), now)
	out := j.Add(plain("  indented but the previous line was JSON", now), now)
	if len(out) != 1 || string(out[0].Raw) != `{"msg":"a"}` {
		t.Fatalf("JSON record was extended: %q", out)
	}
}

func TestJoinerStopsAtTheSizeCap(t *testing.T) {
	now := time.Now()
	j := newJoiner(20, time.Second)
	j.Add(plain("panic: boom", now), now)
	out := j.Add(plain("\tat a-very-long-frame", now), now)
	if len(out) != 1 {
		t.Fatalf("the capped record was not flushed: %d", len(out))
	}
}

// A pending record must not be held forever by a stream that went quiet.
func TestJoinerFlushesAfterTheHoldWindow(t *testing.T) {
	now := time.Now()
	j := newJoiner(64*1024, time.Second)
	j.Add(plain("panic: boom", now), now)
	out := j.Add(plain("\tat main.go:12", now), now.Add(2*time.Second))
	if len(out) != 1 || string(out[0].Raw) != "panic: boom" {
		t.Fatalf("held record was not flushed on the hold window: %q", out)
	}
}

func TestJoinerFlushReturnsThePending(t *testing.T) {
	now := time.Now()
	j := newJoiner(64*1024, time.Second)
	j.Add(plain("panic: boom", now), now)
	if out := j.Flush(); len(out) != 1 {
		t.Fatalf("Flush returned %d records", len(out))
	}
	if out := j.Flush(); len(out) != 0 {
		t.Fatalf("Flush returned the same record twice")
	}
}
