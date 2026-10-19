package run

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/oxcom/agents-mcp-bridge/internal/stream"
)

type trackedPipe struct {
	r        io.Reader
	sawEOF   bool
	eofFirst bool
	closed   bool
}

func (p *trackedPipe) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if errors.Is(err, io.EOF) {
		p.sawEOF = true
	}
	return n, err
}

func (p *trackedPipe) Close() error {
	p.eofFirst = p.sawEOF
	p.closed = true
	return nil
}

func streamSpec(t *testing.T, max int) (Spec, *stream.Transcript) {
	t.Helper()
	tr, err := stream.CreateTranscript(t.TempDir(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return Spec{MaxEventBytes: max, Parser: stream.CodexParser{}, Transcript: tr}, tr
}

func TestTheOversizedEventCarriesATime(t *testing.T) {
	spec, tr := streamSpec(t, 100)
	in := &trackedPipe{r: strings.NewReader(strings.Repeat("x", 500) + "\n")}
	r := &Run{}
	r.streamEvents(in, spec, &capped{limit: 1 << 10})
	_ = tr.Close()

	events, err := stream.ReadTranscript(tr.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != stream.KindOversized {
		t.Fatalf("events = %+v, want one oversized event", events)
	}
	if events[0].Time.IsZero() {
		t.Fatal("oversized event has the zero Time")
	}
}

type failingReader struct {
	data io.Reader
	err  error
}

func (f *failingReader) Read(b []byte) (int, error) {
	n, err := f.data.Read(b)
	if errors.Is(err, io.EOF) {
		return n, f.err
	}
	return n, err
}

func TestAReadErrorIsRecordedAsADiagnostic(t *testing.T) {
	spec, _ := streamSpec(t, 1024)
	in := &trackedPipe{r: &failingReader{data: strings.NewReader("{}\n"), err: errors.New("boom")}}
	r := &Run{}
	r.streamEvents(in, spec, &capped{limit: 1 << 10})

	r.mu.Lock()
	diag := r.diag
	r.mu.Unlock()
	if !strings.Contains(diag, "vendor stream read failed") || !strings.Contains(diag, "boom") {
		t.Fatalf("diag = %q, want the read failure", diag)
	}
}

func TestATranscriptWriteFailureStillDrainsThePipe(t *testing.T) {
	spec, tr := streamSpec(t, 1024)
	_ = tr.Close() // every Append now fails
	msg := `{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"hi"}}` + "\n"
	in := &trackedPipe{r: strings.NewReader(strings.Repeat(msg, 50))}
	r := &Run{}
	r.streamEvents(in, spec, &capped{limit: 1 << 10})

	if !in.closed || !in.eofFirst {
		t.Fatalf("pipe closed before the child finished writing (closed=%v, eofFirst=%v)", in.closed, in.eofFirst)
	}
	r.mu.Lock()
	diag := r.diag
	r.mu.Unlock()
	if !strings.Contains(diag, "transcript write failed") {
		t.Fatalf("diag = %q", diag)
	}
}

// An oversized line is transcript content like any other: a child that emits
// nothing else must still hit max_transcript_bytes (FR-12.3).
func TestOversizedLinesCountAgainstTheTranscriptLimit(t *testing.T) {
	tr, err := stream.CreateTranscript(t.TempDir(), "r", 300)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	spec := Spec{MaxEventBytes: 100, Parser: stream.CodexParser{}, Transcript: tr}

	cancelled := false
	r := &Run{cancel: func() { cancelled = true }}
	in := &trackedPipe{r: strings.NewReader(strings.Repeat(strings.Repeat("x", 500)+"\n", 200))}
	r.streamEvents(in, spec, &capped{limit: 1 << 10})

	r.mu.Lock()
	overflow := r.transcriptOverflow
	r.mu.Unlock()
	if !overflow || !cancelled {
		t.Fatalf("transcriptOverflow=%v cancelled=%v, want both true", overflow, cancelled)
	}
}

type recordingCloser struct {
	closed bool
}

func (c *recordingCloser) Write(b []byte) (int, error) { return len(b), nil }
func (c *recordingCloser) Close() error                { c.closed = true; return nil }

// After the transcript fails the stream keeps being parsed: the terminal event
// must still close the child's stdin and the agent's messages must still reach
// the caller.
func TestATranscriptFailureDoesNotStopLifecycleHandling(t *testing.T) {
	tr, err := stream.CreateTranscript(t.TempDir(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = tr.Close() // every Append now fails
	spec := Spec{
		MaxEventBytes: 1024, Parser: stream.ClaudeParser{}, Transcript: tr,
		CloseStdinAfterResult: true,
	}
	w := &recordingCloser{}
	r := &Run{steerCh: &steerChannel{w: w}}
	in := strings.NewReader(
		`{"type":"assistant","session_id":"s","message":{"role":"assistant","content":[{"type":"text","text":"first"}]}}` + "\n" +
			`{"type":"assistant","session_id":"s","message":{"role":"assistant","content":[{"type":"text","text":"second"}]}}` + "\n" +
			`{"type":"result","subtype":"success","session_id":"s","result":"done"}` + "\n")
	out := &capped{limit: 1 << 10}
	r.streamEvents(io.NopCloser(in), spec, out)

	if !w.closed {
		t.Fatal("the result event after a transcript failure did not close the child's stdin")
	}
	if got := out.String(); !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Fatalf("messages after the transcript failure were lost: %q", got)
	}
	r.mu.Lock()
	diag := r.diag
	r.mu.Unlock()
	if !strings.Contains(diag, "transcript write failed") {
		t.Fatalf("diag = %q", diag)
	}
}
