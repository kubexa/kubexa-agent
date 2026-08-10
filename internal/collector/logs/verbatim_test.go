package logs

import (
	"strings"
	"testing"
	"time"
)

// TestParseLineThenJoinerFoldsAJavaStackTraceIntoOneRecord drives lines
// shaped exactly like what the agent's actual log source hands the scanner —
// the Kubernetes pods/log API with Timestamps: true, which prefixes each
// line with "<RFC3339Nano> " and nothing else — through ParseLine and then
// the joiner, the same path stream.go uses. This is the case the design
// opens with: a JVM exception plus its "Caused by:" cause must ship to Loki
// as one event, not one record per frame.
//
// It fails on the trimming parser because ParseLine strips the leading tab
// off every frame before the joiner ever sees it, so isContinuation's
// leading-whitespace rule never fires and each frame becomes its own record.
func TestParseLineThenJoinerFoldsAJavaStackTraceIntoOneRecord(t *testing.T) {
	apiLines := []string{
		`2026-08-03T10:00:00.000000000Z Exception in thread "main" java.lang.RuntimeException: boom`,
		"2026-08-03T10:00:00.001000000Z \tat com.foo.Bar.method(Bar.java:42)",
		"2026-08-03T10:00:00.002000000Z \tat com.foo.Baz.method(Baz.java:10)",
		`2026-08-03T10:00:00.003000000Z Caused by: java.lang.NullPointerException`,
		"2026-08-03T10:00:00.004000000Z \tat com.foo.Qux.method(Qux.java:5)",
		"2026-08-03T10:00:00.005000000Z \t... 3 more",
	}
	nextRecordLine := `2026-08-03T10:00:00.006000000Z next record, unrelated`

	wantPayloads := []string{
		`Exception in thread "main" java.lang.RuntimeException: boom`,
		"\tat com.foo.Bar.method(Bar.java:42)",
		"\tat com.foo.Baz.method(Baz.java:10)",
		`Caused by: java.lang.NullPointerException`,
		"\tat com.foo.Qux.method(Qux.java:5)",
		"\t... 3 more",
	}
	wantJoined := strings.Join(wantPayloads, "\n")

	now := time.Now()
	j := newJoiner(64*1024, time.Second)

	for _, line := range apiLines {
		parsed := ParseLine(line, now)
		if out := j.Add(parsed, now); len(out) != 0 {
			t.Fatalf("trace record emitted early: %+v", out)
		}
	}

	out := j.Add(ParseLine(nextRecordLine, now), now)
	if len(out) != 1 {
		t.Fatalf("got %d records for the trace, want 1: %+v", len(out), out)
	}
	if got := string(out[0].Raw); got != wantJoined {
		t.Fatalf("joined trace =\n%q\nwant\n%q", got, wantJoined)
	}

	rest := j.Flush()
	if len(rest) != 1 || string(rest[0].Raw) != "next record, unrelated" {
		t.Fatalf("trailing record = %+v", rest)
	}
}

// TestParseLinePreservesLeadingAndInternalWhitespace locks in the "verbatim"
// contract directly at the parser: the payload ParseLine hands back must be
// the application's line as written, not a whitespace-normalized copy of it.
func TestParseLinePreservesLeadingAndInternalWhitespace(t *testing.T) {
	now := time.Now()

	t.Run("leading whitespace with no timestamp prefix", func(t *testing.T) {
		got := ParseLine("\tat com.foo.Bar.method(Bar.java:42)", now)
		want := "\tat com.foo.Bar.method(Bar.java:42)"
		if string(got.Raw) != want {
			t.Fatalf("raw = %q, want %q", got.Raw, want)
		}
	})

	t.Run("leading whitespace behind the API timestamp prefix", func(t *testing.T) {
		got := ParseLine("2026-08-03T10:00:00Z \tat com.foo.Bar.method(Bar.java:42)", now)
		want := "\tat com.foo.Bar.method(Bar.java:42)"
		if string(got.Raw) != want {
			t.Fatalf("raw = %q, want %q", got.Raw, want)
		}
	})

	t.Run("internal whitespace runs are not collapsed", func(t *testing.T) {
		got := ParseLine("col1    col2  col3", now)
		want := "col1    col2  col3"
		if string(got.Raw) != want {
			t.Fatalf("raw = %q, want %q", got.Raw, want)
		}
	})

	t.Run("trailing CR from the transport is still stripped", func(t *testing.T) {
		got := ParseLine("hello world\r", now)
		want := "hello world"
		if string(got.Raw) != want {
			t.Fatalf("raw = %q, want %q", got.Raw, want)
		}
	})

	t.Run("app-indented JSON still parses as JSON", func(t *testing.T) {
		got := ParseLine(`  {"level":"info","msg":"started"}`, now)
		if got.Message != "started" {
			t.Fatalf("message = %q, want started", got.Message)
		}
		want := `  {"level":"info","msg":"started"}`
		if string(got.Raw) != want {
			t.Fatalf("raw = %q, want %q", got.Raw, want)
		}
	})
}
