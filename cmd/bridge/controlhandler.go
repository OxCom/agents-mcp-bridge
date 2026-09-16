package main

import (
	"fmt"
	"sort"

	"github.com/oxcom/agents-mcp-bridge/internal/audit"
	"github.com/oxcom/agents-mcp-bridge/internal/policy"
	"github.com/oxcom/agents-mcp-bridge/internal/run"
	"github.com/oxcom/agents-mcp-bridge/internal/worktree"

	"github.com/oxcom/agents-mcp-bridge/internal/control"
)

// The bridge answers the operator's control channel. These are the only
// operations reachable from it, and none of them is exposed over MCP: the model
// cannot enumerate runs it did not start, and cannot stop anyone else's.

func (b *bridge) Status() control.Status {
	adapters := make([]string, 0, len(b.cfg.Agents))
	for id := range b.cfg.Agents {
		adapters = append(adapters, id)
	}
	sort.Strings(adapters)

	var features []string
	for _, name := range []string{
		"stream", "watch", "interactive", "operator_steering",
		"agent_steering", "sessions", "audit", "progress_notifications",
	} {
		if b.engine.FeatureEnabled(name) {
			features = append(features, name)
		}
	}
	return control.Status{
		PID:               processID(),
		Host:              b.host,
		Depth:             b.depth,
		Adapters:          adapters,
		TranscriptDir:     b.transcriptDir,
		Features:          features,
		DisabledAtRuntime: b.engine.Toggles().Snapshot(),
	}
}

func (b *bridge) Runs() []control.RunInfo {
	snaps := b.runs.List()
	out := make([]control.RunInfo, 0, len(snaps))
	for _, s := range snaps {
		info := control.RunInfo{
			RunID:      s.ID,
			Agent:      s.Agent,
			State:      string(s.State),
			Mode:       s.Mode,
			Confined:   s.Confined,
			Sandboxed:  s.SandboxEnforced,
			Started:    s.Started,
			Duration:   s.Duration,
			Transcript: s.Transcript,
			Awaited:    s.Awaited,
		}
		if s.Question != nil {
			info.QuestionID = s.Question.ID
			info.QuestionText = s.Question.Text
			info.QuestionOptions = s.Question.Options
		}
		info.SupersededBy = s.SupersededBy
		out = append(out, info)
	}
	return out
}

func (b *bridge) Stop(runID string) error {
	r, err := b.runs.Get(runID)
	if err != nil {
		return fmt.Errorf("no such run")
	}
	r.Cancel()
	return nil
}

// Accept applies a confined write run's diff, from the operator's side.
func (b *bridge) Accept(runID string) error {
	if r, err := b.runs.Get(runID); err == nil {
		// Same guard as getChanges/acceptChanges: refusing a superseded
		// chain link is a property of the state machine, not of
		// changeStore.discard having already run (wave-4 review, Important).
		if s := r.Snapshot(); s.State == run.StateSuperseded {
			return &SupersededError{RunID: s.ID, SuccessorID: s.SupersededBy}
		}
	}
	p, ok := b.changes.get(runID)
	if !ok {
		return fmt.Errorf("no pending changes for run %s", runID)
	}
	if err := worktree.Apply(p.wt.RepoRoot, p.patch); err != nil {
		return fmt.Errorf("the changes no longer apply cleanly and were not written: %w", err)
	}
	_ = b.audit.Write(audit.Entry{
		Event: "changes.accepted", RunID: runID,
		Message: fmt.Sprintf("%d file(s) applied by the operator", len(p.files)),
	})
	return b.changes.discard(runID)
}

// Reject discards a confined write run's changes and removes its worktree.
func (b *bridge) Reject(runID string) error {
	if _, ok := b.changes.get(runID); !ok {
		return fmt.Errorf("no pending changes for run %s", runID)
	}
	_ = b.audit.Write(audit.Entry{Event: "changes.rejected", RunID: runID, Message: "discarded by the operator"})
	return b.changes.discard(runID)
}

// Steer injects operator guidance into a live run. The operator is never
// capped: a human redirecting their own agent is the behaviour this tool
// exists to support.
func (b *bridge) Steer(runID, text string) error {
	r, err := b.runs.Get(runID)
	if err != nil {
		return fmt.Errorf("no such run")
	}
	if err := r.Steer(text, run.OriginOperator, -1); err != nil {
		return err
	}
	_ = b.audit.Write(audit.Entry{
		Event: "steer", RunID: runID, SteerOrigin: string(run.OriginOperator),
		SteerCount: r.Snapshot().SteerCount,
	})
	return nil
}

// Answer responds to a question the agent is waiting on. questionID, when
// non-empty, must be the question the operator actually saw: run.Run refuses
// with ErrQuestionChanged rather than delivering a stale answer to whatever
// replaced it.
//
// The gate resolver's audit sink (auditQuestion, gateconfig.go) writes the
// question.answered entry once the verdict this unblocks reaches it — that
// sink fires uniformly for every answer route, not just this one, so writing
// a second entry here would double-count every operator answer.
func (b *bridge) Answer(runID, questionID, text string) error {
	r, err := b.runs.Get(runID)
	if err != nil {
		return fmt.Errorf("no such run")
	}
	return r.AnswerQuestion(questionID, text)
}

// Continue creates a continuation successor for a run resting in needs_input.
// See cmd/bridge/continuation.go for the seed construction and the
// predecessor-retirement transaction (docs/02 §2.3a).
func (b *bridge) Continue(runID, text string) (string, error) {
	return b.continueRun(runID, text)
}

// SetFeature narrows or restores a feature on this live server.
//
// It can never exceed the config ceiling: FeatureEnabled consults the config
// first, so "enable" only clears a runtime override. No MCP tool reaches this.
func (b *bridge) SetFeature(feature string, on bool) error {
	if !policy.KnownFeature(feature) {
		return fmt.Errorf("unknown feature %q", feature)
	}
	// The ceiling is checked BEFORE the toggle moves. Enabling first and
	// checking afterwards left a window where a concurrent disable turned a
	// real refusal into a stale success, and it briefly cleared an override
	// the config never permitted lifting.
	if on {
		if !b.cfg.Features.Enabled(feature) {
			return fmt.Errorf("%q is disabled in the config file; a runtime toggle cannot widen it", feature)
		}
		b.engine.Toggles().Enable(feature)
	} else {
		b.engine.Toggles().Disable(feature)
	}
	_ = b.audit.Write(audit.Entry{
		Event: "toggle", Message: fmt.Sprintf("%s set to %v by the operator", feature, on),
	})
	return nil
}

func processID() int { return osGetpid() }
