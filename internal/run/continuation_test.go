package run

import (
	"errors"
	"testing"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/platform"
)

// TestContinuationSuccessorInheritsThePredecessorsSlot pins the fix for the
// wave-2 defect (see the wave-3 report): a continuation's successor must be
// admitted against the room its predecessor's own needs_input rest state
// already occupies, without that slot being freed first. At maxLive=1 the
// predecessor alone already fills the registry (TestSupersedeFreesThe
// SlotAndAuditsOnce pins that an ORDINARY Start refuses here), so this only
// passes if Start actually excludes spec.ResumedFrom from the count.
func TestContinuationSuccessorInheritsThePredecessorsSlot(t *testing.T) {
	reg := NewRegistry(1, time.Hour)
	pred := NewForTest("pred-run")
	if _, err := pred.Ask(Question{ID: "toolu_1", Tool: "AskUserQuestion", Text: "?"}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	pred.NeedsInput("no operator channel was available to answer a question")

	reg.mu.Lock()
	reg.runs[pred.ID] = pred
	reg.mu.Unlock()

	successorSpec := spec(t, "echo", "hi")
	successorSpec.ResumedFrom = pred.ID
	successor, err := reg.Start(successorSpec, platform.NewProcessGroup())
	if err != nil {
		t.Fatalf("Start with ResumedFrom set must inherit the predecessor's slot: %v", err)
	}
	successor.Await(2 * time.Second)

	if s := pred.Snapshot(); s.State != StateNeedsInput {
		t.Fatalf("predecessor state = %q; Start must not move it on its own", s.State)
	}
}

// The wave-2 rollback regression (a continuation whose successor fails to
// start must leave the predecessor exactly as it was) used to live here as
// TestContinuationRollbackWhenSuccessorFailsToStart, driving Registry/Run
// directly rather than cmd/bridge.continueRun. Mutation testing in the
// wave-4 review found it WEAK: it re-implemented continueRun's own
// transaction inside itself, so it kept passing even with the
// Supersede-before-Start ordering wave-3 exists to fix restored. It has
// been replaced by
// TestContinueRunRollsBackWhenSuccessorFailsToStart in
// cmd/bridge/continuation_test.go, which drives the real b.continueRun and
// therefore fails if that ordering — or the unconditional predecessor
// retirement the wave-4 review's Critical 2 added — regresses.
// Registry.RegisterForTest exists to let that test construct the same
// artificial concurrency condition (an unrelated run occupying the
// registry's only other slot) from outside this package.

// TestSupersedeFreesTheSlotAndAuditsOnce covers the terminal half of
// StateSuperseded (docs/11 §3): a superseded run must stop occupying its max_concurrent_runs
// slot immediately, unlike needs_input, and must emit exactly one
// run.superseded audit transition carrying the successor id.
func TestSupersedeFreesTheSlotAndAuditsOnce(t *testing.T) {
	reg := NewRegistry(1, time.Hour)
	type report struct{ runID, agent, event, detail string }
	reports := make(chan report, 4)
	reg.OnTransition(func(runID, agent, event, detail string) {
		reports <- report{runID, agent, event, detail}
	})

	r := NewForTest("stuck-run")
	r.transition = reg.transition
	if _, err := r.Ask(Question{ID: "toolu_1", Tool: "AskUserQuestion", Text: "?"}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	r.NeedsInput("no operator channel was available to answer a question")

	reg.mu.Lock()
	reg.runs[r.ID] = r
	reg.mu.Unlock()

	// While needs_input, the slot is still occupied.
	if _, err := reg.Start(spec(t, "echo", "hi"), platform.NewProcessGroup()); !errors.Is(err, ErrConcurrencyLimit) {
		t.Fatalf("needs_input must still occupy its slot, got err = %v", err)
	}

	// Pins the wave-4 review's Minor finding: Supersede clearing the
	// continuation record (so a superseded run cannot be continued a second
	// time) had no assertion and its mutant survived.
	r.SetContinuation(&ContinuationRecord{Prompt: "do the thing", Agent: "demo", CWD: t.TempDir(), Mode: "read-only"})

	if err := r.Supersede("successor-run"); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if s := r.Snapshot(); s.State != StateSuperseded || s.SupersededBy != "successor-run" {
		t.Fatalf("snapshot = %+v, want superseded by successor-run", s)
	}
	if !StateSuperseded.IsTerminal() {
		t.Fatal("StateSuperseded must be terminal: a superseded run has no live child")
	}
	// TakeContinuation's own state guard (StateSuperseded != StateNeedsInput)
	// would refuse this run either way, which is why the mutant survived:
	// assert the unexported field directly, in-package, so the assertion
	// actually depends on Supersede's own r.continuation = nil line rather
	// than on TakeContinuation's separate state check.
	r.mu.Lock()
	cleared := r.continuation == nil
	r.mu.Unlock()
	if !cleared {
		t.Fatal("Supersede must clear r.continuation so a superseded run cannot be continued a second time")
	}

	// The freed slot must let a new run start under the same ceiling.
	if _, err := reg.Start(spec(t, "echo", "hi"), platform.NewProcessGroup()); err != nil {
		t.Fatalf("Start after Supersede: %v", err)
	}

	select {
	case rep := <-reports:
		if rep.runID != "stuck-run" || rep.event != "run.superseded" || rep.detail != "successor-run" {
			t.Fatalf("unexpected transition report: %+v", rep)
		}
	case <-time.After(time.Second):
		t.Fatal("Supersede did not report a run.superseded transition")
	}
	select {
	case rep := <-reports:
		t.Fatalf("Supersede reported a second transition: %+v", rep)
	default:
	}
}

// TestSupersedeRefusesANonNeedsInputRun pins the "only a run resting in
// needs_input may become superseded" rule: any other source state must
// refuse with ErrNotAwaitingInput rather than silently overwriting whatever
// state already settled there (finish()'s "first settled state wins"
// discipline, applied here too).
func TestSupersedeRefusesANonNeedsInputRun(t *testing.T) {
	r := NewForTest("live-run") // starts in StateRunning
	if err := r.Supersede("successor-run"); !errors.Is(err, ErrNotAwaitingInput) {
		t.Fatalf("Supersede on a running run: got err = %v, want ErrNotAwaitingInput", err)
	}
	if s := r.Snapshot(); s.State != StateRunning {
		t.Fatalf("state changed to %q despite the refusal", s.State)
	}

	r.finish(StateCompleted, "ok", "", "", nil, false)
	if err := r.Supersede("successor-run"); !errors.Is(err, ErrNotAwaitingInput) {
		t.Fatalf("Supersede on a completed run: got err = %v, want ErrNotAwaitingInput", err)
	}
}

// TestTakeContinuationConsumesTheRecordOnce mirrors TakeWorktree's
// consume-once discipline: a second concurrent continuation attempt on the
// same run must not silently reuse the same seed.
func TestTakeContinuationConsumesTheRecordOnce(t *testing.T) {
	r := NewForTest("run-1")
	if _, err := r.Ask(Question{ID: "toolu_1", Tool: "AskUserQuestion", Text: "?"}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	r.NeedsInput("no operator channel was available to answer a question")

	rec := &ContinuationRecord{Prompt: "do the thing", Agent: "codex", CWD: "/repo", Mode: "write"}
	r.SetContinuation(rec)

	got, err := r.TakeContinuation()
	if err != nil {
		t.Fatalf("TakeContinuation: %v", err)
	}
	if got != rec {
		t.Fatalf("TakeContinuation returned %#v, want the attached record", got)
	}

	if _, err := r.TakeContinuation(); !errors.Is(err, ErrContinuationUnavailable) {
		t.Fatalf("second TakeContinuation: got err = %v, want ErrContinuationUnavailable", err)
	}
}

// TestTakeContinuationRefusesWithoutARecord covers a run that never got a
// continuation record attached (e.g. an unconfined write run, refused
// upstream before Spec.Continuation is ever set) — the typed error, not a
// nil record silently accepted.
func TestTakeContinuationRefusesWithoutARecord(t *testing.T) {
	r := NewForTest("run-1")
	if _, err := r.Ask(Question{ID: "toolu_1", Tool: "AskUserQuestion", Text: "?"}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	r.NeedsInput("no operator channel was available to answer a question")

	if _, err := r.TakeContinuation(); !errors.Is(err, ErrContinuationUnavailable) {
		t.Fatalf("got err = %v, want ErrContinuationUnavailable", err)
	}
}

// TestTakeContinuationRefusesOffANeedsInputRun covers a run that never
// stopped on needs_input at all: nothing to continue.
func TestTakeContinuationRefusesOffANeedsInputRun(t *testing.T) {
	r := NewForTest("run-1")
	r.SetContinuation(&ContinuationRecord{Prompt: "do the thing"})
	if _, err := r.TakeContinuation(); !errors.Is(err, ErrNotAwaitingInput) {
		t.Fatalf("got err = %v, want ErrNotAwaitingInput", err)
	}
}

// TestCheckDepthRefusesAtTheConfiguredCap pins the chain-depth ceiling: a
// chain at Depth == maxContinuations must refuse rather than mint one more
// link.
func TestCheckDepthRefusesAtTheConfiguredCap(t *testing.T) {
	rec := &ContinuationRecord{Depth: 3}
	if err := rec.CheckDepth(3); !errors.Is(err, ErrChainTooDeep) {
		t.Fatalf("depth 3 against max 3: got err = %v, want ErrChainTooDeep", err)
	}
	rec2 := &ContinuationRecord{Depth: 2}
	if err := rec2.CheckDepth(3); err != nil {
		t.Fatalf("depth 2 against max 3 must be allowed: %v", err)
	}
}

// TestPredecessorStopsOccupyingItsSlotWhenTheSuccessorIsAdmitted pins the
// accounting half of the handover. Admitting the successor against the
// predecessor's slot (TestContinuationSuccessorInheritsThePredecessorsSlot)
// used to leave BOTH counted as live until continueRun's Supersede landed a
// moment later, so the live set sat at maxLive+1 for the width of that gap,
// and at maxLive+N with N continuations in flight. The predecessor now stops
// counting inside the same critical section that admits the successor: at
// maxLive=2, one continuation plus one ordinary run is exactly two live runs,
// and a third is refused.
func TestPredecessorStopsOccupyingItsSlotWhenTheSuccessorIsAdmitted(t *testing.T) {
	reg := NewRegistry(2, time.Hour)

	pred := NewForTest("pred-run")
	if _, err := pred.Ask(Question{ID: "toolu_1", Tool: "AskUserQuestion", Text: "?"}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	pred.NeedsInput("no operator channel was available to answer a question")
	reg.mu.Lock()
	reg.runs[pred.ID] = pred
	reg.mu.Unlock()

	// spec() derives RunID from the test name, so each Start here needs its
	// own id: identical ids collide on one registry key and the count never
	// rises, which would make this test pass against the defect it pins.
	successorSpec := spec(t, "sleep", "10s")
	successorSpec.RunID = "run-successor"
	successorSpec.ResumedFrom = pred.ID
	successor, err := reg.Start(successorSpec, platform.NewProcessGroup())
	if err != nil {
		t.Fatalf("successor must inherit the predecessor's slot: %v", err)
	}
	defer successor.Cancel()

	// The predecessor has no child left — needs_input means the process is
	// already gone — and its slot now belongs to the successor, so the second
	// slot is genuinely free and this ordinary run must be admitted.
	otherSpec := spec(t, "sleep", "10s")
	otherSpec.RunID = "run-other"
	other, err := reg.Start(otherSpec, platform.NewProcessGroup())
	if err != nil {
		t.Fatalf("the predecessor must stop occupying a slot once its successor is admitted: %v", err)
	}
	defer other.Cancel()

	// Two live runs is the cap: a third must be refused, so the handover
	// frees exactly one slot rather than disabling the accounting.
	thirdSpec := spec(t, "echo", "hi")
	thirdSpec.RunID = "run-third"
	if _, err := reg.Start(thirdSpec, platform.NewProcessGroup()); !errors.Is(err, ErrConcurrencyLimit) {
		t.Fatalf("a third run must exceed maxLive=2, got err = %v", err)
	}

	// Retirement is accounting only: the caller-facing state stays
	// needs_input until Supersede performs the real transition.
	if s := pred.Snapshot(); s.State != StateNeedsInput {
		t.Fatalf("predecessor state = %q, want needs_input until Supersede lands", s.State)
	}
}
