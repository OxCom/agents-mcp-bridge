package gate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/run"
)

func askJSON(t *testing.T, tool string) Ask {
	t.Helper()
	in, _ := json.Marshal(map[string]any{
		"questions": []map[string]any{{"question": "red or blue?", "options": []map[string]any{
			{"label": "red.txt"}, {"label": "blue.txt"}}}},
	})
	return Ask{RunID: "run-1", Tool: tool, ToolUseID: "toolu_1", Input: in}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func TestAnApprovalIsDeniedAndNeverSurfaced(t *testing.T) {
	r := run.NewForTest("run-1")
	elicited := false
	res := NewResolver(func() (*run.Run, error) { return r, nil }, Channels{
		TUIAttached: func() bool { return true },
		Elicit:      func(context.Context, run.Question) (string, bool) { elicited = true; return "", false },
		Timeout:     time.Second,
	}, func(s string) string { return s }, func(string, string) {})

	got := res.Resolve(context.Background(), askJSON(t, "Bash"))
	if got.Behavior != "deny" {
		t.Fatalf("an approval must be denied, got %#v", got)
	}
	if elicited {
		t.Fatal("an approval must never reach a human in v1")
	}
	if r.PendingQuestion() != nil {
		t.Fatal("an approval must not become a pending question")
	}
}

func TestQuestionPrefersTheTUIOverElicitation(t *testing.T) {
	r := run.NewForTest("run-1")
	elicited := false
	res := NewResolver(func() (*run.Run, error) { return r, nil }, Channels{
		TUIAttached: func() bool { return true },
		Elicit:      func(context.Context, run.Question) (string, bool) { elicited = true; return "x", true },
		Timeout:     2 * time.Second,
	}, func(s string) string { return s }, func(string, string) {})

	done := make(chan Reply, 1)
	go func() { done <- res.Resolve(context.Background(), askJSON(t, "AskUserQuestion")) }()

	waitFor(t, func() bool { return r.PendingQuestion() != nil })
	if err := r.AnswerQuestion("", "blue.txt"); err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	got := <-done
	if got.Behavior != "deny" || got.Message != "blue.txt" {
		t.Fatalf("verdict = %#v", got)
	}
	if elicited {
		t.Fatal("elicitation must not be used while a TUI is attached")
	}
}

func TestQuestionFallsBackToElicitationWithNoTUI(t *testing.T) {
	r := run.NewForTest("run-1")
	res := NewResolver(func() (*run.Run, error) { return r, nil }, Channels{
		TUIAttached: func() bool { return false },
		Elicit:      func(context.Context, run.Question) (string, bool) { return "red.txt", true },
		Timeout:     2 * time.Second,
	}, func(s string) string { return s }, func(string, string) {})

	got := res.Resolve(context.Background(), askJSON(t, "AskUserQuestion"))
	if got.Message != "red.txt" {
		t.Fatalf("verdict = %#v", got)
	}
}

func TestNoChannelFailsClosedImmediately(t *testing.T) {
	r := run.NewForTest("run-1")
	res := NewResolver(func() (*run.Run, error) { return r, nil }, Channels{
		TUIAttached: func() bool { return false },
		Elicit:      func(context.Context, run.Question) (string, bool) { return "", false },
		Timeout:     time.Hour, // must NOT be waited out
	}, func(s string) string { return s }, func(string, string) {})

	start := time.Now()
	got := res.Resolve(context.Background(), askJSON(t, "AskUserQuestion"))
	if time.Since(start) > 5*time.Second {
		t.Fatal("with no human reachable the resolver must fail closed at once")
	}
	if got.Behavior != "deny" {
		t.Fatalf("verdict = %#v", got)
	}
	if r.Snapshot().State != run.StateNeedsInput {
		t.Fatalf("run state = %q, want needs_input", r.Snapshot().State)
	}
}

func TestTUIAttachedButSilentTimesOutIntoNeedsInput(t *testing.T) {
	r := run.NewForTest("run-1")
	res := NewResolver(func() (*run.Run, error) { return r, nil }, Channels{
		TUIAttached: func() bool { return true },
		Elicit:      func(context.Context, run.Question) (string, bool) { return "", false },
		Timeout:     200 * time.Millisecond,
	}, func(s string) string { return s }, func(string, string) {})

	got := res.Resolve(context.Background(), askJSON(t, "AskUserQuestion"))
	if got.Behavior != "deny" {
		t.Fatalf("verdict = %#v", got)
	}
	if r.Snapshot().State != run.StateNeedsInput {
		t.Fatalf("run state = %q, want needs_input", r.Snapshot().State)
	}
}

// TestLookupErrorFailsClosed covers the case the brief does not: the gate
// starts before the run exists, so a lookup can fail (run not yet
// registered, or already reaped). It must deny, not panic, and must never
// allow.
func TestLookupErrorFailsClosed(t *testing.T) {
	res := NewResolver(func() (*run.Run, error) { return nil, errors.New("run not found") }, Channels{
		TUIAttached: func() bool { return true },
		Elicit:      func(context.Context, run.Question) (string, bool) { return "x", true },
		Timeout:     time.Hour,
	}, func(s string) string { return s }, func(string, string) {})

	start := time.Now()
	got := res.Resolve(context.Background(), askJSON(t, "AskUserQuestion"))
	if time.Since(start) > 5*time.Second {
		t.Fatal("a lookup failure must fail closed at once")
	}
	if got.Behavior != "deny" {
		t.Fatalf("a failed lookup must deny, got %#v", got)
	}
}

// TestAnswerRacingAnExpiredTimeoutIsNeverDiscarded covers fix-round-1 finding
// 1 (Critical): a real answer already buffered on verdicts must never lose
// to a timeout that fires in the same instant. Before the priority
// non-blocking check was added, Go's select picked a ready case at random,
// so this could discard the answer, tell the child noHuman, and drive the
// run to needs_input even though the operator answered in time.
//
// Fix-round-2: a single iteration is not a reliable regression check. With
// both the verdict and the (already-closed) timeout channel ready at once,
// select picks between them uniformly at random even in the unfixed code, so
// one run passes about half the time. Looping 50 times, each with a fresh
// run/question/verdict, drives the probability that unfixed code passes
// every iteration down to negligible — this is the whole point of the
// change, not incidental thoroughness.
func TestAnswerRacingAnExpiredTimeoutIsNeverDiscarded(t *testing.T) {
	res := &resolver{clean: func(s string) string { return s }, audit: func(string, string) {}}

	for i := 0; i < 50; i++ {
		r := run.NewForTest("run-1")
		q := run.Question{ID: "toolu_1", Tool: "AskUserQuestion", Text: "red or blue?"}
		verdicts, err := r.Ask(q)
		if err != nil {
			t.Fatalf("iteration %d: Ask: %v", i, err)
		}
		if err := r.AnswerQuestion(q.ID, "blue.txt"); err != nil {
			t.Fatalf("iteration %d: AnswerQuestion: %v", i, err)
		}

		timedOut := make(chan time.Time)
		close(timedOut) // already expired

		got := res.awaitVerdict(context.Background(), r, q, verdicts, timedOut)
		if got.Behavior != "deny" || got.Message != "blue.txt" {
			t.Fatalf("iteration %d: a buffered answer racing an expired timeout must win, got %#v", i, got)
		}
		if r.Snapshot().State == run.StateNeedsInput {
			t.Fatalf("iteration %d: an answer that was delivered must not force needs_input", i)
		}
	}
}

// TestDenyQuestionAfterAnswerReturnsErrorSoResolverMustNotForceNeedsInput
// covers fix-round-1 finding 2: when the timeout branch's DenyQuestion loses
// the resolve race to a concurrent real answer, it must return an error
// rather than silently doing nothing, since awaitVerdict's timeout and
// ctx.Done cases both gate their NeedsInput call on that error. This pins the
// run-level contract the resolver's fix depends on.
func TestDenyQuestionAfterAnswerReturnsErrorSoResolverMustNotForceNeedsInput(t *testing.T) {
	r := run.NewForTest("run-1")
	q := run.Question{ID: "toolu_1", Tool: "AskUserQuestion", Text: "red or blue?"}
	if _, err := r.Ask(q); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if err := r.AnswerQuestion(q.ID, "blue.txt"); err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}

	if err := r.DenyQuestion(noHuman); err == nil {
		t.Fatal("DenyQuestion must fail once the question is already answered")
	}
	if r.Snapshot().State == run.StateNeedsInput {
		t.Fatal("a lost DenyQuestion race must not by itself move the run to needs_input")
	}
}

// TestCtxCancelledWhileWaitingTransitionsToNeedsInput covers fix-round-1
// finding 3: cancelling the request while a TUI wait is in flight must still
// leave the run somewhere the operator can see and act on, not StateRunning
// with nothing pending and no path to completion.
func TestCtxCancelledWhileWaitingTransitionsToNeedsInput(t *testing.T) {
	r := run.NewForTest("run-1")
	var events []string
	res := NewResolver(func() (*run.Run, error) { return r, nil }, Channels{
		TUIAttached: func() bool { return true },
		Elicit:      func(context.Context, run.Question) (string, bool) { return "", false },
		Timeout:     time.Hour,
	}, func(s string) string { return s }, func(event, detail string) { events = append(events, event) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Reply, 1)
	go func() { done <- res.Resolve(ctx, askJSON(t, "AskUserQuestion")) }()
	waitFor(t, func() bool { return r.PendingQuestion() != nil })
	cancel()

	got := <-done
	if got.Behavior != "deny" {
		t.Fatalf("verdict = %#v", got)
	}
	if r.Snapshot().State != run.StateNeedsInput {
		t.Fatalf("run state = %q, want needs_input", r.Snapshot().State)
	}
	// docs/11 §3 invariant 5: every transition is one audit entry. The
	// ctx.Done() branch must audit its needs_input transition exactly like
	// the timeout and no-channel branches do.
	if len(events) == 0 || events[len(events)-1] != "question.timeout" {
		t.Fatalf("events = %v, want the cancelled transition to end with question.timeout", events)
	}
}

// TestElicitedAnswerThatFailsToDeliverFallsClosed covers fix-round-1 finding
// 4: an elicited answer that AnswerQuestion refuses (here, oversized) must
// not be reported to the child as delivered.
func TestElicitedAnswerThatFailsToDeliverFallsClosed(t *testing.T) {
	r := run.NewForTest("run-1")
	oversized := strings.Repeat("x", run.MaxSteerBytes+1)
	res := NewResolver(func() (*run.Run, error) { return r, nil }, Channels{
		TUIAttached: func() bool { return false },
		Elicit:      func(context.Context, run.Question) (string, bool) { return oversized, true },
		Timeout:     2 * time.Second,
	}, func(s string) string { return s }, func(string, string) {})

	got := res.Resolve(context.Background(), askJSON(t, "AskUserQuestion"))
	if got.Behavior != "deny" || got.Message != noHuman {
		t.Fatalf("an answer that fails to deliver must fail closed, got %#v", got)
	}
	if r.Snapshot().State != run.StateNeedsInput {
		t.Fatalf("run state = %q, want needs_input", r.Snapshot().State)
	}
}

// TestUnansweredElicitationFailsClosedAfterTheTimeout covers C2: the context
// passed into Resolve is context.Background(), so nothing bounded an
// elicitation branch whose host accepts elicitation/create and then never
// answers. Elicit here ignores its context entirely and blocks forever, the
// way a broken or hostile host might — the fix must not rely on the host
// honouring cancellation.
func TestUnansweredElicitationFailsClosedAfterTheTimeout(t *testing.T) {
	r := run.NewForTest("run-1")
	res := NewResolver(func() (*run.Run, error) { return r, nil }, Channels{
		TUIAttached: func() bool { return false },
		Elicit: func(context.Context, run.Question) (string, bool) {
			select {} // never returns, ignores ctx entirely
		},
		Timeout: 200 * time.Millisecond,
	}, func(s string) string { return s }, func(string, string) {})

	start := time.Now()
	got := res.Resolve(context.Background(), askJSON(t, "AskUserQuestion"))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("resolver did not give up on an unanswered elicitation: took %s", elapsed)
	}
	if got.Behavior != "deny" || got.Message != noHuman {
		t.Fatalf("verdict = %#v", got)
	}
	if r.Snapshot().State != run.StateNeedsInput {
		t.Fatalf("run state = %q, want needs_input", r.Snapshot().State)
	}
}

// TestChildSuppliedEmptyToolUseIDNeverProducesAnEmptyQuestionID covers I2:
// the question id must be one the bridge minted, never the child's own
// tool_use_id verbatim — an empty tool_use_id must not reach run.resolve's
// "answer whatever is pending" branch, reserved for the operator's blind
// `bridge answer` CLI.
func TestChildSuppliedEmptyToolUseIDNeverProducesAnEmptyQuestionID(t *testing.T) {
	r := run.NewForTest("run-1")
	res := NewResolver(func() (*run.Run, error) { return r, nil }, Channels{
		TUIAttached: func() bool { return true },
		Elicit:      func(context.Context, run.Question) (string, bool) { return "", false },
		Timeout:     time.Hour,
	}, func(s string) string { return s }, func(string, string) {})

	a := askJSON(t, "AskUserQuestion")
	a.ToolUseID = "" // a hostile or buggy child sends no id at all

	done := make(chan Reply, 1)
	go func() { done <- res.Resolve(context.Background(), a) }()
	waitFor(t, func() bool { return r.PendingQuestion() != nil })

	q := r.PendingQuestion()
	if q.ID == "" {
		t.Fatal("the bridge must mint its own question id; an empty child tool_use_id must never surface as one")
	}
	if q.ChildQuestionID != "" {
		t.Fatalf("ChildQuestionID = %q, want the empty tool_use_id preserved for correlation only", q.ChildQuestionID)
	}

	if err := r.AnswerQuestion(q.ID, "blue.txt"); err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	<-done
}

// TestControlChannelAnswerProducesExactlyOneAnsweredAuditEntry pins the
// fix-round-3 dedup: cmd/bridge's Answer (the operator's control-channel
// route, which does nothing but call run.AnswerQuestion) used to write its
// own question.answered entry on top of this one, double-counting every
// operator answer. The resolver's sink is the only writer now, and it must
// fire exactly once per answer regardless of which channel delivered it.
func TestControlChannelAnswerProducesExactlyOneAnsweredAuditEntry(t *testing.T) {
	r := run.NewForTest("run-1")
	var events []string
	res := NewResolver(func() (*run.Run, error) { return r, nil }, Channels{
		TUIAttached: func() bool { return true },
		Elicit:      func(context.Context, run.Question) (string, bool) { return "", false },
		Timeout:     2 * time.Second,
	}, func(s string) string { return s }, func(event, _ string) { events = append(events, event) })

	done := make(chan Reply, 1)
	go func() { done <- res.Resolve(context.Background(), askJSON(t, "AskUserQuestion")) }()

	waitFor(t, func() bool { return r.PendingQuestion() != nil })
	// This is exactly what cmd/bridge's Answer does on the control channel:
	// call AnswerQuestion and nothing else.
	if err := r.AnswerQuestion("", "blue.txt"); err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	<-done

	answered := 0
	for _, e := range events {
		if e == "question.answered" {
			answered++
		}
	}
	if answered != 1 {
		t.Fatalf("question.answered entries = %d, want exactly 1; events = %v", answered, events)
	}
}

// TestElicitedAnswerProducesExactlyOneAnsweredAuditEntry covers the other
// answer route the dedup had to preserve: an answer delivered through
// elicitation (no TUI attached) must still produce exactly one
// question.answered entry, from the same resolver sink the control-channel
// route uses.
func TestElicitedAnswerProducesExactlyOneAnsweredAuditEntry(t *testing.T) {
	r := run.NewForTest("run-1")
	var events []string
	res := NewResolver(func() (*run.Run, error) { return r, nil }, Channels{
		TUIAttached: func() bool { return false },
		Elicit:      func(context.Context, run.Question) (string, bool) { return "red.txt", true },
		Timeout:     2 * time.Second,
	}, func(s string) string { return s }, func(event, _ string) { events = append(events, event) })

	got := res.Resolve(context.Background(), askJSON(t, "AskUserQuestion"))
	if got.Message != "red.txt" {
		t.Fatalf("verdict = %#v", got)
	}

	answered := 0
	for _, e := range events {
		if e == "question.answered" {
			answered++
		}
	}
	if answered != 1 {
		t.Fatalf("question.answered entries = %d, want exactly 1; events = %v", answered, events)
	}
}
