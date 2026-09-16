package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/audit"
	"github.com/oxcom/agents-mcp-bridge/internal/gate"
	"github.com/oxcom/agents-mcp-bridge/internal/platform"
	"github.com/oxcom/agents-mcp-bridge/internal/policy"
	"github.com/oxcom/agents-mcp-bridge/internal/run"
	"github.com/oxcom/agents-mcp-bridge/internal/sanitize"
)

// writeGateConfig writes the per-run mcp-config that points the child at this
// run's gate and at nothing else.
//
// The token rides in this file's per-server env map rather than in the child's
// inherited environment, so the invariant that the child never receives
// AGENTS_BRIDGE_* survives: it learns one socket for one run.
func writeGateConfig(dir, runID, command, socket, token string) (string, error) {
	doc := map[string]any{
		"mcpServers": map[string]any{
			"bridge_gate": map[string]any{
				"command": command,
				"args":    []string{"gate"},
				"env": map[string]string{
					"AGENTS_BRIDGE_GATE_SOCKET": socket,
					"AGENTS_BRIDGE_GATE_TOKEN":  token,
					"AGENTS_BRIDGE_GATE_RUN":    runID,
				},
			},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "gate-"+runID+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func newGateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// resolver builds the Resolver for one run. It is constructed before the
// *run.Run exists — the gate must start before the child is spawned — so it
// cannot close over the Run directly; instead it looks runID up in b.runs
// lazily, inside Resolve, once a request actually arrives. A lookup that
// fails denies, never allows (see gate.NewResolver).
//
// TUIAttached and Elicit are the two human channels, in the precedence
// gate.NewResolver enforces: a watcher attached to the operator TUI always
// wins over elicitation, and elicitation is only ever consulted for the run
// currently blocked inside an await_agent call.
//
// TUIAttached is scoped to THIS run (I1): WatchersAttachedTo(runID), not the
// server-wide WatchersAttached. A watcher on a different run must not claim
// this run's question — that would route it to a TUI that never shows it and
// skip elicitation, the operator's only other channel, entirely.
func (b *bridge) resolver(runID string) gate.Resolver {
	return gate.NewResolver(
		func() (*run.Run, error) { return b.runs.Get(runID) },
		gate.Channels{
			TUIAttached: func() bool { return b.control.WatchersAttachedTo(runID) },
			Elicit:      b.elicit(runID),
			Timeout:     time.Duration(b.cfg.Defaults.QuestionTimeoutS) * time.Second,
		},
		func(s string) string { return sanitize.Clean(s, b.cfg.Defaults.MaxOutputBytes).Text },
		b.auditQuestion(runID),
	)
}

// auditQuestion returns the resolver's audit sink for one run. The resolver
// emits question.asked, question.answered, question.denied and
// question.timeout; this writes each straight through to the run's own audit
// trail so the operator can reconstruct what a run asked and how it was
// settled, without the question text itself (already sanitized by clean
// before it reaches the resolver) needing a second envelope here.
//
// Every entry also carries TargetAgent so an operator reading the log can
// tell which delegated agent asked, matching the attribution the TUI panel
// and the elicitation prompt already carry (roadmap 3.5). The lookup can
// fail — the gate starts, and can receive a request, before b.runs.Start
// registers the run it serves (docs/superpowers spec §3) — in which case the
// entry is written with no TargetAgent rather than blocking the audit write
// on a run that does not exist yet.
func (b *bridge) auditQuestion(runID string) func(event, detail string) {
	return func(event, detail string) {
		agent := ""
		if r, err := b.runs.Get(runID); err == nil {
			agent = r.Agent
		}
		_ = b.audit.Write(audit.Entry{Event: event, RunID: runID, TargetAgent: agent, Message: detail})
	}
}

// startGate mints a per-run token, starts this run's gate socket and writes
// the mcp-config that points the child at it. On any failure it cleans up
// whatever it already started: a half-started gate must not survive a failed
// buildSpec.
func (b *bridge) startGate(runID string, d *policy.Decision) (socket, token, cfgPath string, err error) {
	token, err = newGateToken()
	if err != nil {
		return "", "", "", fmt.Errorf("could not mint a gate token: %w", err)
	}

	srv, err := gate.Listen(b.runtimeDir, runID, token, platform.NewControlEndpoint(), b.resolver(runID), b.log)
	if err != nil {
		return "", "", "", fmt.Errorf("could not start the gate: %w", err)
	}

	cfgPath, err = writeGateConfig(b.stateDir, runID, os.Args[0], srv.Socket(), token)
	if err != nil {
		_ = srv.Close()
		return "", "", "", fmt.Errorf("could not write the gate config: %w", err)
	}

	b.gatesMu.Lock()
	b.gates[runID] = srv
	b.gatesMu.Unlock()

	return srv.Socket(), token, cfgPath, nil
}

// closeGate stops a run's gate, if any, and removes its mcp-config file. A
// gate outliving its run is a fault: the socket and the config are per-run
// secrets that must not remain reachable once the run is done.
func (b *bridge) closeGate(runID string) {
	b.gatesMu.Lock()
	srv, ok := b.gates[runID]
	if ok {
		delete(b.gates, runID)
	}
	b.gatesMu.Unlock()
	if ok {
		if err := srv.Close(); err != nil {
			b.log.Warn("gate close failed", "run", runID, "err", err)
		}
	}
	// Attempted unconditionally, independent of the map hit: a prior call
	// (needs_input, then a repeated await_agent, C4) already removed srv from
	// b.gates and returned early here before this line ever ran, so a config
	// file that survived one failed os.Remove — a transient permission error,
	// a concurrent unlink — was never retried (Minor).
	cfgPath := filepath.Join(b.stateDir, "gate-"+runID+".json")
	if err := os.Remove(cfgPath); err != nil && !os.IsNotExist(err) {
		b.log.Warn("gate config remove failed", "run", runID, "err", err)
	}
}

// sweepStaleGateConfigs removes orphaned gate-*.json files from a previous
// process's state dir. A crash skips both recordCompletion and the shutdown
// defer, so nothing else ever removes them; call this once at startup,
// before any run of the new process can start its own gate, so a stale
// file's run ID can never collide with a live one. Only files matching the
// gate-*.json pattern are touched.
//
// The state dir is shared by every bridge instance on the machine (I3), so a
// file found here is not necessarily this process's own: a second bridge
// that is live right now has its own gate-*.json sitting in the same
// directory, and removing it would break that bridge's child mid-run.
// Ownership is established, not assumed: each config records the unix socket
// its gate listens on, and gateConfigLive dials it. A listener answering
// means some process still owns that run; only a config whose socket answers
// no one is treated as orphaned and removed.
func sweepStaleGateConfigs(stateDir, runtimeDir string, log *slog.Logger) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return
	}
	cleared := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "gate-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		runID := strings.TrimSuffix(strings.TrimPrefix(name, "gate-"), ".json")
		path := filepath.Join(stateDir, name)
		if gateConfigLive(path, runtimeDir, runID) {
			continue // another process's run still owns this gate; never touch a live one
		}
		if err := os.Remove(path); err != nil {
			log.Warn("stale gate config remove failed", "file", name, "err", err)
			continue
		}
		cleared++
	}
	if cleared > 0 {
		log.Debug("cleared stale gate configs", "count", cleared)
	}
}

// gateConfigLive reports whether a gate-*.json file still points at a socket
// something is listening on (I3). It fails toward "live": a file that cannot
// be read or parsed, or that names no socket, is treated as live and left
// alone, because a live run broken by an over-eager sweep is worse than a
// harmless stale file left on disk. Only a socket that actively refuses the
// connection is proof of an orphan.
//
// The socket dialed is never the one the file names. A confined write run's
// worktree lives inside this same state dir (cmd/bridge/tools.go,
// worktree.Create), so a delegated agent running as this UID could otherwise
// write its own gate-*.json naming an arbitrary unix socket anywhere on the
// machine and make sweepStaleGateConfigs dial it — a liveness oracle, and a
// socket-activation trigger, reachable from untrusted agent output
// (Important 3). Instead this computes the ONE path a legitimate gate for
// runID (taken from the trusted filename, never from file content) could
// ever be listening on, and dials only that. The file's own socket field is
// read solely to check it agrees; a mismatch, like an unparseable or
// unreadable file, means ownership cannot be proven, so it is left alone —
// unproven ownership never deletes, and is never dialed either.
func gateConfigLive(path, runtimeDir, runID string) bool {
	// #nosec G304 -- path is the state directory joined with a gate-*.json name enumerated
	// there by ReadDir; it comes from the trusted filename, never from file content.
	raw, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	var doc struct {
		McpServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return true
	}
	srv, ok := doc.McpServers["bridge_gate"]
	if !ok {
		return true
	}
	socket := srv.Env["AGENTS_BRIDGE_GATE_SOCKET"]
	if socket == "" {
		return true
	}
	want := filepath.Join(runtimeDir, "gate", "gate-"+runID+".sock")
	if filepath.Clean(socket) != want {
		return true // not ours: never dial a path this process did not compute itself
	}
	conn, err := platform.DialControl(want, 200*time.Millisecond)
	if err != nil {
		return false // nothing is listening: orphaned
	}
	_ = conn.Close()
	return true
}

// closeAllGates stops every live gate. Called on server shutdown so no gate
// outlives the process that owns it.
func (b *bridge) closeAllGates() {
	b.gatesMu.Lock()
	ids := make([]string, 0, len(b.gates))
	for id := range b.gates {
		ids = append(ids, id)
	}
	b.gatesMu.Unlock()
	for _, id := range ids {
		b.closeGate(id)
	}
}
