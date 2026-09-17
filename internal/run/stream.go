package run

import (
	"bufio"
	"io"

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

	sc := bufio.NewScanner(rc)
	max := spec.MaxEventBytes
	if max <= 0 {
		max = 256 << 10
	}
	sc.Buffer(make([]byte, 64<<10), max)

	truncated := false
	sawMessage := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
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
		overLimit, err := spec.Transcript.Append(e)
		if err != nil {
			// A transcript that cannot be written is a fault the operator must
			// see: the watch TUI, the progress digest and the audit trail all
			// read it. Swallowing the error left a silently empty transcript.
			r.setDiagnostic("transcript write failed: " + err.Error())
			return truncated
		}
		if overLimit {
			// The transcript cap is a stop condition, not a silent truncation.
			r.mu.Lock()
			r.transcriptOverflow = true
			r.mu.Unlock()
			r.Cancel()
			return truncated
		}
	}
	if err := sc.Err(); err != nil {
		// A line longer than the buffer is recorded rather than lost.
		_, _ = spec.Transcript.Append(stream.Event{
			Kind: stream.KindOversized,
			Text: "a vendor event exceeded the configured line limit and was dropped",
		})
		truncated = true
	}
	return truncated
}
