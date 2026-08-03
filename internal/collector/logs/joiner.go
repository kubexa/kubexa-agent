package logs

import (
	"strings"
	"time"
)

// joiner folds continuation lines into the record they belong to. One per
// container stream: lines from two containers interleave in time, and joining
// across them would attach one program's stack frames to another's panic.
//
// Deliberately conservative. A wrongly joined record is worse than a split one:
// the split is visible and searchable, the join silently reattributes text.
type joiner struct {
	maxBytes int
	hold     time.Duration

	pending  *ParsedLine
	openedAt time.Time
}

func newJoiner(maxBytes int, hold time.Duration) *joiner {
	return &joiner{maxBytes: maxBytes, hold: hold}
}

// Add feeds one parsed line and returns the records that are now complete.
//
// A record completes when a line that starts a new one arrives, when the joined
// size would pass maxBytes, or when the pending record has been held longer
// than the hold window. The last record of a quiet stream therefore waits for
// the next line or for Flush — the stream reader calls Flush at EOF.
func (j *joiner) Add(line ParsedLine, now time.Time) []ParsedLine {
	if j.pending != nil && now.Sub(j.openedAt) > j.hold {
		out := j.take()
		j.open(line, now)
		return out
	}

	if j.pending == nil || !j.canExtend(line) {
		out := j.take()
		j.open(line, now)
		return out
	}

	joined := string(j.pending.Raw) + "\n" + string(line.Raw)
	if len(joined) > j.maxBytes {
		out := j.take()
		j.open(line, now)
		return out
	}
	j.pending.Raw = []byte(joined)
	return nil
}

// Flush returns any pending record. Idempotent.
func (j *joiner) Flush() []ParsedLine { return j.take() }

func (j *joiner) open(line ParsedLine, now time.Time) {
	copied := line
	j.pending = &copied
	j.openedAt = now
}

func (j *joiner) take() []ParsedLine {
	if j.pending == nil {
		return nil
	}
	out := []ParsedLine{*j.pending}
	j.pending = nil
	return out
}

// canExtend reports whether `line` continues the pending record. A JSON record
// is never extended: it is complete by construction, and `| json` has to be
// able to parse what reaches Loki.
func (j *joiner) canExtend(line ParsedLine) bool {
	if strings.HasPrefix(string(j.pending.Raw), "{") {
		return false
	}
	return isContinuation(string(line.Raw))
}

// isContinuation matches the shapes runtimes actually emit for a wrapped
// record: indented frames, Java's "at"/"Caused by", and Go's goroutine header.
func isContinuation(line string) bool {
	if line == "" {
		return false
	}
	if line[0] == ' ' || line[0] == '\t' {
		return true
	}
	for _, prefix := range []string{"Caused by:", "... ", "goroutine "} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}
