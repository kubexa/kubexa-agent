package logs

import (
	"bufio"
	"io"
	"unicode/utf8"
)

// readBufferSize is the reader's own buffer, not the line cap. Lines longer
// than this are assembled across several reads.
const readBufferSize = 64 * 1024

// lineReader reads newline-delimited lines and caps each at max bytes.
//
// It replaces bufio.Scanner, which fails permanently on a long line: Scan()
// returns false with bufio.ErrTooLong and the scan is over. For a followed pod
// log that meant the stream tore down, reconnected with SinceTime, read the
// same line and died again -- that pod's logs stopped forever and the only
// signal was one warning. Truncating is strictly better than losing the pod.
type lineReader struct {
	r   *bufio.Reader
	max int
}

// newLineReader wraps r. A non-positive max disables truncation, matching
// "0 = no rule pushed" everywhere else in this feature.
func newLineReader(r io.Reader, max int) *lineReader {
	return &lineReader{r: bufio.NewReaderSize(r, readBufferSize), max: max}
}

// next returns the next line without its terminator. truncated reports that
// bytes were discarded.
func (lr *lineReader) next() ([]byte, bool, error) {
	var (
		out       []byte
		truncated bool
	)
	for {
		chunk, err := lr.r.ReadSlice('\n')
		if n := len(chunk); n > 0 && chunk[n-1] == '\n' {
			chunk = chunk[:n-1]
		}
		if len(chunk) > 0 {
			switch {
			case lr.max <= 0:
				out = append(out, chunk...)
			case len(out)+len(chunk) > lr.max:
				// Keep what fits and consume the rest: the bytes past the cap
				// are read and dropped, never returned, so the next call
				// resynchronises on the following newline.
				if room := lr.max - len(out); room > 0 {
					out = append(out, chunk[:room]...)
				}
				truncated = true
			default:
				out = append(out, chunk...)
			}
		}
		switch err {
		case nil:
			return trimToRune(out, truncated), truncated, nil
		case bufio.ErrBufferFull:
			// More of the same line follows; keep reading.
			continue
		case io.EOF:
			if len(out) > 0 {
				// A final line with no terminator: a container's last line is
				// routinely unterminated and it is usually the one that says
				// why the process exited.
				return trimToRune(out, truncated), truncated, nil
			}
			return nil, false, io.EOF
		default:
			return nil, false, err
		}
	}
}

// trimToRune drops a partial UTF-8 sequence from the tail of a line that was
// CUT. Cutting mid-rune writes invalid bytes into the line Loki stores, and
// every reader downstream renders a replacement character from then on.
//
// It runs only when truncated is true. A line the reader did not cut is passed
// through byte for byte: an application is free to write invalid UTF-8, and
// silently repairing its output would be this collector inventing content.
func trimToRune(b []byte, truncated bool) []byte {
	if !truncated {
		return b
	}
	for len(b) > 0 {
		r, size := utf8.DecodeLastRune(b)
		// (RuneError, 1) is a broken tail. A legitimately-encoded U+FFFD
		// decodes as (RuneError, 3), so the size check is what tells a cut
		// sequence from a replacement character the application wrote itself.
		if r != utf8.RuneError || size > 1 {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
}
