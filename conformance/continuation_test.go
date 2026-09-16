// Continuation conformance: does the whole chain spec §10 describes actually
// hold against a real vendor CLI — a real delegated agent asks a question
// with no operator attached, the run stops in needs_input, the operator
// answers later over the control channel, and the successor completes the
// original task using that answer?
//
// Nothing else in this suite proves that end to end. internal/run,
// cmd/bridge and internal/worktree's own (non-gated) tests already pin the
// state machine, the seed construction and the worktree handoff in
// isolation; what only a real vendor CLI can prove is that a real model
// actually calls AskUserQuestion, actually settles into needs_input when no
// human is reachable, and actually incorporates a later answer into its
// final output once continued — see docs/12-spike-results.md for why this
// package exists instead of trusting the unit tests alone.
//
// Like the rest of this package, this test spends real vendor credits and is
// gated behind BRIDGE_CONFORMANCE=1.
package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oxcom/agents-mcp-bridge/internal/control"
)

// TestNeedsInputAnswerContinuesIntoASuccessorRun is the end-to-end claim
// spec §10's "Gated conformance" line describes. It reuses
// claudeInteractive (interactive_test.go): a read-only, tier-full adapter
// with the gate wired in. Read-only is deliberate — the confined-write
// continuation path (worktree.ContinueFrom) has its own coverage in
// internal/worktree's BRIDGE_GIT_TESTS suite, and pulling git into this test
// would make it a test about git rather than about continuation.
func TestNeedsInputAnswerContinuesIntoASuccessorRun(t *testing.T) {
	requireConformance(t)
	requireCLI(t, "claude")

	cfg, _ := interactiveConfig(t)
	h, sock, stateHome := startInteractiveBridge(t, cfg, "codex")
	c := dialControl(t, sock)
	defer c.Close()
	assertStateIsolated(t, c, stateHome)

	// The instruction to ask is conditional on not already knowing the answer,
	// which is what makes this a test of continuation rather than of prompt
	// obedience: an unconditional "ask me whether to ..." is still in force in
	// the successor's seed (the chain's original prompt is replayed verbatim,
	// docs/02-architecture.md §"continuation seed"), so a successor that asks
	// again is obeying the caller, not losing the answer. The bridge must not
	// rewrite a caller's prompt to prevent that; max_continuations bounds the
	// chain instead.
	predID := h.ask(t, "claude",
		"Decide what to name a file. If you do not already know the answer, use the "+
			"AskUserQuestion tool once to ask whether to name it red.txt or blue.txt. "+
			"As soon as you know the answer, do not ask again: reply with exactly the "+
			"filename you decided on, quoting it verbatim.",
		askDeadline)

	// No watcher is ever attached to this run: the point of this test is
	// that the question was asked with no operator present, exactly as
	// TestNoOperatorChannelFailsClosed proves in isolation. awaitResult
	// must therefore settle into needs_input instead of hanging or falling
	// back to elicitation (this test's own MCP client advertises no
	// elicitation capability — see harness.initialise).
	state, sessionHandle, body := h.awaitResult(t, predID, 180, awaitDeadline)
	if state != "needs_input" {
		t.Fatalf("predecessor state = %q, want needs_input:\n%s", state, body)
	}
	if sessionHandle == "" {
		t.Fatal("needs_input run returned no session handle; nothing to continue from")
	}

	// The operator arrives after the fact and answers over the control
	// channel, exactly as `bridge answer` does (cmd/bridge/accept.go
	// answerRun): a run resting in needs_input turns an answer into a
	// continuation (VerbContinue) rather than VerbAnswer, which only
	// applies to a still-live pending question. The answer names a
	// filename the prompt never mentions, so the successor's final output
	// can only contain it if the operator's text actually reached the
	// successor through the continuation seed — matching "blue.txt" alone
	// would also pass if the model merely guessed from the prompt's own two
	// options, and would not distinguish this run from any other.
	const distinctiveMarker = "oxcom-continuation-6621"
	const distinctiveAnswer = "The user answered: ignore red.txt and blue.txt, name the file " +
		distinctiveMarker + ".txt instead."
	resp, err := c.Do(control.Request{Verb: control.VerbContinue, RunID: predID, Text: distinctiveAnswer})
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	successorID := resp.SuccessorID
	if successorID == "" {
		t.Fatalf("continue returned no successor id: %+v", resp)
	}

	// continueRun (cmd/bridge/continuation.go) only calls Supersede, and
	// only returns the successor id, once Start has already admitted the
	// successor — so by the time c.Do above returns, the predecessor's
	// terminal transition has already happened. findRun's own poll loop is
	// still used rather than a single VerbRuns call, so a future change
	// that makes any part of this asynchronous fails this step by name
	// instead of racing it.
	pred := findRun(t, c, predID, runLookupS)
	if pred.State != "superseded" {
		t.Fatalf("predecessor state = %q, want superseded", pred.State)
	}
	if pred.SupersededBy != successorID {
		t.Fatalf("predecessor superseded_by = %q, want the successor id %q", pred.SupersededBy, successorID)
	}

	assertResumedFrom(t, stateHome, successorID, predID)

	succState, _, succBody := h.awaitResult(t, successorID, 180, awaitDeadline)
	if succState != "completed" {
		t.Fatalf("successor state = %q, want completed (the whole point of continuing is that the "+
			"original task finishes):\n%s", succState, succBody)
	}
	if !strings.Contains(succBody, distinctiveMarker) {
		t.Fatalf("the operator's answer did not reach the successor's output: %q", succBody)
	}
}

// assertResumedFrom proves the successor's own run.admitted audit entry
// (internal/audit.Entry.ResumedFrom, spec §8) names the predecessor. This is
// the only channel that exposes resumed_from at all: control.RunInfo
// (cmd/bridge/controlhandler.go's Runs()) surfaces SupersededBy on the
// predecessor's side but never ResumedFrom on the successor's, and no MCP
// tool returns it either (askOutput deliberately carries only
// SupersededBy — see cmd/bridge/tools.go's own comment on why a vendor
// session id, and by the same reasoning any continuation lineage, stays off
// the caller-facing surface). The audit write is synchronous and fsynced
// (internal/audit.Writer.Write's own doc comment) and happens inside
// continueRun before VerbContinue's response is sent, so no poll or
// deadline is needed here: the line is already on disk once c.Do above
// returned.
func assertResumedFrom(t *testing.T, stateHome, successorID, predID string) {
	t.Helper()
	auditPath := filepath.Join(stateHome, "agents-bridge", "audit.jsonl")
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log %s: %v", auditPath, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry struct {
			Event       string `json:"event"`
			RunID       string `json:"run_id"`
			ResumedFrom string `json:"resumed_from"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.Event == "run.admitted" && entry.RunID == successorID {
			if entry.ResumedFrom != predID {
				t.Fatalf("successor %s's run.admitted audit entry has resumed_from=%q, want %q",
					successorID, entry.ResumedFrom, predID)
			}
			return
		}
	}
	t.Fatalf("no run.admitted audit entry for successor %s found in %s", successorID, auditPath)
}
