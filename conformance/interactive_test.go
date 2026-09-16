// Interactive-mode conformance: does a real question asked by a real vendor
// CLI actually reach a human over the operator channel, and does the human's
// answer actually reach the model? And when no human is reachable, does the
// run fail closed into needs_input rather than hanging or guessing?
//
// Verified behaviour these tests pin (docs/12-spike-results.md C7, C8): a
// question never appears on the child's stdout; it arrives as an MCP
// tools/call on the gate this bridge hands the child via --mcp-config, named
// by --permission-prompt-tool. A human question is tool_name ==
// "AskUserQuestion"; anything else is a permission approval, which v1 denies.
// behavior: "deny" with a message is the answer channel; behavior: "allow"
// discards it and tells the child the user did not answer.
//
// Like the rest of this package, these tests spend real vendor credits and
// are gated behind BRIDGE_CONFORMANCE=1.
package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/control"
)

// shortTempDir returns a fresh temp directory shorter than t.TempDir(),
// suitable as a base for a directory a unix socket path will be built under.
// t.TempDir() nests under os.TempDir() using a pattern built from the full
// test name, which is long enough on its own (e.g. this package's test
// names run 30-60+ bytes) to blow the 104-byte sun_path budget once
// "/agents-bridge/gate/gate-run-xxxxxxxxxxxx.sock" is appended — mirrors
// internal/control/testutil_test.go's helper of the same name and reason.
func shortTempDir(t *testing.T) string {
	t.Helper()
	base := os.TempDir()
	if runtime.GOOS == "darwin" {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "b")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// claudeInteractive is a tier-full claude adapter with the gate wired in:
// capabilities.interactive: true (rule 18, backed by a non-empty interactive
// flag list for the mode) and features.interactive: true (opt-in; the
// feature defaults false). The flag lists mirror the ones examples/config.yaml
// documents as verified against a real CLI. --model pins a cheap model to
// keep conformance-run credit spend down, per the task brief.
const claudeInteractive = `
version: 1
allowed_roots: [{{WORK}}]
defaults:
  timeout_s: 180
features:
  interactive: true
agents:
  claude:
    description: Claude, tier full, with the gate enabled for a conformance run.
    command: claude
    tier: full
    mode: read-only
    capabilities:
      stream: true
      interactive: true
    sandbox:
      read-only:
        - "--tools"
        - "Read,Glob,Grep"
        - "--permission-mode"
        - "manual"
        - "--permission-prompts"
        - "none"
        - "--setting-sources"
        - ""
        - "--strict-mcp-config"
    interactive:
      read-only:
        # The roster flag repeated with AskUserQuestion added (rule 21): it
        # lands after {{sandbox_flags}}, so only this run gains the tool.
        - "--tools"
        - "Read,Glob,Grep,AskUserQuestion"
        - "--strict-mcp-config"
        - "--mcp-config"
        - "{{gate_config}}"
        - "--permission-prompt-tool"
        - "mcp__bridge_gate__ask"
        - "--permission-prompts"
        - "host"
    invoke:
      # {{sandbox_flags}} before {{gate_flags}}: last flag wins (docs/12 C8),
      # so --permission-prompts host (from the interactive list) is what
      # actually lands on argv, not the sandbox list's none.
      args: ["-p", "--output-format", "stream-json", "--verbose", "--model", "claude-haiku-4-5-20251001", "{{sandbox_flags}}", "{{gate_flags}}", "--", "{{prompt}}"]
      prompt: argv
`

// interactiveConfig writes claudeInteractive to a temp config file.
func interactiveConfig(t *testing.T) (configPath, work string) {
	t.Helper()
	return writeConfig(t, claudeInteractive)
}

// Deadlines for the harness's own stdio round trips. Both are well inside
// CI's default per-package timeout, so a wedged bridge fails this specific
// test with a named step instead of blocking the whole run.
const (
	askDeadline   = 30 * time.Second
	awaitDeadline = 190 * time.Second // > the 180s timeout_s passed to await_agent
	gateShutdownS = 30 * time.Second
	questionWaitS = 90 * time.Second
	runLookupS    = 30 * time.Second
)

// startInteractiveBridge starts the bridge under private XDG_RUNTIME_DIR and
// XDG_STATE_HOME directories.
//
// Both matter, not just the runtime one: internal/platform.NewPaths resolves
// transcripts, the per-run gate config (cmd/bridge/gateconfig.go
// writeGateConfig, under paths.State()) and the audit log and its HMAC key
// (cmd/bridge/serve.go) from XDG_STATE_HOME. Left at its real default
// (~/.local/state/agents-bridge), every conformance run would write real
// transcripts and audit entries into the operator's own state directory, and
// could collide with a genuinely running bridge's audit log. XDG_RUNTIME_DIR
// isolation alone (the previous version of this helper) only isolated the
// control and gate sockets, not that.
//
// The control socket path is then computable directly ("<pid>.sock" under
// runtime/agents-bridge, see internal/control.Listen), rather than guessed or
// scraped from stderr.
//
// Cleanup sends SIGTERM and waits a window sized for a real vendor child
// mid-turn to unwind (exactly the state a failing or timing-out test leaves
// it in) before falling back to SIGKILL, then asserts the per-run gate left
// no trace: no gate-*.json under the isolated state dir, no gate-*.sock under
// the isolated runtime dir. That is the exact failure closeAllGates
// (cmd/bridge/serve.go) exists to prevent, so a stray file here is reported
// as a failure of this test, not silently left for the next run to trip
// over.
func startInteractiveBridge(t *testing.T, configPath, host string) (h *harness, controlSocket, stateHome string) {
	t.Helper()
	// runtimeHome must be short: it becomes XDG_RUNTIME_DIR, from which both
	// the control socket (internal/control.Listen) and, for an interactive
	// run, the per-run gate socket (internal/gate.Listen) derive their
	// paths. t.TempDir() nests under a directory built from t.Name() (long
	// for these tests, e.g. "TestClaudeQuestionReachesTheGateAndThe...")
	// plus a numeric suffix; once "/agents-bridge/gate/gate-run-xxxx.sock"
	// is appended that exceeds the 104-byte portable sun_path limit
	// (internal/platform.sunPathMax) and every socket bind in this test
	// fails before any vendor CLI is ever invoked. shortTempDir sidesteps
	// this the same way internal/control/testutil_test.go's helper of the
	// same name does for that package's own socket-binding tests.
	// stateHome has no such constraint (it holds files, not sockets), so it
	// keeps using t.TempDir() for the isolation guarantees documented below.
	runtimeHome := shortTempDir(t)
	stateHome = t.TempDir()

	// Snapshot the REAL XDG_STATE_HOME's audit files before overriding the
	// env var, so a regression that keeps transcripts on the temp path while
	// deriving the audit log or its key from somewhere else (the gap
	// assertStateIsolated's protocol-only check cannot see) is caught: the
	// cleanup below re-snapshots and fails if either changed.
	realAgentsBridgeDir := realStateAgentsBridgeDir(t)
	realAuditBefore := snapshotFile(filepath.Join(realAgentsBridgeDir, "audit.jsonl"))
	realKeyBefore := snapshotFile(filepath.Join(realAgentsBridgeDir, "audit.key"))

	t.Setenv("XDG_RUNTIME_DIR", runtimeHome)
	t.Setenv("XDG_STATE_HOME", stateHome)

	// Registered before the bridge starts, so it runs after the cleanup that
	// stops it: a failed run's raw vendor transcript survives the temp dir
	// when BRIDGE_CONFORMANCE_ARTIFACTS is set.
	t.Cleanup(func() { captureTranscripts(t, stateHome) })

	gateRuntimeDir := filepath.Join(runtimeHome, "agents-bridge")
	gateStateDir := filepath.Join(stateHome, "agents-bridge")

	h = start(t, configPath, host)
	controlSocket = filepath.Join(gateRuntimeDir, fmt.Sprintf("%d.sock", h.cmd.Process.Pid))

	t.Cleanup(func() {
		if h.cmd.Process == nil {
			return
		}
		_ = h.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = h.cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(gateShutdownS):
			t.Errorf("bridge did not exit within %s of SIGTERM; the vendor child may still have been "+
				"mid-turn. Forcing SIGKILL, which means the gate-cleanup check below cannot be trusted "+
				"(shutdown defers only run on a clean exit) and is skipped", gateShutdownS)
			_ = h.cmd.Process.Kill()
			<-done
			return
		}

		if matches, _ := filepath.Glob(filepath.Join(gateStateDir, "gate-*.json")); len(matches) > 0 {
			t.Errorf("stray gate config left behind after a clean shutdown: %v", matches)
		}
		if matches, _ := filepath.Glob(filepath.Join(gateRuntimeDir, "gate-*.sock")); len(matches) > 0 {
			t.Errorf("stray gate socket left behind after a clean shutdown: %v", matches)
		}

		realAuditAfter := snapshotFile(filepath.Join(realAgentsBridgeDir, "audit.jsonl"))
		if realAuditAfter != realAuditBefore {
			t.Errorf("this run wrote to the REAL audit log at %s (before=%+v after=%+v); "+
				"XDG_STATE_HOME isolation failed to hold",
				filepath.Join(realAgentsBridgeDir, "audit.jsonl"), realAuditBefore, realAuditAfter)
		}
		realKeyAfter := snapshotFile(filepath.Join(realAgentsBridgeDir, "audit.key"))
		if realKeyAfter != realKeyBefore {
			t.Errorf("this run wrote to the REAL audit key at %s (before=%+v after=%+v); "+
				"XDG_STATE_HOME isolation failed to hold",
				filepath.Join(realAgentsBridgeDir, "audit.key"), realKeyBefore, realKeyAfter)
		}
	})

	return h, controlSocket, stateHome
}

// realStateAgentsBridgeDir resolves the bridge's "agents-bridge" directory
// under the REAL XDG_STATE_HOME (i.e. before this test overrides it),
// mirroring internal/platform.NewPaths's own resolution: XDG_STATE_HOME if
// set, else ~/.local/state. Called before t.Setenv touches the variable.
func realStateAgentsBridgeDir(t *testing.T) string {
	t.Helper()
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("resolve home directory: %v", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "agents-bridge")
}

// fileSnapshot is a cheap, comparable proxy for "this file changed": good
// enough to detect a write this suite must never make, without hashing
// contents.
type fileSnapshot struct {
	exists  bool
	size    int64
	modTime time.Time
}

func snapshotFile(path string) fileSnapshot {
	info, err := os.Stat(path)
	if err != nil {
		return fileSnapshot{}
	}
	return fileSnapshot{exists: true, size: info.Size(), modTime: info.ModTime()}
}

// assertStateIsolated confirms the bridge actually resolved its state
// (and therefore its transcripts, gate configs and audit log) inside the
// private XDG_STATE_HOME this test set, rather than a developer's real
// ~/.local/state/agents-bridge — checked two ways. First, over the control
// protocol: VerbStatus's TranscriptDir is the only field the operator
// protocol exposes that is derived from paths.State() (cmd/bridge/serve.go:
// transcriptDir = filepath.Join(paths.State(), "transcripts")). Second, and
// load-bearing for the actual claim: on the filesystem. The audit log and its
// HMAC key are opened/created eagerly in audit.New at server startup
// (internal/audit/audit.go), whenever features.audit is enabled — true by
// default — so by the time this function runs (after the harness's
// initialise() round trip, which only completes once the server is fully up)
// both files must already exist under the isolated state dir. Checking only
// the protocol string would miss a regression that kept transcripts on the
// temp path while deriving the audit log or its key from somewhere else;
// checking the filesystem is what actually proves nothing escaped.
func assertStateIsolated(t *testing.T, c *control.Client, stateHome string) {
	t.Helper()
	resp, err := c.Do(control.Request{Verb: control.VerbStatus})
	if err != nil || resp.Status == nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.HasPrefix(resp.Status.TranscriptDir, stateHome) {
		t.Fatalf("bridge state escaped the test's XDG_STATE_HOME: transcript_dir = %q, want a prefix of %q",
			resp.Status.TranscriptDir, stateHome)
	}

	auditLog := filepath.Join(stateHome, "agents-bridge", "audit.jsonl")
	auditKey := filepath.Join(stateHome, "agents-bridge", "audit.key")
	if _, err := os.Stat(auditLog); err != nil {
		t.Fatalf("audit log did not land under the isolated state dir %s: %v", auditLog, err)
	}
	if _, err := os.Stat(auditKey); err != nil {
		t.Fatalf("audit HMAC key did not land under the isolated state dir %s: %v", auditKey, err)
	}
}

// dialControl connects to the bridge's own control socket, retrying briefly:
// control.Listen runs before the MCP server starts serving, but the harness's
// initialise() round trip only proves the MCP transport is up, not that the
// control socket file has been created and is accepting yet.
func dialControl(t *testing.T, socket string) *control.Client {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := control.Dial(socket)
		if err == nil {
			return c
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("dial control socket %s: %v", socket, lastErr)
	return nil
}

// attachWatcher marks this control connection as an operator watcher. Do is a
// synchronous unix-socket round trip and the server's dispatch
// (internal/control/server.go) increments the watcher count in the same
// goroutine, BEFORE it writes the response — so a successful, OK response
// here is itself the proof that WatchersAttachedTo(runID) is already true
// for any gate request the resolver handles from this point on.
//
// This matches the production semantics cmd/bridge/watch.go's runWatch
// uses (attachToRun): a watcher always names the run it watches. An attach
// with no run id is a picker (internal/control/server.go) that never
// populates byRun, so it would never route this run's question here — that
// was the C-NEW regression a prior wave introduced by dropping the run id
// here too. Because a watcher can only name a run it already knows about,
// the caller must attach AFTER the run exists (after ask() returns its run
// id), not before; the resolver still only checks TUIAttached synchronously
// once the child actually calls AskUserQuestion, which needs real model
// latency the attach round trip comfortably beats.
func attachWatcher(t *testing.T, c *control.Client, runID string) {
	t.Helper()
	resp, err := c.Do(control.Request{Verb: control.VerbAttach, RunID: runID})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !resp.OK {
		t.Fatalf("attach: server returned not-OK: %+v", resp)
	}
}

// ask starts a delegated run and returns its run id, without awaiting it —
// the caller needs the run live so it can watch for a pending question. The
// stdio read is bounded by deadline: a wedged bridge fails this step by name
// instead of blocking forever on an unbounded pipe read.
func (h *harness) ask(t *testing.T, agent, prompt string, deadline time.Duration) string {
	t.Helper()
	h.send("tools/call", map[string]any{
		"name":      "ask_" + agent,
		"arguments": map[string]any{"prompt": prompt},
	})
	resp := h.readWithDeadline(t, "ask_"+agent, deadline)
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("ask_%s failed: %v", agent, resp)
	}
	sc, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("ask_%s returned no structured content: %v", agent, result)
	}
	runID, ok := sc["run_id"].(string)
	if !ok || runID == "" {
		t.Fatalf("ask_%s returned no run_id: %v", agent, sc)
	}
	return runID
}

// awaitResult calls await_agent and returns the run's terminal state, its
// session handle (non-empty only for needs_input, see cmd/bridge/tools.go),
// and the enveloped text body the caller would see. deadline must exceed the
// timeoutS passed to await_agent itself, or this step's own bound would race
// the server's.
func (h *harness) awaitResult(t *testing.T, runID string, timeoutS int, deadline time.Duration) (state, sessionHandle, body string) {
	t.Helper()
	h.send("tools/call", map[string]any{
		"name":      "await_agent",
		"arguments": map[string]any{"run_id": runID, "timeout_s": timeoutS},
	})
	resp := h.readWithDeadline(t, "await_agent run="+runID, deadline)
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("await_agent run=%s failed: %v", runID, resp)
	}
	if sc, ok := result["structuredContent"].(map[string]any); ok {
		if s, ok := sc["state"].(string); ok {
			state = s
		}
		if s, ok := sc["session_handle"].(string); ok {
			sessionHandle = s
		}
	}
	var sb strings.Builder
	if content, ok := result["content"].([]any); ok {
		for _, c := range content {
			if m, ok := c.(map[string]any); ok {
				if txt, ok := m["text"].(string); ok {
					sb.WriteString(txt)
				}
			}
		}
	}
	return state, sessionHandle, sb.String()
}

// readWithDeadline reads one JSON-RPC message off the bridge's stdout pipe,
// bounded by deadline. step names the caller's operation in the failure
// message, so a hang in "ask_claude" and a hang in "await_agent" are
// distinguishable instead of both surfacing as an unattributed pipe timeout.
func (h *harness) readWithDeadline(t *testing.T, step string, deadline time.Duration) map[string]any {
	t.Helper()
	if err := h.stdout.SetReadDeadline(time.Now().Add(deadline)); err != nil {
		t.Fatalf("%s: set read deadline: %v", step, err)
	}
	defer func() { _ = h.stdout.SetReadDeadline(time.Time{}) }()
	var msg map[string]any
	if err := h.dec.Decode(&msg); err != nil {
		t.Fatalf("%s: no response within %s (bridge likely wedged): %v", step, deadline, err)
	}
	return msg
}

// waitForQuestion polls VerbRuns, bounded by timeout, until the given run
// reports a pending question. The caller must already have attached a
// watcher to this run id (attachWatcher) — see that function's comment for
// why the attach happens right after the run id is known rather than
// before, and why that is still early enough.
func waitForQuestion(t *testing.T, c *control.Client, runID string, timeout time.Duration) control.RunInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := c.Do(control.Request{Verb: control.VerbRuns})
		if err != nil {
			t.Fatalf("waitForQuestion run=%s: runs: %v", runID, err)
		}
		for _, r := range resp.Runs {
			if r.RunID == runID && r.QuestionID != "" {
				return r
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("waitForQuestion run=%s: no question appeared within %s", runID, timeout)
	return control.RunInfo{}
}

// findRun polls VerbRuns for one run, without ever sending VerbAttach — used
// by the fail-closed test to read the run's Transcript path (control's own
// operator-facing view, RunInfo.Transcript) without providing the run an
// operator channel.
func findRun(t *testing.T, c *control.Client, runID string, timeout time.Duration) control.RunInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := c.Do(control.Request{Verb: control.VerbRuns})
		if err != nil {
			t.Fatalf("findRun run=%s: runs: %v", runID, err)
		}
		for _, r := range resp.Runs {
			if r.RunID == runID {
				return r
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("findRun run=%s: run never appeared on the control channel within %s", runID, timeout)
	return control.RunInfo{}
}

// TestClaudeQuestionReachesTheGateAndTheAnswerReachesTheModel is the
// end-to-end claim SECURITY.md withholds "enforced" on: a real claude, asked
// a prompt engineered to trigger AskUserQuestion, is routed to a human over
// the control channel, and the human's answer changes what the model does
// next.
func TestClaudeQuestionReachesTheGateAndTheAnswerReachesTheModel(t *testing.T) {
	requireConformance(t)
	requireCLI(t, "claude")

	cfg, _ := interactiveConfig(t)
	// host differs from the delegated agent id ("claude"): self-exclusion is
	// by agent_id == --host, matching the pattern the rest of this package
	// uses (claudeHostileRepo delegates to "claude" while hosted as "codex").
	h, sock, stateHome := startInteractiveBridge(t, cfg, "codex")
	c := dialControl(t, sock)
	defer c.Close()
	assertStateIsolated(t, c, stateHome)

	runID := h.ask(t, "claude",
		"Use the AskUserQuestion tool to ask me whether to name the file red.txt or blue.txt. "+
			"Then tell me exactly which filename you will use, quoting it verbatim.",
		askDeadline)

	// Attach as soon as the run id is known, matching production semantics
	// (attachWatcher's comment): a watcher always names the run it watches,
	// so it cannot attach before the run exists. ask() returns as soon as
	// the run is registered, well before the model has produced enough
	// output to reach AskUserQuestion, so this still lands ahead of the
	// resolver's synchronous TUIAttached check.
	attachWatcher(t, c, runID)

	q := waitForQuestion(t, c, runID, questionWaitS)
	// The choices routinely arrive in the question's options rather than in
	// its text — a real claude question is "What should I name the file?"
	// with options [red.txt blue.txt] — so asserting on the text alone would
	// fail a question that reached the bridge intact.
	asked := q.QuestionText + " " + strings.Join(q.QuestionOptions, " ")
	if q.QuestionText == "" || (!strings.Contains(asked, "red.txt") && !strings.Contains(asked, "blue.txt")) {
		t.Fatalf("question did not reach the bridge: %+v", q)
	}

	// The answer names a filename the prompt never mentions, so the model's
	// final output can only contain it if the operator's text actually
	// reached the model — matching "blue.txt" alone would also pass if the
	// model merely guessed from the prompt's own two options.
	const distinctiveAnswer = "The user answered: ignore red.txt and blue.txt, name the file oxcom-gate-4417.txt instead."
	const distinctiveMarker = "oxcom-gate-4417.txt"
	if _, err := c.Do(control.Request{
		Verb: control.VerbAnswer, RunID: runID, QuestionID: q.QuestionID, Text: distinctiveAnswer,
	}); err != nil {
		t.Fatalf("answer: %v", err)
	}

	state, _, out := h.awaitResult(t, runID, 180, awaitDeadline)
	if state != "completed" && state != "failed" {
		t.Fatalf("run ended in state %q, want completed or failed (either proves the run actually "+
			"finished instead of hanging back in needs_input): %s", state, out)
	}
	if !strings.Contains(out, distinctiveMarker) {
		t.Fatalf("the answer did not reach the model: %q", out)
	}
}

// TestNoOperatorChannelFailsClosed proves the other half of the claim: with
// no watcher attached to the control channel, and this test's own MCP client
// advertising no elicitation capability (see harness.initialise), a run that
// hits AskUserQuestion has no operator channel at all and must fail closed
// into needs_input rather than hang, guess, or silently deny-and-continue.
func TestNoOperatorChannelFailsClosed(t *testing.T) {
	requireConformance(t)
	requireCLI(t, "claude")

	cfg, _ := interactiveConfig(t)
	h, sock, stateHome := startInteractiveBridge(t, cfg, "codex")
	// A read-only control connection to confirm state isolation and read back
	// RunInfo.Transcript for the "transcript path must not leak" assertion
	// below. It never sends VerbAttach, so it does not itself become an
	// operator channel — the point of this test is that NONE is available.
	c := dialControl(t, sock)
	defer c.Close()
	assertStateIsolated(t, c, stateHome)

	runID := h.ask(t, "claude",
		"Use the AskUserQuestion tool to ask me whether to name the file red.txt or blue.txt.",
		askDeadline)

	run := findRun(t, c, runID, runLookupS)
	if run.Transcript == "" {
		t.Fatal("test setup: run has no transcript path; the leak assertion below would pass vacuously")
	}

	state, sessionHandle, body := h.awaitResult(t, runID, 180, awaitDeadline)
	if state != "needs_input" {
		t.Fatalf("state = %q, want needs_input:\n%s", state, body)
	}
	if !strings.Contains(body, "untrusted_agent_output") {
		t.Fatal("the question must reach the caller inside the untrusted-data envelope")
	}
	if !strings.Contains(body, "red.txt") && !strings.Contains(body, "blue.txt") {
		t.Fatalf("the question text itself did not reach the caller: %q", body)
	}
	if sessionHandle == "" {
		t.Fatal("needs_input must return a session handle identifying the stopped run, " +
			"even though there is no resume path: await_agent returned none")
	}
	if strings.Contains(body, run.Transcript) {
		t.Fatal("the transcript path must never reach the caller")
	}
}
