package stream

import "testing"

// TestCodexTopLevelErrorIsNotLost pins the shape a usage limit arrives in.
// Observed against codex-cli 0.154.0 on 2026-09-17: the message is a
// TOP-LEVEL "error" event, not an item.completed error item. Mapping it to
// KindUnknown dropped the only line saying why the run exited 1, and the
// caller was told "the agent exited with status 1: " and nothing more.
func TestCodexTopLevelErrorIsNotLost(t *testing.T) {
	line := `{"type":"error","message":"You've hit your usage limit."}`
	e, ok := CodexParser{}.Parse([]byte(line))
	if !ok {
		t.Fatal("line skipped")
	}
	if e.Kind != KindVendorError {
		t.Fatalf("Kind = %q, want %q", e.Kind, KindVendorError)
	}
	if e.Text != "You've hit your usage limit." {
		t.Fatalf("Text = %q", e.Text)
	}
}

// TestCodexTurnFailedCarriesItsReason keeps the turn's own verdict, which
// nests the message one level down and is the last event before a non-zero
// exit.
func TestCodexTurnFailedCarriesItsReason(t *testing.T) {
	line := `{"type":"turn.failed","error":{"message":"You've hit your usage limit."}}`
	e, ok := CodexParser{}.Parse([]byte(line))
	if !ok {
		t.Fatal("line skipped")
	}
	if e.Kind != KindRunFailed {
		t.Fatalf("Kind = %q, want %q", e.Kind, KindRunFailed)
	}
	if e.Text != "You've hit your usage limit." {
		t.Fatalf("Text = %q", e.Text)
	}
}

// TestCodexTurnFailedWithoutErrorObjectStillFails guards the nil dereference:
// a vendor may report the verdict with no body.
func TestCodexTurnFailedWithoutErrorObjectStillFails(t *testing.T) {
	e, ok := CodexParser{}.Parse([]byte(`{"type":"turn.failed"}`))
	if !ok {
		t.Fatal("line skipped")
	}
	if e.Kind != KindRunFailed || e.Text != "" {
		t.Fatalf("Kind = %q, Text = %q", e.Kind, e.Text)
	}
}
