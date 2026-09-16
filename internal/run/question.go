package run

import (
	"errors"
	"strings"
	"time"
)

// NewForTest builds a Run with no live process, for state-machine tests that
// need a *Run without spawning a child. Exported because internal/gate's
// tests need it too (Task 6). It starts no process: the state machine is
// testable without one.
func NewForTest(id string) *Run {
	return &Run{ID: id, Agent: "test", Started: time.Now(), state: StateRunning, done: make(chan struct{})}
}

// Question is one blocking request from the child, correlated by the vendor's
// own tool-use id. Text is sanitized by the caller BEFORE it gets here.
type Question struct {
	// ID is the bridge's own id for this question (I2): a child's own
	// tool_use_id is never trusted as the identity used for matching an
	// answer, because an empty value would reach resolve's "answer whatever
	// is pending" branch, which is reserved for the operator's blind
	// `bridge answer` CLI.
	ID string
	// ChildQuestionID is the vendor's own tool_use_id, carried alongside ID
	// purely to correlate this question back to the raw vendor event in
	// logs. Never used for identity or matching.
	ChildQuestionID string
	Tool            string
	Text            string
	Options         []string
	Asked           time.Time
	Deadline        time.Time
}

// Verdict is what the gate returns to the child. Spike C7: a question is
// answered with a denial carrying the answer; "allow" discards it.
type Verdict struct {
	Answer  string
	Deny    bool
	Message string
}

var (
	// ErrQuestionPending is returned when a run already holds an unanswered
	// question. One run asks one question at a time, so a second Ask is
	// refused rather than queued or allowed to displace the first.
	ErrQuestionPending = errors.New("a question is already pending for this run")
	// ErrNoQuestion is returned when an answer arrives for a run that holds
	// no pending question at all, which is what separates it from
	// ErrQuestionChanged.
	ErrNoQuestion = errors.New("this run is not waiting on a question")
	// ErrQuestionChanged is returned when an answer names a question id that
	// is no longer the one pending — resolved another way (timeout, CLI,
	// another TUI) and, in the dangerous case, already replaced by a new
	// question. It is distinct from ErrNoQuestion: the run IS waiting on
	// something, just not on what the caller thinks it answered.
	ErrQuestionChanged = errors.New("this is not the question that is currently pending")
)

// Ask records a pending question and returns the channel its verdict arrives on.
func (r *Run) Ask(q Question) (<-chan Verdict, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// needs_input is deliberately non-terminal (IsTerminal is false), but it
	// is still a rested state with no live child: admitting a second
	// question here would let expireIfPastDeadlineLocked later flip the run
	// to StateFailed while a verdict channel sits unresolved, stranding a
	// delegated child for a further full question_timeout_s (Important 2).
	// Refuse here instead; the resolver already treats an Ask error as
	// fail-closed (internal/gate/resolver.go:82-85).
	if r.state.IsTerminal() || r.state == StateNeedsInput {
		return nil, ErrRunNotLive
	}
	if r.pending != nil {
		return nil, ErrQuestionPending
	}
	if q.Asked.IsZero() {
		q.Asked = time.Now()
	}
	ch := make(chan Verdict, 1)
	r.pending = &q
	r.lastQuestion = &q
	r.verdict = ch
	return ch, nil
}

// resolve delivers a verdict for whatever is pending, optionally scoped to a
// specific question id.
//
// wantID == "" resolves whatever is pending unconditionally — used where the
// caller unambiguously owns the question it is resolving (an internal
// timeout/cancellation, or an operator answering blind with no rendered
// question to bind to, e.g. the `bridge answer` CLI). A non-empty wantID that
// does not match r.pending.ID returns ErrQuestionChanged WITHOUT resolving:
// this is the operator-TUI path, where the answer was composed against a
// specific question on screen, and delivering it to whatever replaced that
// question — after a timeout, a CLI answer, or another TUI — would answer the
// wrong thing with text the operator never meant for it. The id check and the
// resolve happen under the same lock, so a concurrent Ask() for a new
// question can never land in the gap between them.
func (r *Run) resolve(wantID string, v Verdict) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		return ErrNoQuestion
	}
	if wantID != "" && r.pending.ID != wantID {
		return ErrQuestionChanged
	}
	r.pending = nil
	ch := r.verdict
	r.verdict = nil
	ch <- v
	close(ch)
	return nil
}

// AnswerQuestion delivers the operator's answer. Operator only.
//
// questionID is the id of the question the operator actually saw and
// answered; "" answers whatever is currently pending, for a caller with no
// specific question to bind to. A non-empty questionID that no longer matches
// what is pending returns ErrQuestionChanged rather than misdelivering the
// answer to a question that replaced it.
//
// The answer is operator-authored but still crosses into a vendor process, so
// it is capped and stripped of control characters: a terminal escape typed by
// accident must not travel into another agent's transcript.
func (r *Run) AnswerQuestion(questionID, text string) error {
	if len(text) > MaxSteerBytes {
		return ErrSteerTooLarge
	}
	return r.resolve(questionID, Verdict{Deny: true, Message: stripControl(text)})
}

// stripControl removes C0/C1 control characters, keeping tab and newline.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n':
			return r
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			return -1
		}
		return r
	}, s)
}

// DenyQuestion refuses without an answer, for an approval or a timeout.
//
// It always resolves whatever is pending (wantID ""): every caller today
// (internal/gate/resolver.go) denies the one question it just asked and owns
// synchronously, with no separate identity to bind to as AnswerQuestion's
// operator callers must.
func (r *Run) DenyQuestion(message string) error {
	return r.resolve("", Verdict{Deny: true, Message: message})
}

// PendingQuestion returns a copy of the question the child is blocked on.
func (r *Run) PendingQuestion() *Question {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		return nil
	}
	q := *r.pending
	return &q
}

// NeedsInput stops the run in the one non-terminal resting state. The child
// is killed and the pending question is cleared; there is no resume path —
// the caller gets the sanitized question back and the run id identifies the
// stopped run for the operator's own tooling (audit trail, `bridge runs`).
//
// It closes r.done itself rather than relying solely on cancel(): a
// NewForTest run has no supervise goroutine to call finish, and even for a
// real run, Await must not block until the killed child's process exit is
// reaped. finish() guards against the resulting double-close with doneClosed.
//
// NeedsInput is precisely the path taken when no operator channel was
// available to answer a pending question, so it must release any goroutine
// still blocked on the verdict channel Ask returned — otherwise that call
// leaks forever and holds a delegated agent's gate call open with it. It does
// this itself, rather than calling resolve, so that the pending question is
// cleared in the SAME critical section as the state transition: otherwise a
// concurrent PendingQuestion() could observe needs_input alongside a stale
// pending question in the gap before resolve reacquires the lock. The verdict
// send and channel close still happen after unlocking — never while holding
// r.mu. lastQuestion (set by Ask) is untouched here, so the caller reading a
// needs_input Snapshot afterwards can still see what was asked; see
// snapshot(). A run with nothing pending leaves pending/verdict nil, so the
// send below is skipped and NeedsInput stays idempotent and safe to call with
// no question in flight.
func (r *Run) NeedsInput(reason string) {
	r.mu.Lock()
	if r.state.IsTerminal() || r.state == StateNeedsInput {
		r.mu.Unlock()
		return
	}
	r.state = StateNeedsInput
	r.failure = reason
	// The needs_input rest period gets its own deadline, re-anchored to now
	// rather than reusing lastQuestion.Deadline verbatim (C3): every caller
	// in internal/gate/resolver.go reaches NeedsInput only after already
	// waiting out that same deadline on the TUI or elicitation channel, so
	// by the time NeedsInput runs, Question.Deadline has usually already
	// passed — reusing it as-is would fail the run the instant needs_input
	// began, leaving no resting period at all. The question still "carries"
	// the deadline in the sense that matters: its own Asked→Deadline span is
	// the duration (question_timeout_s) needs_input rests for, just measured
	// from the moment the run stops waiting for a human rather than from the
	// moment it started.
	if q := r.lastQuestion; q != nil {
		if dur := q.Deadline.Sub(q.Asked); dur > 0 {
			r.deadline = time.Now().Add(dur)
		}
	}
	if !r.doneClosed {
		r.doneClosed = true
		close(r.done)
	}
	cancel := r.cancel
	pending := r.pending
	ch := r.verdict
	r.pending = nil
	r.verdict = nil
	r.mu.Unlock()
	if pending != nil && ch != nil {
		ch <- Verdict{Deny: true, Message: reason}
		close(ch)
	}
	if cancel != nil {
		cancel()
	}
}
