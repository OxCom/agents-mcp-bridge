package run

import (
	"strings"
	"testing"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/platform"
	"github.com/oxcom/agents-mcp-bridge/internal/stream"
)

// TestAVendorFailureReachesTheCallerWhenStderrIsEmpty reproduces the codex
// usage limit of 2026-09-17: the CLI reported the refusal on its event stream,
// exited 1 and wrote nothing to stderr, so the caller received
// "the agent exited with status 1: " with no reason at all.
func TestAVendorFailureReachesTheCallerWhenStderrIsEmpty(t *testing.T) {
	dir := t.TempDir()
	tr, err := stream.CreateTranscript(dir, "run-vf", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tr.Close() }()

	const reason = "You've hit your usage limit."
	spec := Spec{
		RunID:   "run-vf",
		Agent:   "codex",
		Command: helperCommand(t),
		Args: []string{"emitfail",
			`{"type":"error","message":"` + reason + `"}`,
			`{"type":"turn.failed","error":{"message":"` + reason + `"}}`,
		},
		CWD:            dir,
		Env:            helperEnviron(),
		Timeout:        15 * time.Second,
		MaxOutput:      1 << 16,
		Parser:         stream.CodexParser{},
		Transcript:     tr,
		TranscriptPath: tr.Path(),
	}

	reg := NewRegistry(4, time.Hour)
	r, err := reg.Start(spec, platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	s := r.Await(10 * time.Second)

	if s.State != StateFailed {
		t.Fatalf("state = %q, want %q", s.State, StateFailed)
	}
	if !strings.Contains(s.Failure, reason) {
		t.Fatalf("Failure = %q, want it to carry the vendor's reason", s.Failure)
	}
	if len(s.VendorErrors) != 1 || s.VendorErrors[0] != reason {
		t.Fatalf("VendorErrors = %q, want the top-level error event", s.VendorErrors)
	}
}
