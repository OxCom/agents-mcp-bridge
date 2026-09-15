package run

import (
	"errors"
	"time"
)

// QATurn is one question-answer pair in a continuation chain, in the order it
// happened.
type QATurn struct {
	Question Question
	Answer   string
}

// ContinuationRecord holds exactly what a successor's seed needs and nothing
// else: the original prompt, the adapter id, cwd, mode, sandbox selection,
// and the ordered (question, answer) pairs of the chain so far. It lives
// exactly as long as the needs_input window and is discarded with the run —
// see the doc comment on Run.continuation for why it is never persisted.
//
// Depth is how many links this chain has already used to reach the run
// carrying this record: 0 for an original run, 1 for its first successor,
// and so on. CheckDepth refuses before that count would exceed the
// configured ceiling.
type ContinuationRecord struct {
	Prompt   string
	Agent    string
	CWD      string
	Mode     string
	Confined bool
	// SandboxSelection is the sandbox flag key the original run resolved
	// (config.Mode as a string, e.g. "read-only" or "write"), carried so the
	// successor's seed can be built without re-deriving policy.
	SandboxSelection string
	Turns            []QATurn
	Depth            int
}

var (
	// ErrNotAwaitingInput is returned by Supersede and TakeContinuation when
	// the run is not resting in needs_input. Only a run resting there may be
	// continued or superseded — the design's one non-terminal resting state.
	ErrNotAwaitingInput = errors.New("this run is not resting in needs_input; it cannot be continued")
	// ErrContinuationUnavailable is returned when a run's continuation record
	// is gone: never attached, already consumed by an earlier continuation
	// attempt, or lost to a bridge restart (continuation records are never
	// persisted). The caller must treat this as "this run can no longer be
	// continued", never retry the same record.
	ErrContinuationUnavailable = errors.New("this run can no longer be continued: its continuation record is gone")
	// ErrChainTooDeep is returned when applying an answer would push a chain
	// past defaults.max_continuations, so an agent that keeps re-asking the
	// same question cannot loop forever.
	ErrChainTooDeep = errors.New("this continuation chain has reached its configured limit")
)

// SetContinuation attaches the record that would let this run be continued
// after a needs_input stop. The run manager sets this once at admission
// (Spec.Continuation), before Start returns, so nothing can observe the run
// without it already in place.
func (r *Run) SetContinuation(rec *ContinuationRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.continuation = rec
}

// TakeContinuation returns the run's continuation record exactly once, and
// only while the run rests in needs_input — the same "consume once" pattern
// as TakeWorktree, so a second concurrent continuation attempt on the same
// run fails instead of silently reusing (and duplicating) the same seed.
func (r *Run) TakeContinuation() (*ContinuationRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != StateNeedsInput {
		return nil, ErrNotAwaitingInput
	}
	if r.continuation == nil {
		return nil, ErrContinuationUnavailable
	}
	rec := r.continuation
	r.continuation = nil
	return rec, nil
}

// CheckDepth refuses a continuation that would push the chain past
// maxContinuations. rec.Depth is how many links already led to this run, so
// the successor being built would sit at rec.Depth+1.
func (rec *ContinuationRecord) CheckDepth(maxContinuations int) error {
	if rec.Depth+1 > maxContinuations {
		return ErrChainTooDeep
	}
	return nil
}

// Supersede transitions a run resting in needs_input to the terminal
// StateSuperseded, carrying the successor's run id. It is terminal —
// unlike StateNeedsInput — so it frees its max_concurrent_runs slot
// immediately (IsTerminal) and becomes prunable on the normal retention
// clock, matching finish()'s "first settled state wins" discipline: calling
// Supersede on anything but a needs_input run refuses with
// ErrNotAwaitingInput rather than silently overwriting whatever state
// already settled there.
//
// It clears any surviving continuation record (a superseded run cannot be
// continued a second time; its own successor already exists) and emits
// exactly one audit transition, run.superseded, carrying the successor id —
// docs/superpowers/specs/2026-09-15-continuation-design.md §8 and docs/11
// §3 invariant 5 (every transition is one audit entry).
func (r *Run) Supersede(successorID string) error {
	r.mu.Lock()
	if r.state != StateNeedsInput {
		r.mu.Unlock()
		return ErrNotAwaitingInput
	}
	r.state = StateSuperseded
	r.resumedBy = successorID
	r.continuation = nil
	r.Finished = time.Now()
	if !r.doneClosed {
		// Defensive: NeedsInput already closes r.done, so this run should
		// already have doneClosed true by the time it can be superseded.
		// Guarded the same way finish() guards it, in case that invariant
		// ever changes.
		r.doneClosed = true
		close(r.done)
	}
	transition := r.transition
	id, agent := r.ID, r.Agent
	r.mu.Unlock()

	if transition != nil {
		transition(id, agent, "run.superseded", successorID)
	}
	return nil
}
