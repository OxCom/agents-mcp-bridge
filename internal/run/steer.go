package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Origin says who injected a steer. Operator steers are uncapped; agent steers
// are counted, because an agent that can redirect another agent without a human
// in the loop is the loop this project exists to keep bounded.
type Origin string

const (
	OriginOperator Origin = "operator"
	OriginAgent    Origin = "agent"
)

// MaxSteerBytes bounds one injected message. Guidance is a sentence or two;
// anything larger is a mistake or an attempt to push a payload through the
// channel, and both are better refused than buffered.
const MaxSteerBytes = 64 << 10

var (
	// ErrSteerTooLarge is returned for a message beyond MaxSteerBytes.
	ErrSteerTooLarge = errors.New("steer message is too large")
	// ErrSteerUnsupported is returned for an adapter with no injection channel.
	// It is a typed refusal, never a silent no-op: a caller that believes it
	// redirected an agent, and did not, is worse than one that got an error.
	ErrSteerUnsupported = errors.New("this agent has no steering channel")
	// ErrRunNotLive is returned once a run has finished.
	ErrRunNotLive = errors.New("run is not live")
	// ErrSteerCapReached is returned when agent-originated steers hit the cap.
	ErrSteerCapReached = errors.New("agent steer cap reached")
)

// steerChannel writes user messages into a live child's stdin.
//
// VERIFIED (docs/12-spike-results.md C2): a message written mid-turn is QUEUED
// and answered after the current turn ends. It does not interrupt. The adapter
// therefore declares `steer: queued`, and callers are told the same.
type steerChannel struct {
	mu     sync.Mutex
	w      io.WriteCloser
	closed bool
	// sent counts calls to send that got as far as attempting delivery. Tests
	// use it to prove a path never touches the steer channel at all.
	sent int
}

func (c *steerChannel) send(text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent++
	if c.closed || c.w == nil {
		return ErrRunNotLive
	}
	// Built by a JSON encoder from a typed value. A string template here would
	// let a message containing a quote inject arbitrary stream-json fields.
	rec := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode steer message: %w", err)
	}
	if _, err := c.w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("deliver steer message: %w", err)
	}
	return nil
}

// close ends the child's input.
//
// A stream-json child does NOT exit when its turn completes: it blocks on stdin
// EOF (VERIFIED, C1). Nothing closes it for us, so a run whose stdin is never
// closed hangs until its timeout.
func (c *steerChannel) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.w == nil {
		return
	}
	c.closed = true
	_ = c.w.Close()
}

// Steer injects guidance into a live run.
//
// Delivery is at the next turn boundary for every adapter shipped today; the
// caller is told that rather than left to assume an interruption.
func (r *Run) Steer(text string, origin Origin, agentCap int) error {
	if len(text) > MaxSteerBytes {
		return fmt.Errorf("%w: %d bytes, limit %d", ErrSteerTooLarge, len(text), MaxSteerBytes)
	}
	r.mu.Lock()
	if r.state.IsTerminal() {
		r.mu.Unlock()
		return ErrRunNotLive
	}
	if r.steerCh == nil {
		r.mu.Unlock()
		return ErrSteerUnsupported
	}
	// A pending question is the operator's to answer. Letting an agent satisfy
	// it through the steering channel would be automatic answering by the back
	// door, which docs/01-requirements.md §6 puts out of scope.
	if origin == OriginAgent && r.pending != nil {
		r.mu.Unlock()
		return fmt.Errorf("run has an unanswered question; only the operator can respond")
	}
	if origin == OriginAgent {
		if agentCap >= 0 && r.agentSteers >= agentCap {
			r.mu.Unlock()
			return ErrSteerCapReached
		}
		r.agentSteers++
	}
	ch := r.steerCh
	r.mu.Unlock()

	if err := ch.send(text); err != nil {
		return err
	}
	r.mu.Lock()
	r.steerCount++
	r.mu.Unlock()
	return nil
}

// closeSteer ends the child's input, if it has one.
func (r *Run) closeSteer() {
	r.mu.Lock()
	ch := r.steerCh
	r.mu.Unlock()
	if ch != nil {
		ch.close()
	}
}

// setDiagnostic records an operator-facing fault that does not by itself end
// the run.
func (r *Run) setDiagnostic(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.diag == "" {
		r.diag = msg
	}
}
