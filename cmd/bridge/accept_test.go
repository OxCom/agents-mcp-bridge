package main

import (
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/control"
	"github.com/oxcom/agents-mcp-bridge/internal/platform"
	"github.com/oxcom/agents-mcp-bridge/internal/run"
)

// TestAnswerRunResolvesThePendingQuestionID pins item 4: `bridge answer`
// must not send an empty QuestionID (which run.Run.AnswerQuestion treats as
// "answer whatever is pending", the operator-blind branch reserved for a
// caller with nothing rendered to bind to) when a question is actually
// pending. answerRun looks up the run's live QuestionID over VerbRuns and
// binds the answer to it — the same identity check that already protects
// the TUI, which sends the id it rendered.
func TestAnswerRunResolvesThePendingQuestionID(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", shortTempDir(t))

	b, _ := newTestBridge(t, "/bin/sleep", []string{"5"})
	b.log = slog.New(slog.NewTextHandler(io.Discard, nil))

	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	srv, err := control.Listen(paths.Runtime(), platform.NewControlEndpoint(), b, b.log)
	if err != nil {
		t.Fatalf("control.Listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	r, err := b.runs.Start(run.Spec{
		RunID: "run-answered", Agent: "echoer", HostAgent: "claude",
		Command: "/bin/sleep", Args: []string{"5"}, CWD: t.TempDir(),
		Env:       []string{"PATH=" + os.Getenv("PATH")},
		Timeout:   5 * time.Second,
		MaxOutput: 1 << 10,
	}, platform.NewProcessGroup())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(b.runs.CancelAll)

	verdicts, err := r.Ask(run.Question{
		ID: "q-live", Tool: "AskUserQuestion", Text: "which way?",
		Asked: time.Now(), Deadline: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if err := answerRun("run-answered", "left"); err != nil {
		t.Fatalf("answerRun: %v", err)
	}

	select {
	case v := <-verdicts:
		if v.Message != "left" {
			t.Fatalf("verdict message = %q, want %q", v.Message, "left")
		}
	case <-time.After(time.Second):
		t.Fatal("answerRun did not deliver a verdict")
	}
}

// TestAnswerRunRefusesWithNothingPending pins the "no escape hatch" choice
// made for item 4: with no question pending, answerRun fails rather than
// blind-answering with an empty QuestionID, which would either silently do
// nothing useful today or answer a question that starts moments later.
func TestAnswerRunRefusesWithNothingPending(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", shortTempDir(t))

	b, _ := newTestBridge(t, "/bin/sleep", []string{"5"})
	b.log = slog.New(slog.NewTextHandler(io.Discard, nil))

	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	srv, err := control.Listen(paths.Runtime(), platform.NewControlEndpoint(), b, b.log)
	if err != nil {
		t.Fatalf("control.Listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	if _, err := b.runs.Start(run.Spec{
		RunID: "run-idle", Agent: "echoer", HostAgent: "claude",
		Command: "/bin/sleep", Args: []string{"5"}, CWD: t.TempDir(),
		Env:       []string{"PATH=" + os.Getenv("PATH")},
		Timeout:   5 * time.Second,
		MaxOutput: 1 << 10,
	}, platform.NewProcessGroup()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(b.runs.CancelAll)

	if err := answerRun("run-idle", "left"); err == nil {
		t.Fatal("answerRun must refuse when nothing is pending, not blind-answer")
	}
}
