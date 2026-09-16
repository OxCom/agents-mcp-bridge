// Package run owns the lifecycle of a delegated execution: spawn, bound, cap,
// reap. It is the only package that starts a process.
//
// The state machine and its invariants are in docs/11-domain-model.md §3.
package run

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// State is where a run is. Terminal states are the ones with no live child.
type State string

const (
	StateRunning    State = "running"
	StateCompleted  State = "completed"
	StateFailed     State = "failed"
	StateCancelled  State = "cancelled"
	StateRefused    State = "refused"
	StateNeedsInput State = "needs_input"
	// StateSuperseded is a predecessor that has been continued: a successor
	// run now carries the chain forward. Unlike StateNeedsInput it IS
	// terminal, so it frees its max_concurrent_runs slot immediately and
	// becomes prunable on the normal retention clock (docs/11 §3).
	StateSuperseded State = "superseded"
)

// IsTerminal reports whether the run has finished. Invariant 1: a run in a
// terminal state has no live child process.
func (s State) IsTerminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateCancelled || s == StateRefused || s == StateSuperseded
}

// Run is one delegated execution.
type Run struct {
	ID              string
	Agent           string
	HostAgent       string
	CWD             string
	Mode            string
	Confined        bool
	SandboxEnforced bool
	Depth           int
	Started         time.Time
	Finished        time.Time
	PromptBytes     int
	TranscriptPath  string
	// ResumedFrom is the predecessor's bridge-issued run id when this run is
	// a continuation's successor, set once at admission. Empty for an
	// ordinary run. It is a run id, never a vendor session id — the vendor
	// session never leaves the bridge (Snapshot.VendorSession).
	ResumedFrom string

	mu    sync.Mutex
	state State
	// retiring marks a predecessor whose continuation successor has already
	// been admitted against its slot. It stops counting as live from that
	// instant, rather than from the moment Supersede lands a few microseconds
	// later, so the live set never exceeds max_concurrent_runs. It is
	// accounting only: no caller-facing state, snapshot or transition reads
	// it, and Supersede still performs the real terminal transition.
	retiring           bool
	output             string
	failure            string // caller-facing: generic, leaks nothing about the host
	diag               string // operator-facing: full detail, goes to the log and audit
	exitCode           *int
	truncated          bool
	done               chan struct{}
	cancel             func()
	vendorSession      string
	transcriptOverflow bool
	worktree           any
	steerCh            *steerChannel
	// live is the output buffer while the run is in flight. Reading it means a
	// caller polling a long run sees partial progress instead of silence.
	live        *capped
	steerCount  int
	agentSteers int
	// pending is the question the child is currently blocked on, if any.
	// verdict is the channel Ask returned; resolve delivers to it and clears
	// both.
	pending *Question
	verdict chan Verdict
	// lastQuestion retains the most recently asked question after pending is
	// cleared, so the caller who reads a needs_input Snapshot after NeedsInput
	// released the waiter can still see what was asked. pending answers "is
	// anyone still blocked"; lastQuestion answers "what was last asked" and
	// the two are read differently on purpose — see snapshot().
	lastQuestion *Question
	// deadline is the wall-clock time a StateNeedsInput rest state must be
	// converted to StateFailed if still unanswered (docs/11 §3 invariant 3),
	// taken from the question's own Deadline when NeedsInput fires. Zero
	// means no deadline — a NewForTest run built with no Question.Deadline
	// set rests in needs_input indefinitely, matching pre-C3 behaviour.
	deadline time.Time
	// awaited records whether anyone ever collected this run. A run nobody
	// awaits is a real failure mode: the caller fires and forgets, and the
	// bridge burns tokens on a result no one reads.
	awaited bool
	// doneClosed guards against closing r.done twice: NeedsInput closes it
	// directly (a needs_input run has no live child left to trigger finish
	// promptly, and a NewForTest run has no supervise goroutine at all), and
	// finish would otherwise try to close it again for a real run whose child
	// exits after NeedsInput already fired.
	doneClosed bool
	// resumedBy is the successor's run id, set once by Supersede. Empty
	// unless state is StateSuperseded.
	resumedBy string
	// continuation holds what a successor's seed needs, attached at
	// admission (Spec.Continuation) and consumed exactly once by
	// TakeContinuation. It is never persisted to disk and does not survive a
	// bridge restart, by design (docs/02 §2.3a): prompt bodies at rest would be a
	// new data-retention commitment in a project that audits digests, not
	// bodies.
	continuation *ContinuationRecord
	// transition, when set, is told about a state change that has no other
	// audit sink in reach — today only the needs_input -> failed expiry
	// (docs/11 §3 invariant 5: every transition is one audit entry). It is a
	// plain callback rather than an import of internal/audit: run precedes
	// audit in the package order (see CLAUDE.md), so run must not depend on
	// it. Set once at Start() from Registry.transition; never called while
	// r.mu is held, and never called while Registry.mu is held either — see
	// Registry.sweepExpired. agent is r.Agent, passed by value rather than
	// looked up back through the registry, so the callback never has to call
	// back into Registry (which would otherwise risk the callback racing or
	// recursing through the very lock it was dispatched outside of).
	transition func(runID, agent, event, detail string)
}

// Snapshot is an immutable view, safe to hand out.
type Snapshot struct {
	ID         string
	Agent      string
	State      State
	Output     string
	Failure    string // safe to return to an untrusted caller
	Diagnostic string // for the operator's log and the audit record only
	ExitCode   *int
	Truncated  bool
	Started    time.Time
	Finished   time.Time
	Duration   time.Duration
	Awaited    bool
	Confined   bool
	// SandboxEnforced false means the vendor enforces nothing for this mode.
	SandboxEnforced bool
	Mode            string
	CWD             string
	// VendorSession is the id the target agent generated. It stays inside the
	// bridge: the caller receives an opaque handle instead.
	VendorSession string
	SteerCount    int
	AgentSteers   int
	Question      *Question
	// Transcript is the on-disk event log. Operator-facing only; returning it
	// to the caller would let it read the raw, un-enveloped stream.
	Transcript string
	// ResumedFrom is the predecessor's run id when this run continues a
	// chain; empty for an ordinary run.
	ResumedFrom string
	// SupersededBy is the successor's run id once State is StateSuperseded;
	// empty otherwise. list_runs and bridge runs surface it as
	// superseded_by (docs/02 §2.3a).
	SupersededBy string
}

func (r *Run) snapshot() Snapshot {
	output := r.output
	if !r.state.IsTerminal() && r.live != nil {
		output = r.live.String()
	}
	d := time.Since(r.Started)
	if !r.Finished.IsZero() {
		d = r.Finished.Sub(r.Started)
	}
	var q *Question
	switch {
	case r.pending != nil:
		cp := *r.pending
		q = &cp
	case r.state == StateNeedsInput && r.lastQuestion != nil:
		// The waiter was already released, so nothing is pending, but the
		// caller reading a needs_input Snapshot still needs to see what was
		// asked.
		cp := *r.lastQuestion
		q = &cp
	}
	return Snapshot{
		ID:              r.ID,
		Agent:           r.Agent,
		State:           r.state,
		Output:          output,
		Failure:         r.failure,
		Diagnostic:      r.diag,
		ExitCode:        r.exitCode,
		Truncated:       r.truncated,
		Started:         r.Started,
		Finished:        r.Finished,
		Duration:        d,
		Awaited:         r.awaited,
		Confined:        r.Confined,
		SandboxEnforced: r.SandboxEnforced,
		VendorSession:   r.vendorSession,
		SteerCount:      r.steerCount,
		AgentSteers:     r.agentSteers,
		Question:        q,
		Transcript:      r.TranscriptPath,
		Mode:            r.Mode,
		CWD:             r.CWD,
		ResumedFrom:     r.ResumedFrom,
		SupersededBy:    r.resumedBy,
	}
}

// setVendorSession records the id the vendor generated for this conversation.
// It is never handed to the caller: an opaque bridge handle is.
func (r *Run) setVendorSession(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.vendorSession == "" {
		r.vendorSession = id
	}
}

// TakeWorktree returns the run's worktree exactly once, so two callers cannot
// both try to remove it.
func (r *Run) TakeWorktree() any {
	r.mu.Lock()
	defer r.mu.Unlock()
	wt := r.worktree
	r.worktree = nil
	return wt
}

// Snapshot returns the current view of the run.
func (r *Run) Snapshot() Snapshot {
	r.expireIfDue()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshot()
}

// peekState reads state and Finished without applying the needs_input
// expiry transition or dispatching its audit callback (contrast
// expireIfDue). It is the only run accessor Registry.liveCountLocked and
// Registry.pruneLocked may call while holding Registry.mu: expiry does disk
// I/O through the transition callback (item 1), and that must never happen
// with Registry.mu held, so those two call sites see state as of the last
// time something (Start/Get/List, via sweepExpired) actually ran expiry,
// not a live check.
func (r *Run) peekState() (State, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state, r.Finished
}

// countsAsLive reports whether this run occupies a concurrency slot. A
// retiring predecessor does not: its slot has already been handed to the
// successor admitted in its place.
func (r *Run) countsAsLive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.state.IsTerminal() && !r.retiring
}

// retire marks the run as no longer occupying a slot. Start calls it under
// Registry.mu, in the same critical section that admits the successor, which
// is what makes the handover atomic.
func (r *Run) retire() {
	r.mu.Lock()
	r.retiring = true
	r.mu.Unlock()
}

// expireIfDue converts a needs_input run whose deadline has passed into
// StateFailed, releases any stranded verdict waiter, and — unlike the
// pre-C-item-1 version of this logic — dispatches the transition audit
// callback itself, entirely after releasing r.mu. It never holds r.mu (or
// any lock) while doing the send/callback, which is what lets
// Registry.sweepExpired call it once Registry.mu has already been released.
func (r *Run) expireIfDue() {
	r.mu.Lock()
	pendingCh, reason, expired := r.expireIfPastDeadlineLocked()
	transition := r.transition
	id, agent := r.ID, r.Agent
	r.mu.Unlock()

	// Resolving the stranded verdict and reporting the transition both
	// happen after unlocking: never send on a channel or do I/O while
	// holding r.mu.
	if pendingCh != nil {
		pendingCh <- Verdict{Deny: true, Message: reason}
		close(pendingCh)
	}
	if expired && transition != nil {
		transition(id, agent, "run.expired", reason)
	}
}

// expireIfPastDeadlineLocked converts a needs_input run whose deadline has
// passed into StateFailed (C3), so it stops resting on a concurrency slot
// forever (Registry.liveCountLocked reads state through Snapshot). Callers
// must hold r.mu. A zero deadline (no Question.Deadline was ever set) never
// expires.
//
// Ask now refuses on a needs_input run (see question.go), so r.pending
// should already be nil here on every real path; this still resolves it
// defensively — belt and suspenders against ever stranding a waiter through
// this transition (Important 2) — and reports pendingCh/pendingMsg for the
// caller to deliver and audit after unlocking.
func (r *Run) expireIfPastDeadlineLocked() (pendingCh chan Verdict, reason string, expired bool) {
	if r.state != StateNeedsInput || r.deadline.IsZero() || !time.Now().After(r.deadline) {
		return nil, "", false
	}
	r.state = StateFailed
	r.failure = "no operator answered before the question's deadline"
	r.diag = r.failure
	r.Finished = time.Now()
	if r.pending != nil && r.verdict != nil {
		pendingCh = r.verdict
		r.pending = nil
		r.verdict = nil
	}
	return pendingCh, r.failure, true
}

// Await blocks for at most timeout, returning as soon as the run finishes.
// It is re-callable: a caller may poll a long run in bounded slices.
func (r *Run) Await(timeout time.Duration) Snapshot {
	r.mu.Lock()
	r.awaited = true
	done := r.done
	r.mu.Unlock()

	select {
	case <-done:
	case <-time.After(timeout):
	}
	return r.Snapshot()
}

// Cancel stops the run. It is idempotent.
func (r *Run) Cancel() {
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (r *Run) finish(state State, output, failure, diag string, exitCode *int, truncated bool) {
	r.mu.Lock()
	if r.state.IsTerminal() || r.state == StateNeedsInput {
		// First settled state wins: a timeout racing an exit must not flip it,
		// and neither may a killed child's belated exit flip needs_input back
		// to cancelled — the state is non-terminal (IsTerminal stays false so
		// the run stays visible to the operator, with no live child) but must be
		// just as sticky here.
		r.mu.Unlock()
		return
	}
	r.state = state
	r.output = output
	r.failure = failure
	r.diag = diag
	r.exitCode = exitCode
	r.truncated = truncated
	r.Finished = time.Now()
	if !r.doneClosed {
		r.doneClosed = true
		close(r.done)
	}
	// A question can still be pending here: the child crashed, was killed by
	// a timeout, or the run was cancelled while the resolver's Ask() verdict
	// channel was still open. Terminal means no live child, so nothing else
	// will ever resolve it — release it now rather than stranding whatever
	// goroutine holds the other end (Important 2's finish() half). Cleared in
	// the same critical section as the state transition, matching NeedsInput.
	pending := r.pending
	ch := r.verdict
	r.pending = nil
	r.verdict = nil
	r.mu.Unlock()
	if pending != nil && ch != nil {
		ch <- Verdict{Deny: true, Message: failure}
		close(ch)
	}
}

var (
	// ErrUnknownRun is returned for a run id the registry never issued, or one
	// whose retention window has passed.
	ErrUnknownRun = errors.New("unknown run")
	// ErrConcurrencyLimit is returned when too many runs are already live.
	ErrConcurrencyLimit = errors.New("too many concurrent runs")
)

// Registry holds live and recently finished runs.
//
// A finished run stays resolvable for the retention window so a late Await
// returns the result rather than an error (invariant 4).
type Registry struct {
	mu        sync.Mutex
	runs      map[string]*Run
	maxLive   int
	retention time.Duration
	seq       int
	// transition is handed to every Run this registry starts (see
	// exec.go:Start), so a state change with no other audit sink in reach —
	// today only the needs_input -> failed expiry — still produces one audit
	// entry (docs/11 §3 invariant 5). Set with OnTransition; nil is a valid,
	// silent default for callers (tests, NewForTest) that never wire audit.
	transition func(runID, agent, event, detail string)
	// started is set the first time Start mints a run. OnTransition refuses
	// once this is true (see OnTransition): wiring audit after a run has
	// already started means that run's expiry would silently go unaudited,
	// which docs/11 §3 invariant 5 treats as a hard requirement rather than a
	// best-effort one, so the ordering mistake panics instead of passing
	// silently.
	started bool
}

// NewRegistry creates a registry bounded by maxLive concurrent runs.
func NewRegistry(maxLive int, retention time.Duration) *Registry {
	if maxLive <= 0 {
		maxLive = 4
	}
	if retention <= 0 {
		retention = time.Hour
	}
	return &Registry{runs: make(map[string]*Run), maxLive: maxLive, retention: retention}
}

// RegisterForTest inserts a Run directly into the registry, bypassing
// Start's admission check entirely, and wires it to the registry's
// transition callback the way Start does. It exists so a test in another
// package can construct an artificial concurrency condition: given
// liveCountExcludingLocked's exclusion, the ordinary Start path can never by
// itself leave more than maxLive-1 OTHER runs live alongside a given
// predecessor, so a test that wants Start to refuse a continuation's own
// admission needs a run whose presence did not come through that guarantee.
// Production code never calls this.
func (reg *Registry) RegisterForTest(r *Run) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	r.transition = reg.transition
	reg.runs[r.ID] = r
}

// OnTransition wires a callback for state changes a Run cannot audit itself
// (see Registry.transition). It must be called before Start, once, at
// server startup — cmd/bridge wires it to b.audit. Calling it after any run
// has already started panics rather than silently discarding the mistake
// (item 2): that ordering is exactly the one that leaves an already-started
// run's needs_input -> failed expiry unaudited, and this codebase treats
// that class of setup error as a load error, not a warning.
func (reg *Registry) OnTransition(fn func(runID, agent, event, detail string)) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if reg.started {
		panic("run: OnTransition called after a run already started; wire it before the first Start")
	}
	reg.transition = fn
}

// Get returns a run by id.
func (reg *Registry) Get(id string) (*Run, error) {
	reg.sweepExpired()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.pruneLocked()
	r, ok := reg.runs[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownRun, id)
	}
	return r, nil
}

// List returns a snapshot of every known run, newest first.
func (reg *Registry) List() []Snapshot {
	reg.sweepExpired()
	reg.mu.Lock()
	reg.pruneLocked()
	runs := make([]*Run, 0, len(reg.runs))
	for _, r := range reg.runs {
		runs = append(runs, r)
	}
	reg.mu.Unlock()

	// Snapshot is built outside reg.mu: it no longer does I/O (expiry already
	// ran, above, in sweepExpired), but nothing here needs the registry lock
	// held while it copies a Run's fields under r.mu either.
	out := make([]Snapshot, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.Snapshot())
	}
	return out
}

// sweepExpired converts any needs_input run whose deadline has passed into
// failed and dispatches its audit entry, entirely outside Registry.mu (item
// 1): it copies the run pointers under the lock, releases it, then calls
// Run.expireIfDue on each — the method that does the actual I/O. Get, List
// and Start all call this before touching Registry.mu, so a slow or full
// disk stalls one expiry check, never every concurrent run start or list
// behind the registry lock.
func (reg *Registry) sweepExpired() {
	reg.mu.Lock()
	runs := make([]*Run, 0, len(reg.runs))
	for _, r := range reg.runs {
		runs = append(runs, r)
	}
	reg.mu.Unlock()
	for _, r := range runs {
		r.expireIfDue()
	}
}

// Abandoned returns finished runs nobody ever awaited, so the server can remind
// the calling model that results are waiting.
func (reg *Registry) Abandoned() []Snapshot {
	var out []Snapshot
	for _, s := range reg.List() {
		if s.State.IsTerminal() && !s.Awaited {
			out = append(out, s)
		}
	}
	return out
}

// CancelAll stops every live run. No run outlives its server.
func (reg *Registry) CancelAll() {
	reg.mu.Lock()
	live := make([]*Run, 0, len(reg.runs))
	for _, r := range reg.runs {
		live = append(live, r)
	}
	reg.mu.Unlock()
	for _, r := range live {
		r.Cancel()
	}
}

// pruneLocked and liveCountLocked both run with Registry.mu held (item 1),
// so they read state through peekState, never Snapshot: Snapshot may run the
// needs_input expiry transition, whose audit callback does disk I/O, and
// that must never happen while Registry.mu is held. Callers that need expiry
// applied first call Registry.sweepExpired before taking the lock, so by the
// time these run, any run that should already be failed already is.
//
// liveCountLocked errs high, never low: a needs_input run that crosses its
// deadline after sweepExpired returns but before this runs still counts,
// until the next sweep that the next Start, Get or List triggers. That
// refuses one admission it could have allowed, which is the safe direction.
// The other way round — admitting past the cap — was possible while a
// continuation's predecessor stayed live between Start and Supersede, and is
// now closed by Run.retiring (see Start).
func (reg *Registry) pruneLocked() {
	cutoff := time.Now().Add(-reg.retention)
	for id, r := range reg.runs {
		state, finished := r.peekState()
		if state.IsTerminal() && finished.Before(cutoff) {
			delete(reg.runs, id)
		}
	}
}

func (reg *Registry) liveCountLocked() int {
	return reg.liveCountExcludingLocked("")
}

// liveCountExcludingLocked is liveCountLocked but never counts the run with
// the given id (an empty id excludes nothing). Start passes spec.ResumedFrom
// here so a continuation's successor is admitted against the room its
// predecessor's own needs_input rest state already occupies, instead of
// requiring that slot freed in advance by retiring the predecessor first.
// That is what lets Run.Supersede run AFTER Start succeeds rather than
// before it (docs/02 §2.3a; cmd/bridge/continuation.go): the predecessor's slot is never actually
// vacated by this exclusion, only treated as available to its own successor,
// so nothing here can be used to admit more than one extra run per
// predecessor — every other non-terminal run still counts normally.
func (reg *Registry) liveCountExcludingLocked(excludeID string) int {
	n := 0
	for id, r := range reg.runs {
		if excludeID != "" && id == excludeID {
			continue
		}
		if r.countsAsLive() {
			n++
		}
	}
	return n
}
