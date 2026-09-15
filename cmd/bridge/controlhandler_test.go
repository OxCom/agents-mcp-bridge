package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/oxcom/agents-mcp-bridge/internal/config"
	"github.com/oxcom/agents-mcp-bridge/internal/control"
	"github.com/oxcom/agents-mcp-bridge/internal/run"
)

// TestAnswerWithNoPendingQuestionFailsAndIsNotAudited pins the behaviour the
// fix-round reviewer confirmed but nothing in cmd/bridge tested: Answer on a
// run that never asked anything must return run.ErrNoQuestion, a clear error
// rather than a silent success, and must not write a question.answered audit
// entry for an answer that was never delivered.
func TestAnswerWithNoPendingQuestionFailsAndIsNotAudited(t *testing.T) {
	b, auditPath := newTestBridge(t, "/bin/sleep", []string{"5"})
	ask := b.makeAsk("echoer")
	_, out, err := ask(context.Background(), nil, askInput{Prompt: "hi"})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	t.Cleanup(b.runs.CancelAll)

	err = b.Answer(out.RunID, "", "an answer nobody asked for")
	if !errors.Is(err, run.ErrNoQuestion) {
		t.Fatalf("Answer on a run with nothing pending = %v, want run.ErrNoQuestion", err)
	}

	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if strings.Contains(string(raw), `"event":"question.answered"`) {
		t.Fatal("an answer that was refused must not be audited as answered")
	}
}

// TestRunsReportsAPendingQuestion pins Task 8b: the watch TUI's question
// panel is populated from control.RunInfo, so a run blocked on a question
// must carry its text and options over the control channel, and a run with
// nothing pending must report empty fields.
func TestRunsReportsAPendingQuestion(t *testing.T) {
	b, _ := newTestBridge(t, "/bin/sleep", []string{"30"})
	b.cfg.Agents["echoer"].Invoke = &config.Invocation{Args: []string{"30"}, Prompt: "argv"}

	ask := b.makeAsk("echoer")
	_, started, err := ask(context.Background(), nil, askInput{Prompt: "x"})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	t.Cleanup(b.runs.CancelAll)

	// Before any question is asked, the run reports nothing pending.
	before := findRunInfo(t, b.Runs(), started.RunID)
	if before.QuestionID != "" || before.QuestionText != "" || len(before.QuestionOptions) != 0 {
		t.Fatalf("a run with no question reported %+v", before)
	}

	r, err := b.runs.Get(started.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if _, err := r.Ask(run.Question{
		ID: "toolu_1", Tool: "AskUserQuestion",
		Text: "red or blue?", Options: []string{"red.txt", "blue.txt"},
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	after := findRunInfo(t, b.Runs(), started.RunID)
	if after.QuestionID != "toolu_1" {
		t.Fatalf("QuestionID = %q, want toolu_1", after.QuestionID)
	}
	if after.QuestionText != "red or blue?" {
		t.Fatalf("QuestionText = %q", after.QuestionText)
	}
	if len(after.QuestionOptions) != 2 || after.QuestionOptions[0] != "red.txt" || after.QuestionOptions[1] != "blue.txt" {
		t.Fatalf("QuestionOptions = %v", after.QuestionOptions)
	}

	// Answering with the right question id clears it again.
	if err := b.Answer(started.RunID, "toolu_1", "red.txt"); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	cleared := findRunInfo(t, b.Runs(), started.RunID)
	if cleared.QuestionID != "" {
		t.Fatalf("an answered question is still reported: %+v", cleared)
	}
}

// TestRunsReportsTheSecondQuestionAfterTheFirstIsGone is the control-channel
// side of fix-round-1's identity-binding property: a second question on the
// same run must fully replace the first in what Runs() reports, never leave
// stale fields from Q1 mixed with Q2's, and an operator answer scoped to Q1's
// id must be refused once Q2 is what is actually pending.
func TestRunsReportsTheSecondQuestionAfterTheFirstIsGone(t *testing.T) {
	b, _ := newTestBridge(t, "/bin/sleep", []string{"30"})
	b.cfg.Agents["echoer"].Invoke = &config.Invocation{Args: []string{"30"}, Prompt: "argv"}

	ask := b.makeAsk("echoer")
	_, started, err := ask(context.Background(), nil, askInput{Prompt: "x"})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	t.Cleanup(b.runs.CancelAll)

	r, err := b.runs.Get(started.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if _, err := r.Ask(run.Question{ID: "q1", Text: "red or blue?", Options: []string{"red", "blue"}}); err != nil {
		t.Fatalf("Ask q1: %v", err)
	}
	// q1 resolves another way — a CLI answer with no question id — before Q2
	// is ever asked.
	if err := b.Answer(started.RunID, "", "cli answer"); err != nil {
		t.Fatalf("Answer q1 blind: %v", err)
	}
	if _, err := r.Ask(run.Question{ID: "q2", Text: "yes or no?", Options: []string{"yes", "no"}}); err != nil {
		t.Fatalf("Ask q2: %v", err)
	}

	info := findRunInfo(t, b.Runs(), started.RunID)
	if info.QuestionID != "q2" || info.QuestionText != "yes or no?" {
		t.Fatalf("Runs() still reflects q1, not q2: %+v", info)
	}
	if len(info.QuestionOptions) != 2 || info.QuestionOptions[0] != "yes" || info.QuestionOptions[1] != "no" {
		t.Fatalf("QuestionOptions = %v, want q2's", info.QuestionOptions)
	}

	// An operator answer bound to q1's id must not resolve q2.
	if err := b.Answer(started.RunID, "q1", "red"); !errors.Is(err, run.ErrQuestionChanged) {
		t.Fatalf("Answer(q1) once q2 is pending = %v, want ErrQuestionChanged", err)
	}
	stillPending := findRunInfo(t, b.Runs(), started.RunID)
	if stillPending.QuestionID != "q2" {
		t.Fatalf("q2 must still be pending untouched: %+v", stillPending)
	}
}

func findRunInfo(t *testing.T, runs []control.RunInfo, id string) control.RunInfo {
	t.Helper()
	for _, r := range runs {
		if r.RunID == id {
			return r
		}
	}
	t.Fatalf("no run %q in %+v", id, runs)
	return control.RunInfo{}
}
