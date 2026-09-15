package stream

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenEvents parses a recorded vendor stream. The files in testdata/streams
// were captured from the real CLIs; re-record them when a pinned version
// changes, which is what the nightly conformance job exists to notice.
func goldenEvents(t *testing.T, p Parser, name string) []Event {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "streams", name)
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("golden stream %s unavailable: %v", name, err)
	}
	defer f.Close()

	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if e, ok := p.Parse([]byte(line)); ok {
			out = append(out, e)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func findKind(events []Event, k Kind) (Event, bool) {
	for _, e := range events {
		if e.Kind == k {
			return e, true
		}
	}
	return Event{}, false
}

func TestCodexGoldenStream(t *testing.T) {
	events := goldenEvents(t, CodexParser{}, "codex.jsonl")
	if len(events) == 0 {
		t.Fatal("no events parsed")
	}

	// The session id is on thread.started, at the top level.
	start, ok := findKind(events, KindRunStarted)
	if !ok || start.SessionID == "" {
		t.Fatal("no session id extracted from the codex stream")
	}

	// The final message is an item.completed whose item.type is agent_message,
	// NOT a distinct event type. A parser keying on a message event finds
	// nothing, which is the mistake this test exists to prevent.
	msg, ok := findKind(events, KindMessage)
	if !ok {
		t.Fatal("the final assistant message was not recognised")
	}
	if !strings.Contains(msg.Text, "GOLDEN") {
		t.Fatalf("message text = %q", msg.Text)
	}

	// Codex reports a config warning as an item of type error. It is a warning
	// about the operator's own setup, not a run failure.
	if _, ok := findKind(events, KindVendorError); !ok {
		t.Log("no vendor error in this recording (fine; it depends on local config)")
	}
	if u, ok := findKind(events, KindUsage); !ok || u.Usage == nil || u.Usage.OutputTokens == 0 {
		t.Fatal("token usage was not extracted")
	}
}

func TestClaudeGoldenStream(t *testing.T) {
	events := goldenEvents(t, ClaudeParser{}, "claude.jsonl")
	if len(events) == 0 {
		t.Fatal("no events parsed")
	}

	// session_id is present on EVERY record, which is what makes correlation
	// possible without waiting for a particular event.
	for i, e := range events {
		if e.SessionID == "" {
			t.Fatalf("event %d (%s) carries no session id", i, e.Kind)
		}
	}

	msg, ok := findKind(events, KindMessage)
	if !ok || !strings.Contains(strings.ToUpper(msg.Text), "GOLDEN") {
		t.Fatalf("assistant message not recognised: %+v", msg)
	}

	// The result record is the completion signal. The process itself does not
	// exit until stdin closes, so nothing may key off process exit.
	if _, ok := findKind(events, KindRunFinished); !ok {
		t.Fatal("the result record was not recognised as run completion")
	}
}

func TestUnknownVendorEventIsKeptNotDropped(t *testing.T) {
	// A vendor adding an event type must not take the bridge down, and must not
	// lose evidence either.
	for _, p := range []Parser{CodexParser{}, ClaudeParser{}} {
		e, ok := p.Parse([]byte(`{"type":"something.brand.new","payload":{"a":1}}`))
		if !ok {
			t.Fatalf("%s dropped an unknown event", p.Name())
		}
		if e.Kind != KindUnknown {
			t.Fatalf("%s classified an unknown event as %q", p.Name(), e.Kind)
		}
		if !strings.Contains(string(e.Raw), "brand.new") {
			t.Fatalf("%s discarded the original bytes", p.Name())
		}
	}
}

func TestMalformedLineNeverFailsTheRun(t *testing.T) {
	for _, p := range []Parser{CodexParser{}, ClaudeParser{}} {
		for _, line := range []string{
			`{"type":`,
			`not json at all`,
			`{"type":"item.completed","item":null}`,
			`[]`,
		} {
			e, ok := p.Parse([]byte(line))
			if !ok {
				continue
			}
			if e.Kind == "" {
				t.Fatalf("%s produced an event with no kind for %q", p.Name(), line)
			}
		}
	}
}

func TestBlankLinesAreSkipped(t *testing.T) {
	if _, ok := (CodexParser{}).Parse(nil); ok {
		t.Fatal("an empty line should produce no event")
	}
}

func TestNonJSONLineIsStillRecordable(t *testing.T) {
	// Raw is a json.RawMessage. A parser that puts a non-JSON line in it makes
	// the whole EVENT unmarshalable, so the transcript silently loses exactly
	// the line the parser was written to preserve.
	dir := t.TempDir()
	tr, err := CreateTranscript(dir, "run", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	for _, p := range []Parser{CodexParser{}, ClaudeParser{}} {
		e, ok := p.Parse([]byte("not json at all"))
		if !ok {
			t.Fatalf("%s dropped the line", p.Name())
		}
		if len(e.Raw) != 0 {
			t.Fatalf("%s put non-JSON into Raw: %q", p.Name(), e.Raw)
		}
		if _, err := tr.Append(e); err != nil {
			t.Fatalf("%s produced an event the transcript cannot record: %v", p.Name(), err)
		}
	}

	events, err := ReadTranscript(tr.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("recorded %d events, want 2", len(events))
	}
	for _, e := range events {
		if e.Text != "not json at all" {
			t.Fatalf("the line was not preserved: %q", e.Text)
		}
	}
}

func TestValidJSONIsKeptInRaw(t *testing.T) {
	// The opposite direction: a real vendor event must keep its payload, so an
	// unrecognised event type loses nothing.
	e, ok := CodexParser{}.Parse([]byte(`{"type":"something.new","detail":{"a":1}}`))
	if !ok || len(e.Raw) == 0 {
		t.Fatalf("valid JSON was not preserved in Raw: ok=%v raw=%q", ok, e.Raw)
	}
}

func TestTranscriptDegradesRatherThanDroppingAnUnmarshalableEvent(t *testing.T) {
	// Defence in depth for a future parser that reintroduces the bug: losing an
	// event is worse than losing its raw payload.
	dir := t.TempDir()
	tr, err := CreateTranscript(dir, "run", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	bad := Event{Kind: KindUnknown, Raw: []byte("this is not json")}
	if _, err := tr.Append(bad); err != nil {
		t.Fatalf("the event was dropped instead of degraded: %v", err)
	}
	events, err := ReadTranscript(tr.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	if events[0].Text == "" {
		t.Fatal("the degraded event carries no explanation of what was lost")
	}
}

func TestParserForOnlyKnowsFullTierAdapters(t *testing.T) {
	if _, ok := ParserFor("codex"); !ok {
		t.Fatal("codex parser missing")
	}
	if _, ok := ParserFor("claude"); !ok {
		t.Fatal("claude parser missing")
	}
	// A basic-tier adapter has no parser, and must not silently get one.
	if _, ok := ParserFor("antigravity"); ok {
		t.Fatal("an unregistered adapter must have no parser")
	}
}
