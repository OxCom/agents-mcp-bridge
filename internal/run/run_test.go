package run

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/platform"
	"github.com/oxcom/agents-mcp-bridge/internal/stream"
)

// spec asks the test-binary helper for one behaviour (see helper_test.go).
// No POSIX binary is named, so the spawn works on every supported OS.
func spec(t *testing.T, behaviour string, args ...string) Spec {
	t.Helper()
	return rawSpec(t, helperCommand(t), append([]string{behaviour}, args...)...)
}

// rawSpec names a command directly. Only the two missing-binary tests and the
// POSIX-only process-group test need it.
func rawSpec(t *testing.T, cmd string, args ...string) Spec {
	t.Helper()
	return Spec{
		RunID:     "run-" + t.Name(),
		Agent:     "demo",
		HostAgent: "claude",
		Command:   cmd,
		Args:      args,
		CWD:       t.TempDir(),
		Env:       helperEnviron(),
		Timeout:   10 * time.Second,
		MaxOutput: 1 << 16,
	}
}

func TestCompletedRunCapturesOutput(t *testing.T) {
	reg := NewRegistry(4, time.Hour)
	r, err := reg.Start(spec(t, "echo", "hello"), platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	s := r.Await(5 * time.Second)
	if s.State != StateCompleted {
		t.Fatalf("state = %q, failure = %q", s.State, s.Failure)
	}
	if !strings.Contains(s.Output, "hello") {
		t.Fatalf("output = %q", s.Output)
	}
	if s.ExitCode == nil || *s.ExitCode != 0 {
		t.Fatalf("exit code = %v", s.ExitCode)
	}
}

func TestNonZeroExitIsAFailureNotAResult(t *testing.T) {
	reg := NewRegistry(4, time.Hour)
	r, err := reg.Start(spec(t, "fail", "out", "boom", "3"), platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	s := r.Await(5 * time.Second)
	if s.State != StateFailed {
		t.Fatalf("state = %q, want failed", s.State)
	}
	if s.ExitCode == nil || *s.ExitCode != 3 {
		t.Fatalf("exit code = %v, want 3", s.ExitCode)
	}
	if !strings.Contains(s.Failure, "boom") {
		t.Fatalf("stderr not surfaced in the failure: %q", s.Failure)
	}
	if !strings.Contains(s.Output, "out") {
		t.Fatal("partial output must still be returned")
	}
}

func TestCancelStopsTheRun(t *testing.T) {
	reg := NewRegistry(4, time.Hour)
	r, err := reg.Start(spec(t, "sleep", "60s"), platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		r.Cancel()
	}()
	s := r.Await(10 * time.Second)
	if s.State != StateCancelled {
		t.Fatalf("state = %q, want cancelled", s.State)
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	reg := NewRegistry(4, time.Hour)
	r, err := reg.Start(spec(t, "sleep", "60s"), platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	r.Cancel()
	r.Cancel()
	if s := r.Await(10 * time.Second); s.State != StateCancelled {
		t.Fatalf("state = %q", s.State)
	}
}

func TestOutputIsCappedWithoutKillingTheChild(t *testing.T) {
	// Closing the pipe early would SIGPIPE the child and turn an oversized
	// answer into a crash.
	reg := NewRegistry(4, time.Hour)
	sp := spec(t, "bytes", "200000")
	sp.MaxOutput = 1024
	r, err := reg.Start(sp, platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	s := r.Await(10 * time.Second)
	if s.State != StateCompleted {
		t.Fatalf("state = %q failure = %q", s.State, s.Failure)
	}
	if len(s.Output) > 1024 {
		t.Fatalf("cap ignored: %d bytes", len(s.Output))
	}
	if !s.Truncated {
		t.Fatal("truncation not reported")
	}
}

func TestPromptOnStdinNeverTouchesArgv(t *testing.T) {
	reg := NewRegistry(4, time.Hour)
	sp := spec(t, "cat")
	sp.Prompt = "the prompt body"
	sp.PromptStdin = true
	r, err := reg.Start(sp, platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	s := r.Await(5 * time.Second)
	if !strings.Contains(s.Output, "the prompt body") {
		t.Fatalf("stdin not delivered: %q", s.Output)
	}
	for _, a := range sp.Args {
		if strings.Contains(a, "the prompt body") {
			t.Fatal("the prompt reached argv, where process listings expose it")
		}
	}
}

func TestConcurrencyLimitRefusesRatherThanQueues(t *testing.T) {
	reg := NewRegistry(1, time.Hour)
	first, err := reg.Start(spec(t, "sleep", "5s"), platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Cancel()

	if _, err := reg.Start(spec(t, "echo", "hi"), platform.NewProcessGroup()); err == nil {
		t.Fatal("the concurrency ceiling was not enforced")
	}
}

func TestAwaitIsRecallableAndReportsStillRunning(t *testing.T) {
	reg := NewRegistry(4, time.Hour)
	r, err := reg.Start(spec(t, "sleep", "2s"), platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Cancel()

	if s := r.Await(100 * time.Millisecond); s.State != StateRunning {
		t.Fatalf("first await state = %q, want running", s.State)
	}
	if s := r.Await(10 * time.Second); s.State != StateCompleted {
		t.Fatalf("second await state = %q, want completed", s.State)
	}
}

func TestUnawaitedRunsAreReported(t *testing.T) {
	// A run nobody collects burns tokens for a result no one reads.
	reg := NewRegistry(4, time.Hour)
	r, err := reg.Start(spec(t, "echo", "hi"), platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, StateCompleted)

	if got := reg.Abandoned(); len(got) != 1 {
		t.Fatalf("abandoned = %d, want 1", len(got))
	}
	r.Await(time.Second)
	if got := reg.Abandoned(); len(got) != 0 {
		t.Fatalf("a collected run is not abandoned; got %d", len(got))
	}
}

func TestFinishedRunStaysResolvableWithinRetention(t *testing.T) {
	reg := NewRegistry(4, time.Hour)
	r, err := reg.Start(spec(t, "echo", "hi"), platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, StateCompleted)
	if _, err := reg.Get(r.ID); err != nil {
		t.Fatalf("a finished run must stay resolvable: %v", err)
	}
}

func TestRunIsForgottenAfterRetention(t *testing.T) {
	reg := NewRegistry(4, time.Millisecond)
	r, err := reg.Start(spec(t, "echo", "hi"), platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, r, StateCompleted)
	time.Sleep(20 * time.Millisecond)
	if _, err := reg.Get(r.ID); err == nil {
		t.Fatal("a run past its retention window must be forgotten")
	}
}

func TestCancelAllLeavesNoLiveRun(t *testing.T) {
	// No run outlives its server.
	reg := NewRegistry(4, time.Hour)
	var runs []*Run
	for i := 0; i < 3; i++ {
		sp := spec(t, "sleep", "30s")
		sp.RunID = sp.RunID + string(rune('a'+i))
		r, err := reg.Start(sp, platform.NewProcessGroup())
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, r)
	}
	reg.CancelAll()
	for _, r := range runs {
		if s := r.Await(10 * time.Second); !s.State.IsTerminal() {
			t.Fatalf("run %s survived shutdown in state %q", r.ID, s.State)
		}
	}
}

func TestMissingCommandFailsTheRunNotTheServer(t *testing.T) {
	reg := NewRegistry(4, time.Hour)
	r, err := reg.Start(rawSpec(t, "/nonexistent/binary"), platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	if s := r.Await(5 * time.Second); s.State != StateFailed {
		t.Fatalf("state = %q, want failed", s.State)
	}
}

// TestNeedsInputPastDeadlineTransitionsAndFreesTheSlot covers C3: a run
// resting in needs_input past its question's deadline must become failed and
// stop counting against max_concurrent_runs, or a delegated agent that asks
// questions with no operator attached could exhaust delegation capacity
// until the process restarts.
func TestNeedsInputPastDeadlineTransitionsAndFreesTheSlot(t *testing.T) {
	reg := NewRegistry(1, time.Hour)
	r := NewForTest("stuck-run")
	// NeedsInput re-anchors its own deadline to the moment it is called,
	// using the question's Asked→Deadline span as the duration (see
	// question.go): a short positive span here, then a wait past it, is how
	// a test observes the needs_input rest period actually expiring.
	asked := time.Now()
	if _, err := r.Ask(Question{
		ID:       "toolu_1",
		Tool:     "AskUserQuestion",
		Text:     "?",
		Asked:    asked,
		Deadline: asked.Add(5 * time.Millisecond),
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	r.NeedsInput("no operator channel was available to answer a question")
	time.Sleep(50 * time.Millisecond)

	reg.mu.Lock()
	reg.runs[r.ID] = r
	reg.mu.Unlock()

	if s := r.Snapshot(); s.State != StateFailed {
		t.Fatalf("state = %q, want a run past its deadline to become failed", s.State)
	}

	// The freed slot must let a new run start under the same ceiling.
	if _, err := reg.Start(spec(t, "echo", "hi"), platform.NewProcessGroup()); err != nil {
		t.Fatalf("Start after the stuck run expired: %v", err)
	}
}

// TestRefusedStartClosesTheTranscript pins that Start owns spec.Transcript on
// the error path too. The caller opens the file before it knows whether the run
// will be admitted, and a leaked handle is invisible on POSIX but blocks the
// directory's removal on Windows, where CI caught it.
func TestRefusedStartClosesTheTranscript(t *testing.T) {
	reg := NewRegistry(1, time.Hour)
	held := NewForTest("occupies-the-only-slot")
	reg.mu.Lock()
	reg.runs[held.ID] = held
	reg.mu.Unlock()

	tr, err := stream.CreateTranscript(t.TempDir(), "run-refused", 1<<20)
	if err != nil {
		t.Fatalf("CreateTranscript: %v", err)
	}
	s := spec(t, "echo", "hi")
	s.Transcript = tr

	if _, err := reg.Start(s, platform.NewProcessGroup()); !errors.Is(err, ErrConcurrencyLimit) {
		t.Fatalf("Start err = %v, want ErrConcurrencyLimit", err)
	}
	// Closing an already-closed file reports it, which is the only portable
	// proof that Start closed this one.
	if err := tr.Close(); err == nil {
		t.Fatal("Start left the transcript open after refusing the run")
	}
}

// TestNeedsInputWithinDeadlineStillCountsAgainstTheLimit is
// TestNeedsInputPastDeadlineTransitionsAndFreesTheSlot's control: a run whose
// deadline has not yet passed must keep occupying its slot and must stay
// visible and needs_input, not silently fail early.
func TestNeedsInputWithinDeadlineStillCountsAgainstTheLimit(t *testing.T) {
	reg := NewRegistry(1, time.Hour)
	r := NewForTest("stuck-run")
	if _, err := r.Ask(Question{
		ID:       "toolu_1",
		Tool:     "AskUserQuestion",
		Text:     "?",
		Deadline: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	r.NeedsInput("no operator channel was available to answer a question")

	reg.mu.Lock()
	reg.runs[r.ID] = r
	reg.mu.Unlock()

	if _, err := reg.Start(spec(t, "echo", "hi"), platform.NewProcessGroup()); !errors.Is(err, ErrConcurrencyLimit) {
		t.Fatalf("a needs_input run within its deadline must still occupy its slot, got err = %v", err)
	}
	got, err := reg.Get(r.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s := got.Snapshot(); s.State != StateNeedsInput {
		t.Fatalf("state = %q, want needs_input to survive while the deadline has not passed", s.State)
	}
}

// TestExpiryResolvesAStrandedPendingQuestionDefensively covers Important 2's
// belt-and-suspenders half. Ask now refuses on a needs_input run (see
// question_test.go:TestAskRefusesOnANeedsInputRun), so r.pending should
// never actually be non-nil while state == StateNeedsInput on a real path;
// this pins that expireIfPastDeadlineLocked would still resolve it if it
// ever were, by reaching into the run's unexported fields directly — the
// one way, from this package, to construct the state the guard is meant to
// make unreachable.
func TestExpiryResolvesAStrandedPendingQuestionDefensively(t *testing.T) {
	r := NewForTest("stuck-run")
	r.mu.Lock()
	r.state = StateNeedsInput
	r.deadline = time.Now().Add(-time.Second) // already past
	ch := make(chan Verdict, 1)
	r.pending = &Question{ID: "toolu_1", Tool: "AskUserQuestion", Text: "?"}
	r.verdict = ch
	r.mu.Unlock()

	s := r.Snapshot()
	if s.State != StateFailed {
		t.Fatalf("state = %q, want failed once the deadline has passed", s.State)
	}

	select {
	case v := <-ch:
		if !v.Deny {
			t.Fatalf("a released waiter must be denied, got %#v", v)
		}
	case <-time.After(time.Second):
		t.Fatal("expiry left a stranded pending question's gate call blocked forever")
	}
}

// TestExpiryReportsATransitionForAudit covers the missing-audit-entry
// finding: the needs_input -> failed expiry has no other audit sink in
// reach (docs/11 §3 invariant 5), so Registry.OnTransition's callback must
// fire for it.
func TestExpiryReportsATransitionForAudit(t *testing.T) {
	reg := NewRegistry(4, time.Hour)
	type report struct{ runID, agent, event, detail string }
	reports := make(chan report, 1)
	reg.OnTransition(func(runID, agent, event, detail string) {
		reports <- report{runID, agent, event, detail}
	})

	r := NewForTest("stuck-run")
	r.transition = reg.transition
	asked := time.Now()
	if _, err := r.Ask(Question{
		ID: "toolu_1", Tool: "AskUserQuestion", Text: "?",
		Asked: asked, Deadline: asked.Add(5 * time.Millisecond),
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	r.NeedsInput("no operator channel was available to answer a question")
	time.Sleep(50 * time.Millisecond)

	if s := r.Snapshot().State; s != StateFailed {
		t.Fatalf("state = %q, want failed once the deadline has passed", s)
	}

	select {
	case rep := <-reports:
		if rep.runID != "stuck-run" || rep.event != "run.expired" || rep.agent != "test" {
			t.Fatalf("report = %+v, want runID=stuck-run event=run.expired agent=test", rep)
		}
	case <-time.After(time.Second):
		t.Fatal("expiry never reported a transition for audit")
	}
}

func waitFor(t *testing.T, r *Run, want State) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s := r.Snapshot(); s.State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run did not reach %q; state = %q", want, r.Snapshot().State)
}

func TestChildRunsInTheDirectoryPolicyApproved(t *testing.T) {
	// The canonical, validated path must be what the child actually gets. If
	// cmd.Dir were wired to anything else, every containment test above would
	// still pass while the child ran somewhere else entirely.
	reg := NewRegistry(4, time.Hour)
	approved := t.TempDir()
	sp := spec(t, "pwd")
	sp.CWD = approved
	r, err := reg.Start(sp, platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	s := r.Await(5 * time.Second)
	got := strings.TrimSpace(s.Output)

	// macOS reports /private/var for /var, so compare resolved forms.
	want, err := filepath.EvalSymlinks(approved)
	if err != nil {
		t.Fatal(err)
	}
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("child reported an unusable cwd %q: %v", got, err)
	}
	if gotResolved != want {
		t.Fatalf("child ran in %q, want the approved directory %q", gotResolved, want)
	}
}

func TestCallerFacingFailureHidesHostDetail(t *testing.T) {
	// A missing binary must not tell an untrusted caller the path we tried or
	// what the OS said about it.
	reg := NewRegistry(4, time.Hour)
	sp := rawSpec(t, "/opt/secret-location/agent-binary")
	r, err := reg.Start(sp, platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	s := r.Await(5 * time.Second)
	if s.State != StateFailed {
		t.Fatalf("state = %q", s.State)
	}
	if strings.Contains(s.Failure, "secret-location") || strings.Contains(s.Failure, "no such file") {
		t.Fatalf("caller-facing failure leaks host detail: %q", s.Failure)
	}
	if !strings.Contains(s.Diagnostic, "secret-location") {
		t.Fatalf("the operator's diagnostic lost the detail it needs: %q", s.Diagnostic)
	}
}
