package run

import (
	"bufio"
	"errors"
	"io"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/stream"
)

// streamEvents reads a vendor's line-delimited output, normalises each line and
// appends it to the transcript, while keeping the agent's own messages as the
// run's result.
//
// It returns whether output was truncated. It never fails the run on a line it
// cannot parse: unparsable lines become vendor.raw events, because a vendor
// adding a field must not take the bridge down.
func (r *Run) streamEvents(rc io.ReadCloser, spec Spec, out *capped) bool {
	defer func() { _ = rc.Close() }()

	max := spec.MaxEventBytes
	if max <= 0 {
		max = 1 << 20
	}
	lr := newLineReader(rc, max)

	truncated := false
	sawMessage := false
	transcriptFailed := false
	// record appends to the transcript and reports whether the run must stop
	// because the transcript hit its cap. The cap is a stop condition, not a
	// silent truncation. A write failure is reported once and ends transcript
	// writing only: parsing goes on, because the stream still carries the
	// agent's messages and the terminal event that closes a stream-json
	// child's stdin.
	record := func(e stream.Event) (stop bool) {
		if transcriptFailed {
			return false
		}
		overLimit, err := spec.Transcript.Append(e)
		if err != nil {
			// The watch TUI, the progress digest and the audit trail all read
			// the transcript, so the operator must see that it is incomplete.
			r.setDiagnostic("transcript write failed: " + err.Error())
			transcriptFailed = true
			return false
		}
		if overLimit {
			r.mu.Lock()
			r.transcriptOverflow = true
			r.mu.Unlock()
			r.Cancel()
			return true
		}
		return false
	}
	for {
		line, tooLong, err := lr.next()
		if tooLong {
			// A line longer than the limit is recorded and skipped, and the
			// stream keeps being read. Returning here would close the pipe
			// while the vendor is still writing: codex dies of EPIPE with
			// exit status 101 on its next event.
			truncated = true
			if record(stream.Event{
				Time: time.Now().UTC(),
				Kind: stream.KindOversized,
				Text: "a vendor event exceeded the configured line limit and was dropped",
			}) {
				return truncated
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				r.setDiagnostic("vendor stream read failed: " + err.Error())
			}
			break
		}
		if tooLong || len(line) == 0 {
			continue
		}
		e, ok := spec.Parser.Parse(line)
		if !ok {
			continue
		}
		if e.SessionID != "" {
			r.setVendorSession(e.SessionID)
		}
		// The agent's own messages are the answer; everything else is activity
		// the operator watches but the caller does not need.
		//
		// The result record usually REPEATS the final assistant message, so it
		// is used only when the agent said nothing else — otherwise the caller
		// receives the whole answer twice.
		switch e.Kind {
		case stream.KindMessage:
			if e.Text != "" {
				sawMessage = true
				if over := out.append(e.Text + "\n"); over {
					truncated = true
				}
			}
		case stream.KindVendorError:
			// Not fatal — the vendor decides that by its exit status, and most
			// of these are warnings about the operator's own config. Recorded
			// so the caller sees what the stream reported (FR-14).
			r.noteVendorError(e.Text)
		case stream.KindRunFailed:
			// The vendor's own verdict on the turn. It arrives just before a
			// non-zero exit whose stderr is empty, so without this the caller
			// is told the status and nothing else.
			r.noteVendorFailure(e.Text)
		case stream.KindRunFinished:
			if e.Text != "" && !sawMessage {
				if over := out.append(e.Text + "\n"); over {
					truncated = true
				}
			}
		}
		// A stream-json child does not exit when its turn completes; it blocks
		// on stdin EOF. Closing here is what turns a completed turn into a
		// finished process, and it is also the moment steering stops being
		// possible.
		if e.Kind == stream.KindRunFinished && spec.CloseStdinAfterResult {
			r.closeSteer()
		}
		if record(e) {
			return truncated
		}
	}
	return truncated
}

// lineReader yields newline-terminated lines of at most max bytes. A longer
// line is consumed to its end and reported as tooLong, so one oversized event
// costs that event and nothing after it.
type lineReader struct {
	br  *bufio.Reader
	max int
	buf []byte
}

func newLineReader(r io.Reader, max int) *lineReader {
	return &lineReader{br: bufio.NewReaderSize(r, 64<<10), max: max}
}

// next returns the following line without its terminator. The slice is valid
// until the next call. err is non-nil at end of input or on a read error.
func (l *lineReader) next() (line []byte, tooLong bool, err error) {
	l.buf = l.buf[:0]
	for {
		chunk, isPrefix, rerr := l.br.ReadLine()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) && len(l.buf) > 0 && !tooLong {
				// An unterminated last line whose length is a multiple of the
				// buffer size arrives as an empty chunk with EOF; the data is
				// already in buf. The next call reports the EOF.
				return l.buf, false, nil
			}
			return l.buf, tooLong, rerr
		}
		if !tooLong {
			if len(l.buf)+len(chunk) > l.max {
				tooLong = true
				l.buf = l.buf[:0]
			} else {
				l.buf = append(l.buf, chunk...)
			}
		}
		if !isPrefix {
			return l.buf, tooLong, nil
		}
	}
}
