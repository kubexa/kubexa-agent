package logs

import (
	"io"
	"strings"
	"testing"
)

// TestLineReaderSurvivesAnOverlongLine is the regression test for a real,
// pre-existing bug. bufio.Scanner returns ErrTooLong for a line above its
// buffer and the scan is over -- for a followed pod log that meant the stream
// tore down, reconnected with SinceTime, read the same line and died again.
// That pod's logs stopped permanently and the only signal was a warning.
func TestLineReaderSurvivesAnOverlongLine(t *testing.T) {
	long := strings.Repeat("x", 100)
	src := strings.NewReader("short\n" + long + "\nafter\n")
	lr := newLineReader(src, 10)

	line, truncated, err := lr.next()
	if err != nil || string(line) != "short" || truncated {
		t.Fatalf("first line: %q truncated=%v err=%v", line, truncated, err)
	}

	line, truncated, err = lr.next()
	if err != nil {
		t.Fatalf("an overlong line must not error: %v", err)
	}
	if !truncated {
		t.Error("an overlong line must report truncated")
	}
	if len(line) != 10 {
		t.Errorf("want 10 bytes, got %d (%q)", len(line), line)
	}

	line, truncated, err = lr.next()
	if err != nil || string(line) != "after" || truncated {
		t.Fatalf("the reader must continue past the long line: %q truncated=%v err=%v", line, truncated, err)
	}

	if _, _, err = lr.next(); err != io.EOF {
		t.Fatalf("want io.EOF, got %v", err)
	}
}

// TestLineReaderSurvivesALineLongerThanItsOwnBuffer. The 100-byte case above
// still fits one ReadSlice; this one does not, so it exercises the
// ErrBufferFull loop -- the path where the bytes past the cap are consumed and
// discarded across several reads rather than in one.
func TestLineReaderSurvivesALineLongerThanItsOwnBuffer(t *testing.T) {
	long := strings.Repeat("y", readBufferSize*3)
	lr := newLineReader(strings.NewReader(long+"\nafter\n"), 16)

	line, truncated, err := lr.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if !truncated || len(line) != 16 {
		t.Fatalf("want 16 truncated bytes, got %d truncated=%v", len(line), truncated)
	}
	line, _, err = lr.next()
	if err != nil || string(line) != "after" {
		t.Fatalf("the reader must resynchronise on the next newline: %q err=%v", line, err)
	}
}

// TestLineReaderCutsOnARuneBoundary: cutting mid-rune writes invalid UTF-8
// into the line Loki stores, and every reader downstream renders a replacement
// character forever after.
func TestLineReaderCutsOnARuneBoundary(t *testing.T) {
	// "ü" is two bytes. A cap of 5 lands in the middle of the third one.
	src := strings.NewReader("üüü\n")
	lr := newLineReader(src, 5)

	line, truncated, err := lr.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if !truncated {
		t.Error("want truncated")
	}
	if string(line) != "üü" {
		t.Errorf("want %q, got %q (% x)", "üü", line, line)
	}
}

// TestLineReaderKeepsValidReplacementCharacters. Trimming a partial rune must
// not eat a U+FFFD the application itself wrote: that is three legal bytes,
// not a broken tail, and DecodeLastRune reports both as RuneError.
func TestLineReaderKeepsValidReplacementCharacters(t *testing.T) {
	lr := newLineReader(strings.NewReader("a�\n"), 100)
	line, truncated, err := lr.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if truncated {
		t.Error("nothing was over the cap")
	}
	if string(line) != "a�" {
		t.Errorf("want %q, got %q (% x)", "a�", line, line)
	}
}

// TestLineReaderHandlesAFinalLineWithoutNewline: a container's last line often
// arrives unterminated, and dropping it loses the message that says why the
// process exited.
func TestLineReaderHandlesAFinalLineWithoutNewline(t *testing.T) {
	lr := newLineReader(strings.NewReader("tail"), 100)
	line, _, err := lr.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if string(line) != "tail" {
		t.Errorf("want %q, got %q", "tail", line)
	}
	if _, _, err := lr.next(); err != io.EOF {
		t.Fatalf("want io.EOF, got %v", err)
	}
}

// TestLineReaderPreservesEmptyLines. An empty line is payload -- blank lines
// separate stack frames in some formats -- and the joiner's own rules decide
// what to do with it, not the reader.
func TestLineReaderPreservesEmptyLines(t *testing.T) {
	lr := newLineReader(strings.NewReader("a\n\nb\n"), 100)
	for _, want := range []string{"a", "", "b"} {
		line, _, err := lr.next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if string(line) != want {
			t.Errorf("want %q, got %q", want, line)
		}
	}
}

// TestLineReaderWithNoCapNeverTruncates: a non-positive max disables the rule,
// matching "0 = no rule pushed" everywhere else in this feature.
func TestLineReaderWithNoCapNeverTruncates(t *testing.T) {
	long := strings.Repeat("z", readBufferSize*2)
	lr := newLineReader(strings.NewReader(long+"\n"), 0)

	line, truncated, err := lr.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if truncated {
		t.Error("a non-positive cap must not truncate")
	}
	if len(line) != len(long) {
		t.Errorf("want %d bytes, got %d", len(long), len(line))
	}
}
