package gate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/run"
)

// Channels are the ways a human can be reached, in the order docs/10 §1 fixes:
// the TUI owns questions; elicitation is legal only inside a live await_agent.
type Channels struct {
	TUIAttached func() bool
	Elicit      func(context.Context, run.Question) (string, bool)
	Timeout     time.Duration
}

const (
	// A question is answered with a denial carrying the text (spike C7).
	approvalDenied = "This run is supervised and cannot request permissions. " +
		"Do the part of the task that needs no approval, or explain what you needed."
	// No resume path exists: NeedsInput kills the child and clears the
	// pending question, and AnswerQuestion always returns ErrNoQuestion
	// afterwards. This text reaches the delegated agent and, from there, a
	// human reading its transcript, so it must not promise a capability the
	// bridge does not have.
	noHuman = "No operator is reachable. The run has stopped; report what you needed to know."
)

type resolver struct {
	lookup func() (*run.Run, error)
	ch     Channels
	clean  func(string) string
	audit  func(event, detail string)
}

// NewResolver builds a Resolver that looks up the run lazily: the gate starts
// before the run it serves exists. A lookup failure fails closed — it must
// never allow, and must never panic on a nil run.
func NewResolver(lookup func() (*run.Run, error), ch Channels, clean func(string) string, audit func(event, detail string)) Resolver {
	return &resolver{lookup: lookup, ch: ch, clean: clean, audit: audit}
}

func (x *resolver) Resolve(ctx context.Context, a Ask) Reply {
	// One channel carries both kinds (docs/12 C7). Only a question reaches a
	// human in v1; an approval is denied and recorded.
	if a.Tool != "AskUserQuestion" {
		x.audit("question.denied", a.Tool)
		return Reply{Behavior: "deny", Message: approvalDenied}
	}

	r, err := x.lookup()
	if err != nil || r == nil {
		detail := "no run"
		if err != nil {
			detail = err.Error()
		}
		x.audit("question.denied", detail)
		return Reply{Behavior: "deny", Message: noHuman}
	}

	q := run.Question{
		// ID is minted by the bridge, never trusted from the child (I2): the
		// wire id (a.ToolUseID) is the child's own, and run.resolve treats an
		// empty id as "answer whatever is pending" — a branch meant for the
		// operator-facing `bridge answer` CLI with nothing rendered to bind
		// to, not for a child that happens to send an empty tool_use_id.
		// ChildQuestionID carries the child's id alongside it, for
		// correlating this question back to the raw vendor event in logs.
		ID:              mintQuestionID(),
		ChildQuestionID: a.ToolUseID,
		Tool:            a.Tool,
		Text:            x.clean(questionText(a.Input)),
		Options:         x.cleanAll(questionOptions(a.Input)),
		Asked:           time.Now(),
		Deadline:        time.Now().Add(x.ch.Timeout),
	}
	verdicts, err := r.Ask(q)
	if err != nil {
		x.audit("question.denied", err.Error())
		return Reply{Behavior: "deny", Message: noHuman}
	}
	x.audit("question.asked", q.ID)

	if x.ch.TUIAttached != nil && x.ch.TUIAttached() {
		return x.awaitViaTUI(ctx, r, q, verdicts)
	}

	if x.ch.Elicit != nil {
		if answer, ok := x.elicitWithTimeout(ctx, q); ok {
			if err := r.AnswerQuestion(q.ID, answer); err != nil {
				x.audit("question.denied", err.Error())
			} else if v, ok := <-verdicts; ok {
				// The message that actually reached the run, not the local
				// `answer`: they coincide on the normal path, but reading
				// back the verdict is what proves delivery rather than
				// assuming it.
				x.audit("question.answered", q.ID)
				return Reply{Behavior: "deny", Message: v.Message}
			}
			// AnswerQuestion failed, or the channel was already closed by a
			// concurrent resolve with nothing on it: the elicited answer was
			// never delivered. Fall through to fail closed below rather than
			// reporting it as delivered.
		}
	}

	// Fail closed AT ONCE: waiting out the timeout for a human who was never
	// there burns the run's whole budget for nothing.
	_ = r.DenyQuestion(noHuman)
	r.NeedsInput("no operator channel was available to answer a question")
	x.audit("question.timeout", q.ID)
	return Reply{Behavior: "deny", Message: noHuman}
}

// elicitWithTimeout bounds x.ch.Elicit by x.ch.Timeout (C2). The context
// passed into Resolve is context.Background() with no deadline of its own —
// deliberately, so the TUI branch can let a human take minutes — so nothing
// upstream bounds a host that accepts elicitation/create and never answers.
// question_timeout_s must bound this channel exactly as it bounds the TUI
// wait.
//
// The call runs in its own goroutine rather than trusting Elicit to honour
// ctx: a host implementation that ignores cancellation would otherwise still
// hang the resolver forever. That goroutine can leak past the timeout if
// Elicit truly never returns, which is accepted: a leaked goroutine is a far
// smaller failure than a wedged gate call blocking the child, the run, and
// (via NeedsInput) a concurrency slot forever.
func (x *resolver) elicitWithTimeout(ctx context.Context, q run.Question) (string, bool) {
	ectx, cancel := context.WithTimeout(ctx, x.ch.Timeout)
	defer cancel()

	type result struct {
		answer string
		ok     bool
	}
	done := make(chan result, 1)
	go func() {
		answer, ok := x.ch.Elicit(ectx, q)
		done <- result{answer, ok}
	}()

	select {
	case res := <-done:
		return res.answer, res.ok
	case <-ectx.Done():
		return "", false
	}
}

// mintQuestionID returns an id the bridge chooses for a question (I2),
// rather than trusting the child's own tool_use_id.
func mintQuestionID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is effectively unrecoverable, but this must
		// still never panic or return "" — the empty id is reserved for
		// run.resolve's "answer whatever is pending" branch.
		return "q-" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	return "q-" + hex.EncodeToString(b)
}

// awaitViaTUI blocks for the operator's verdict while a TUI is attached.
func (x *resolver) awaitViaTUI(ctx context.Context, r *run.Run, q run.Question, verdicts <-chan run.Verdict) Reply {
	return x.awaitVerdict(ctx, r, q, verdicts, time.After(x.ch.Timeout))
}

// awaitVerdict is awaitViaTUI's body, with the timeout channel as a
// parameter so a test can hand it one that has already fired.
//
// It checks verdicts once, non-blocking, before the blocking select: Go's
// select picks a ready case at random, so without this priority check a real
// answer that lands on verdicts in the same instant the timeout elapses can
// lose to the timer, get discarded unread, and have the child told noHuman
// even though the operator answered in time.
func (x *resolver) awaitVerdict(ctx context.Context, r *run.Run, q run.Question, verdicts <-chan run.Verdict, timedOut <-chan time.Time) Reply {
	select {
	case v := <-verdicts:
		x.audit("question.answered", q.ID)
		return Reply{Behavior: "deny", Message: v.Message}
	default:
	}

	select {
	case v := <-verdicts:
		x.audit("question.answered", q.ID)
		return Reply{Behavior: "deny", Message: v.Message}
	case <-timedOut:
		if err := r.DenyQuestion(noHuman); err != nil {
			// Lost the race: a real answer resolved the question between the
			// priority check above and here. Deliver it, not noHuman — a
			// forced deny here would override an answer that already landed.
			v := <-verdicts
			x.audit("question.answered", q.ID)
			return Reply{Behavior: "deny", Message: v.Message}
		}
		x.audit("question.timeout", q.ID)
		r.NeedsInput("the operator did not answer in time")
		return Reply{Behavior: "deny", Message: noHuman}
	case <-ctx.Done():
		if err := r.DenyQuestion(noHuman); err != nil {
			v := <-verdicts
			x.audit("question.answered", q.ID)
			return Reply{Behavior: "deny", Message: v.Message}
		}
		// Unlike a plain timeout, a cancelled request has no path back to
		// the child at all: without this the run is left in StateRunning
		// with nothing pending and nothing to complete it. Audited like every
		// other needs_input transition here (docs/11 §3 invariant 5): a run
		// with no terminal audit entry is a fault.
		x.audit("question.timeout", q.ID+" cancelled")
		r.NeedsInput("the request was cancelled while waiting for an answer")
		return Reply{Behavior: "deny", Message: noHuman}
	}
}

func (x *resolver) cleanAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, x.clean(s))
	}
	return out
}

// questionText and questionOptions read the vendor's AskUserQuestion shape
// defensively: a missing field yields an empty string, never an error, because
// a vendor adding a field must not take the bridge down.
func questionText(raw json.RawMessage) string {
	var v struct {
		Questions []struct {
			Question string `json:"question"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || len(v.Questions) == 0 {
		return "the delegated agent asked a question the bridge could not read"
	}
	return v.Questions[0].Question
}

func questionOptions(raw json.RawMessage) []string {
	var v struct {
		Questions []struct {
			Options []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || len(v.Questions) == 0 {
		return nil
	}
	out := make([]string, 0, len(v.Questions[0].Options))
	for _, o := range v.Questions[0].Options {
		out = append(out, o.Label)
	}
	return out
}
