package main

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/oxcom/agents-mcp-bridge/internal/audit"
	"github.com/oxcom/agents-mcp-bridge/internal/run"
	"github.com/oxcom/agents-mcp-bridge/internal/sanitize"
	"github.com/oxcom/agents-mcp-bridge/internal/worktree"
)

// pendingChanges is a finished write run waiting for a decision.
type pendingChanges struct {
	wt       *worktree.Worktree
	patch    string
	files    []string
	accepted bool
}

// changeStore holds the diffs of confined write runs until they are accepted,
// rejected, or the run expires.
type changeStore struct {
	mu sync.Mutex
	m  map[string]*pendingChanges
}

func newChangeStore() *changeStore { return &changeStore{m: make(map[string]*pendingChanges)} }

func (s *changeStore) put(runID string, p *pendingChanges) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[runID] = p
}

func (s *changeStore) get(runID string) (*pendingChanges, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[runID]
	return p, ok
}

// discard removes a worktree and forgets the run. A worktree that survives its
// run is a reported fault, not a silent leak.
func (s *changeStore) discard(runID string) error {
	s.mu.Lock()
	p, ok := s.m[runID]
	delete(s.m, runID)
	s.mu.Unlock()
	if !ok {
		return nil
	}
	return p.wt.Remove()
}

// discardAll cleans up on shutdown.
func (s *changeStore) discardAll() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.m))
	for id := range s.m {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		_ = s.discard(id)
	}
}

// specWorktree adapts a Spec to the TakeWorktree interface, for the failure
// path where no Run was ever created.
type specWorktree struct{ spec run.Spec }

func (s *specWorktree) TakeWorktree() any { return s.spec.Worktree }

// reapAbandonedWorktrees removes worktrees belonging to runs that finished and
// that nobody collected within the retention window. Without this, a caller
// that fires and forgets leaves a checkout on disk until the server exits.
func (b *bridge) reapAbandonedWorktrees() {
	for _, s := range b.runs.List() {
		if !s.State.IsTerminal() {
			continue
		}
		if p, ok := b.changes.get(s.ID); ok && p.patch == "" && !p.accepted {
			// Terminal, registered at spawn, never collected: the run failed or
			// was cancelled before it could produce a diff.
			if s.State != run.StateCompleted {
				_ = b.changes.discard(s.ID)
			}
		}
	}
}

// collect runs after a confined write finishes: it captures the diff and holds
// it for a decision.
func (b *bridge) collect(runID string, wt *worktree.Worktree) {
	patch, err := wt.Diff()
	if err != nil {
		_ = b.audit.Write(audit.Entry{
			Event: "changes.diff_failed", RunID: runID, Message: err.Error(),
		})
		_ = wt.Remove()
		return
	}
	files, _ := wt.ChangedFiles()
	if patch == "" {
		// The agent changed nothing. Keeping an empty worktree pending would
		// ask the operator to accept a diff that does not exist.
		_ = b.changes.discard(runID)
		_ = b.audit.Write(audit.Entry{
			Event: "changes.none", RunID: runID, Message: "the agent made no changes",
		})
		return
	}
	b.changes.put(runID, &pendingChanges{wt: wt, patch: patch, files: files})
	_ = b.audit.Write(audit.Entry{
		Event: "changes.pending", RunID: runID,
		Message: fmt.Sprintf("%d file(s) changed, awaiting acceptance", len(files)),
	})
}

type changesOutput struct {
	RunID   string   `json:"run_id"`
	Status  string   `json:"status"`
	Files   []string `json:"files,omitempty"`
	Applied bool     `json:"applied"`
}

// getChanges returns the diff a confined write run produced.
func (b *bridge) getChanges(ctx context.Context, req *mcp.CallToolRequest, in runIDInput) (*mcp.CallToolResult, changesOutput, error) {
	r, err := b.runs.Get(in.RunID)
	if err != nil {
		return nil, changesOutput{}, err
	}
	s := r.Snapshot()
	if s.State == run.StateSuperseded {
		// The predecessor's change-store entry was retired when the
		// continuation started: exactly one acceptable diff exists per
		// chain, and it lives on the successor, not here.
		return nil, changesOutput{}, &SupersededError{RunID: s.ID, SuccessorID: s.SupersededBy}
	}
	if !s.Confined {
		// An unconfined run edited the tree directly: there is no diff to get,
		// and saying so is better than returning an empty success.
		return textResult("run %s ran UNCONFINED in %s: its edits are already in your tree, "+
				"and there is no diff to review", s.ID, s.CWD),
			changesOutput{RunID: s.ID, Status: "no_diff_unconfined", Files: nil}, nil
	}
	p, ok := b.changes.get(in.RunID)
	if !ok {
		return nil, changesOutput{}, fmt.Errorf("no pending changes for run %s", in.RunID)
	}
	// The patch is the agent's output and is treated as such.
	env := sanitize.Envelope(s.Agent, s.ID, p.patch, b.cfg.Defaults.MaxOutputBytes)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: env.Text}}},
		changesOutput{RunID: s.ID, Status: "pending", Files: p.files}, nil
}

// acceptChanges applies a confined write run's diff to the caller's tree.
//
// Exposed to the model only when the operator enabled agent_acceptance; by
// default the operator's own `bridge accept` is the only way in.
func (b *bridge) acceptChanges(ctx context.Context, req *mcp.CallToolRequest, in runIDInput) (*mcp.CallToolResult, changesOutput, error) {
	r, err := b.runs.Get(in.RunID)
	if err != nil {
		return nil, changesOutput{}, err
	}
	s := r.Snapshot()
	if s.State == run.StateSuperseded {
		// Same guard as getChanges: this is a property of the state machine,
		// not of changeStore.discard having already run on this predecessor
		// (wave-4 review, Important — the two windows are not the same
		// thing, and the guard belongs on both).
		return nil, changesOutput{}, &SupersededError{RunID: s.ID, SuccessorID: s.SupersededBy}
	}
	if !s.Confined {
		return textResult("run %s ran UNCONFINED: its edits are already in your tree", s.ID),
			changesOutput{RunID: s.ID, Status: "not_applicable_unconfined"}, nil
	}
	p, ok := b.changes.get(in.RunID)
	if !ok {
		return nil, changesOutput{}, fmt.Errorf("no pending changes for run %s", in.RunID)
	}
	if p.accepted {
		return nil, changesOutput{}, fmt.Errorf("run %s has already been accepted", in.RunID)
	}
	if err := worktree.Apply(p.wt.RepoRoot, p.patch); err != nil {
		_ = b.audit.Write(audit.Entry{Event: "changes.rejected", RunID: in.RunID, Message: err.Error()})
		// A conflicting patch refuses rather than forcing: the operator's own
		// edits are never overwritten by a stale diff.
		return nil, changesOutput{}, fmt.Errorf("the changes no longer apply cleanly and were not written")
	}
	p.accepted = true
	_ = b.audit.Write(audit.Entry{
		Event: "changes.accepted", RunID: in.RunID,
		Message: fmt.Sprintf("%d file(s) applied to %s", len(p.files), p.wt.RepoRoot),
	})
	_ = b.changes.discard(in.RunID)

	return textResult("applied %d file(s) from run %s:\n%s",
			len(p.files), in.RunID, strings.Join(p.files, "\n")),
		changesOutput{RunID: in.RunID, Status: "applied", Files: p.files, Applied: true}, nil
}

// takeWorktree retrieves a finished run's worktree, once.
func (b *bridge) takeWorktree(r interface{ TakeWorktree() any }) *worktree.Worktree {
	wt, _ := r.TakeWorktree().(*worktree.Worktree)
	return wt
}
