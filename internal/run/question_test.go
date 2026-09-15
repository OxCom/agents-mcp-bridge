package run

import (
	"errors"
	"testing"
	"time"
)

func newTestRun(t *testing.T) *Run { return NewForTest("run-test") }

func TestAskDeliversTheOperatorAnswerToTheWaitingChild(t *testing.T) {
	r := newTestRun(t)
	verdicts, err := r.Ask(Question{ID: "toolu_1", Tool: "AskUserQuestion", Text: "red or blue?"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got := r.PendingQuestion(); got == nil || got.ID != "toolu_1" {
		t.Fatalf("question not pending: %#v", got)
	}
	if err := r.AnswerQuestion("toolu_1", "blue\x1b[31m"); err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	select {
	case v := <-verdicts:
		// C7: an answer reaches the model ONLY as a denial carrying the text.
		// The escape sequence is stripped; the answer survives.
		if !v.Deny || v.Message != "blue[31m" {
			t.Fatalf("want deny+stripped message, got %#v", v)
		}
	case <-time.After(time.Second):
		t.Fatal("no verdict delivered")
	}
	if r.PendingQuestion() != nil {
		t.Fatal("question still pending after an answer")
	}
}

// TestAnswerQuestionRefusesAMismatchedID pins fix-round-1's cross-cutting
// defect at its source: an answer naming a question id other than what is
// currently pending must be refused with ErrQuestionChanged, never
// misdelivered to whatever replaced it, and the replacement must stay
// pending and untouched.
func TestAnswerQuestionRefusesAMismatchedID(t *testing.T) {
	r := newTestRun(t)
	verdicts, err := r.Ask(Question{ID: "q1", Text: "red or blue?"})
	if err != nil {
		t.Fatalf("Ask q1: %v", err)
	}
	if err := r.DenyQuestion("timed out"); err != nil {
		t.Fatalf("Deny q1: %v", err)
	}
	<-verdicts

	q2Verdicts, err := r.Ask(Question{ID: "q2", Text: "yes or no?"})
	if err != nil {
		t.Fatalf("Ask q2: %v", err)
	}

	if err := r.AnswerQuestion("q1", "red"); !errors.Is(err, ErrQuestionChanged) {
		t.Fatalf("AnswerQuestion(q1) once q2 is pending = %v, want ErrQuestionChanged", err)
	}
	select {
	case <-q2Verdicts:
		t.Fatal("q2 must still be pending: a mismatched answer must never resolve it")
	default:
	}
	if got := r.PendingQuestion(); got == nil || got.ID != "q2" {
		t.Fatalf("pending = %#v, want q2 still pending", got)
	}

	// "" always answers whatever is pending — no id to bind to.
	if err := r.AnswerQuestion("", "yes"); err != nil {
		t.Fatalf("AnswerQuestion(\"\") on q2: %v", err)
	}
	if r.PendingQuestion() != nil {
		t.Fatal("q2 should have resolved")
	}
}

func TestAnswerNeverTouchesTheSteerChannel(t *testing.T) {
	r := newTestRun(t)
	ch := &steerChannel{}
	r.steerCh = ch
	if _, err := r.Ask(Question{ID: "toolu_2", Tool: "AskUserQuestion", Text: "?"}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if err := r.AnswerQuestion("toolu_2", "x"); err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if ch.sent != 0 {
		t.Fatalf("answer leaked into the steer channel (%d sends)", ch.sent)
	}
}

func TestNeedsInputIsNotTerminalButEndsTheChild(t *testing.T) {
	r := newTestRun(t)
	r.NeedsInput("no human reachable")
	s := r.Snapshot()
	if s.State != StateNeedsInput {
		t.Fatalf("state = %q", s.State)
	}
	if s.State.IsTerminal() {
		t.Fatal("needs_input must not be terminal: the run stays visible to the operator with no live child")
	}
}

func TestNeedsInputSurvivesTheChildBeingReaped(t *testing.T) {
	r := NewForTest("run-1")
	r.NeedsInput("no operator channel was available to answer a question")
	// The supervise goroutine reaps the killed child and reports the run ended.
	r.finish(StateCancelled, "", "", "", nil, false)
	if got := r.Snapshot().State; got != StateNeedsInput {
		t.Fatalf("state = %q, want needs_input to survive the reap", got)
	}
}

func TestNeedsInputReleasesAWaitingQuestion(t *testing.T) {
	r := NewForTest("run-1")
	verdicts, err := r.Ask(Question{ID: "toolu_1", Tool: "AskUserQuestion", Text: "?"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	r.NeedsInput("no operator channel was available to answer a question")
	select {
	case v := <-verdicts:
		if !v.Deny {
			t.Fatalf("a released waiter must be denied, got %#v", v)
		}
	case <-time.After(time.Second):
		t.Fatal("NeedsInput left the gate call blocked forever")
	}
	if r.PendingQuestion() != nil {
		t.Fatal("needs_input must not keep reporting a stale pending question")
	}
	// A second call must not panic (no double close, no send on a closed
	// channel) and must not resolve anything a second time.
	r.NeedsInput("still no operator channel")
	if got := r.Snapshot().State; got != StateNeedsInput {
		t.Fatalf("state = %q after a repeat call", got)
	}
}

func TestTheQuestionSurvivesForTheCallerAfterNeedsInput(t *testing.T) {
	r := NewForTest("run-1")
	if _, err := r.Ask(Question{ID: "toolu_1", Tool: "AskUserQuestion", Text: "red or blue?"}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	r.NeedsInput("no operator channel was available to answer a question")
	if r.PendingQuestion() != nil {
		t.Fatal("nothing is pending once the waiter is released")
	}
	s := r.Snapshot()
	if s.Question == nil || s.Question.Text != "red or blue?" {
		t.Fatalf("the caller must still be able to see what was asked: %#v", s.Question)
	}
}

// TestNeedsInputWithNoQuestionPendingGuardIsIdempotent covers only the
// idempotency guard and the "nothing to release" path: calling NeedsInput
// when no question was ever asked must not panic or double-close r.done, and
// PendingQuestion must stay nil throughout. It does NOT exercise the waiter
// release fixed by TestNeedsInputReleasesAWaitingQuestion — no Ask happens
// here, so this test alone proves nothing about that path.
func TestNeedsInputWithNoQuestionPendingGuardIsIdempotent(t *testing.T) {
	r := NewForTest("run-2")
	r.NeedsInput("no human reachable")
	if r.PendingQuestion() != nil {
		t.Fatal("no question was ever asked; PendingQuestion must stay nil")
	}
	// Idempotent: calling again must not panic.
	r.NeedsInput("no human reachable, again")
	if got := r.Snapshot().State; got != StateNeedsInput {
		t.Fatalf("state = %q", got)
	}
}

func TestAgentSteeringStillRefusedWhileAQuestionIsPending(t *testing.T) {
	r := newTestRun(t)
	if _, err := r.Ask(Question{ID: "toolu_3", Tool: "AskUserQuestion", Text: "?"}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if err := r.Steer("go left", OriginAgent, 10); err == nil {
		t.Fatal("agent steering must be refused while a question is pending")
	}
}

// TestAskRefusesOnANeedsInputRun covers Important 2's first half: needs_input
// is deliberately non-terminal (IsTerminal is false), but it is still a
// rested state with no live child, and admitting a second question here used
// to let expireIfPastDeadlineLocked later flip the run to StateFailed while a
// verdict channel sat unresolved — stranding a delegated child for a further
// full question_timeout_s. Ask must refuse instead.
func TestAskRefusesOnANeedsInputRun(t *testing.T) {
	r := NewForTest("run-1")
	if _, err := r.Ask(Question{ID: "toolu_1", Tool: "AskUserQuestion", Text: "first?"}); err != nil {
		t.Fatalf("first Ask: %v", err)
	}
	r.NeedsInput("no operator channel was available to answer a question")
	if got := r.Snapshot().State; got != StateNeedsInput {
		t.Fatalf("state = %q, want needs_input", got)
	}

	if _, err := r.Ask(Question{ID: "toolu_2", Tool: "AskUserQuestion", Text: "second?"}); !errors.Is(err, ErrRunNotLive) {
		t.Fatalf("Ask on a needs_input run: err = %v, want ErrRunNotLive", err)
	}
}

// TestFinishReleasesAWaitingQuestion covers Important 2's "finish has the
// same gap" half: a child that crashes, is killed by its timeout, or whose
// run is cancelled while a question is still pending must not strand
// whatever goroutine holds the other end of the verdict channel — nothing
// else will ever resolve it once the run is terminal.
func TestFinishReleasesAWaitingQuestion(t *testing.T) {
	r := NewForTest("run-1")
	verdicts, err := r.Ask(Question{ID: "toolu_1", Tool: "AskUserQuestion", Text: "?"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	r.finish(StateCancelled, "", "cancelled", "cancelled by operator or shutdown", nil, false)

	select {
	case v := <-verdicts:
		if !v.Deny {
			t.Fatalf("a released waiter must be denied, got %#v", v)
		}
	case <-time.After(time.Second):
		t.Fatal("finish left the gate call blocked forever")
	}
	if r.PendingQuestion() != nil {
		t.Fatal("a terminal run must not keep reporting a stale pending question")
	}
}
