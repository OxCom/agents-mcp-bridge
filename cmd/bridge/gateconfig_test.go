package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/config"
	"github.com/oxcom/agents-mcp-bridge/internal/control"
	"github.com/oxcom/agents-mcp-bridge/internal/gate"
	"github.com/oxcom/agents-mcp-bridge/internal/platform"
	"github.com/oxcom/agents-mcp-bridge/internal/policy"
	"github.com/oxcom/agents-mcp-bridge/internal/run"
)

func TestGateConfigIsOwnerOnlyAndCarriesTheTokenOutOfBandOfTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	path, err := writeGateConfig(dir, "run-1", "/usr/bin/bridge", "/run/g.sock", "tok-1")
	if err != nil {
		t.Fatalf("writeGateConfig: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// POSIX only. Windows reports 0666 for every file regardless of its ACL,
	// so the mode bits prove nothing there; the control on Windows is the
	// owner-only DACL platform.WriteOwnerOnlyFile applies, which no test reads
	// back yet. Assert the mode where the mode is the control.
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("mcp-config mode = %o, want 600", perm)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	srv, ok := doc.MCPServers["bridge_gate"]
	if !ok {
		t.Fatalf("server name must be bridge_gate, got %v", doc.MCPServers)
	}
	if srv.Command != "/usr/bin/bridge" || len(srv.Args) != 1 || srv.Args[0] != "gate" {
		t.Fatalf("invocation = %q %q", srv.Command, srv.Args)
	}
	if srv.Env["AGENTS_BRIDGE_GATE_TOKEN"] != "tok-1" || srv.Env["AGENTS_BRIDGE_GATE_SOCKET"] != "/run/g.sock" {
		t.Fatalf("env map = %v", srv.Env)
	}
	if strings.Contains(string(raw), "control") {
		t.Fatal("the gate config must not mention the control socket")
	}
}

// interactiveAdapter builds an adapter that would be interactive if both
// capability and feature agreed. capable toggles capabilities.interactive.
func interactiveAdapter(t *testing.T, capable bool) *config.Adapter {
	t.Helper()
	no := false
	return &config.Adapter{
		ID:              "interactive-agent",
		Tier:            config.TierBasic,
		Mode:            config.ModeReadOnly,
		Worktree:        config.WorktreeRequired,
		SandboxEnforced: &no,
		Capabilities:    config.Capabilities{Interactive: capable},
		Interactive: map[string][]string{
			"read-only": {"--mcp-config={{gate_config}}"},
		},
		ResolvedCommand: self(t),
		Invoke: &config.Invocation{
			Args:   stubArgs("run", "{{gate_flags}}", "{{prompt}}"),
			Prompt: "argv",
		},
	}
}

func testEngine(t *testing.T, cfg *config.Config) *policy.Engine {
	t.Helper()
	guard := platform.NewPathGuard()
	root, err := guard.Canonicalise(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.AllowedRoots = []string{root}
	e, err := policy.New(cfg, guard, "claude", 0)
	if err != nil {
		t.Fatalf("policy.New: %v", err)
	}
	return e
}

func featureOn(on bool) config.Features {
	return config.Features{Interactive: &on}
}

// TestDoubleGateHoldsWhenEitherHalfIsMissing asserts the fail-closed default:
// a gate starts only when BOTH the adapter declares capabilities.interactive
// and the operator has turned the "interactive" feature on. Each case below
// flips exactly one half on and leaves the other off, so a version of
// interactiveEnabled that dropped either check would flip that case's
// outcome and fail it.
func TestDoubleGateHoldsWhenEitherHalfIsMissing(t *testing.T) {
	cases := []struct {
		name    string
		capable bool
		feature bool
	}{
		{"capability on, feature off", true, false},
		{"capability off, feature on", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			cfg := &config.Config{Features: featureOn(tc.feature)}
			b := &bridge{
				cfg:        cfg,
				engine:     testEngine(t, cfg),
				stateDir:   stateDir,
				runtimeDir: t.TempDir(),
				log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
				gates:      make(map[string]*gate.Server),
			}
			d := &policy.Decision{Adapter: interactiveAdapter(t, tc.capable), CWD: t.TempDir(), Mode: config.ModeReadOnly}

			spec, err := b.buildSpec(d, "hello", nil)
			if err != nil {
				t.Fatalf("buildSpec: %v", err)
			}

			if len(b.gates) != 0 {
				t.Fatalf("no gate server should start, got %v", b.gates)
			}
			entries, err := os.ReadDir(stateDir)
			if err != nil {
				t.Fatalf("read stateDir: %v", err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "gate-") {
					t.Fatalf("no gate config file should exist, found %s", e.Name())
				}
			}
			for _, arg := range spec.Args {
				if strings.Contains(arg, "gate") || strings.Contains(arg, "mcp-config") {
					t.Fatalf("argv must carry no gate flags, got %q", spec.Args)
				}
			}
		})
	}
}

// TestInteractiveRunCarriesNoBridgeSecretsInSpec is the gate-ON counterpart:
// with both halves of the double gate satisfied, the resulting run.Spec must
// still carry none of the gate's secrets in the child's own environment or
// argv. Those reach the child only through the per-run mcp-config's own env
// map (see writeGateConfig), never through Spec.Env or a bare argument —
// that separation is the whole point of routing interactive mode through a
// gate instead of environment variables.
func TestInteractiveRunCarriesNoBridgeSecretsInSpec(t *testing.T) {
	stateDir := t.TempDir()
	cfg := &config.Config{Features: featureOn(true)}
	b := &bridge{
		cfg:        cfg,
		engine:     testEngine(t, cfg),
		stateDir:   stateDir,
		runtimeDir: shortTempDir(t),
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		gates:      make(map[string]*gate.Server),
	}
	d := &policy.Decision{Adapter: interactiveAdapter(t, true), CWD: t.TempDir(), Mode: config.ModeReadOnly}

	spec, err := b.buildSpec(d, "hello", nil)
	if err != nil {
		t.Fatalf("buildSpec: %v", err)
	}
	t.Cleanup(func() { b.closeGate(spec.RunID) })

	if len(b.gates) != 1 {
		t.Fatalf("gate server should have started, got %v", b.gates)
	}

	// AGENTS_BRIDGE_DEPTH is a deliberate, unrelated exception: it carries
	// delegation-depth tracking to every child (internal/adapter.BuildEnv),
	// gate or not. The invariant this test guards is narrower: the gate's
	// own secrets never reach Spec.Env, only the per-run mcp-config's own
	// env map does (see writeGateConfig).
	for _, kv := range spec.Env {
		if strings.HasPrefix(kv, "AGENTS_BRIDGE_GATE_") {
			t.Fatalf("Spec.Env must carry no AGENTS_BRIDGE_GATE_ variable, found %q", kv)
		}
	}

	// The mcp-config's own PATH is expected in argv — that is how the vendor
	// CLI is told where to find it (--mcp-config={{gate_config}}), and the
	// path is not a secret. What must never appear bare in argv is the
	// gate's actual socket address or any AGENTS_BRIDGE_GATE_* name: those
	// reach the child only through the mcp-config file's own env map.
	srv := b.gates[spec.RunID]
	for _, arg := range spec.Args {
		if strings.Contains(arg, srv.Socket()) {
			t.Fatalf("argv must not carry the gate socket address, found it in %q", arg)
		}
		if strings.Contains(arg, "AGENTS_BRIDGE_GATE_") {
			t.Fatalf("argv must not carry a gate env var name, found %q", arg)
		}
	}
}

// TestSweepStaleGateConfigsRemovesOnlyGateFiles pins both halves of the
// gate-*.json match, not just one: audit.jsonl fails the .json suffix check
// alone (it would survive even a prefix-only matcher), and notgate.json
// fails the gate- prefix check alone (it would survive even a suffix-only
// matcher). Both must be present for the test to prove the sweep checks
// prefix AND suffix together. The two gate-*.json files carry real
// writeGateConfig content naming a socket nothing listens on (I3): a bare
// "{}" would no longer be removed, since gateConfigLive fails toward NOT
// deleting a file it cannot read a socket out of.
func TestSweepStaleGateConfigsRemovesOnlyGateFiles(t *testing.T) {
	dir := t.TempDir()
	runtimeDir := t.TempDir()
	for _, id := range []string{"run-1", "run-2"} {
		// The socket must be the exact path gateConfigLive computes from the
		// filename's run id (Important 3) — a socket anywhere else is now
		// "not ours" and left alone rather than dialed, so this pins
		// ownership before pinning liveness.
		dead := filepath.Join(runtimeDir, "gate", "gate-"+id+".sock")
		if _, err := writeGateConfig(dir, id, "/usr/bin/bridge", dead, "tok"); err != nil {
			t.Fatalf("writeGateConfig %s: %v", id, err)
		}
	}
	survivors := []string{"audit.jsonl", "notgate.json"}
	for _, name := range survivors {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("unrelated"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	sweepStaleGateConfigs(dir, runtimeDir, slog.New(slog.NewTextHandler(io.Discard, nil)))

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Name()] = true
	}
	if len(entries) != len(survivors) {
		t.Fatalf("expected only %v to survive, got %v", survivors, entries)
	}
	for _, name := range survivors {
		if !got[name] {
			t.Fatalf("%s must survive the sweep, got %v", name, entries)
		}
	}
}

// TestNeedsInputClosesTheGateAndRemovesItsConfig pins C4: a run that reaches
// needs_input has no live child left to answer through its gate, so the gate
// must not survive the run past that point either. Unlike
// TestSecondAwaitOnNeedsInputIsIdempotent in integration_test.go, this test
// exercises an adapter with capabilities.interactive: true, so buildSpec
// actually starts a gate and registers it in b.gates — the earlier test's
// echoer adapter never does, so it could not have caught this leak.
func TestNeedsInputClosesTheGateAndRemovesItsConfig(t *testing.T) {
	stateDir := t.TempDir()
	cfg := &config.Config{Features: featureOn(true), Defaults: config.Defaults{MaxOutputBytes: 1 << 16}}
	b := &bridge{
		cfg:        cfg,
		engine:     testEngine(t, cfg),
		stateDir:   stateDir,
		runtimeDir: shortTempDir(t),
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		gates:      make(map[string]*gate.Server),
		runs:       run.NewRegistry(4, time.Hour),
	}
	t.Cleanup(b.runs.CancelAll)

	// A sleeping child, not one that exits at once: the run must still be live when this
	// test drives it into needs_input by hand, the same way a real gate
	// resolver does when no operator channel answers a question.
	a := interactiveAdapter(t, true)
	a.ResolvedCommand = self(t)
	a.Invoke = &config.Invocation{Args: stubArgs("--sleep", "30s"), Prompt: "argv"}
	d := &policy.Decision{Adapter: a, CWD: t.TempDir(), Mode: config.ModeReadOnly}

	spec, err := b.buildSpec(d, "hello", nil)
	if err != nil {
		t.Fatalf("buildSpec: %v", err)
	}
	if _, ok := b.gates[spec.RunID]; !ok {
		t.Fatal("test setup: buildSpec did not start a gate")
	}
	cfgPath := filepath.Join(stateDir, "gate-"+spec.RunID+".json")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("test setup: gate config was not written: %v", err)
	}

	r, err := b.runs.Start(spec, platform.NewProcessGroup())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(r.Cancel)

	if _, err := r.Ask(run.Question{ID: "toolu_1", Tool: "AskUserQuestion", Text: "red or blue?"}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	r.NeedsInput("no operator channel was available to answer a question")

	first, out1, err := b.awaitAgent(context.Background(), nil, awaitInput{RunID: spec.RunID, TimeoutS: 1})
	if err != nil {
		t.Fatalf("first awaitAgent: %v", err)
	}
	if out1.State != string(run.StateNeedsInput) {
		t.Fatalf("state = %q, want needs_input", out1.State)
	}

	if _, ok := b.gates[spec.RunID]; ok {
		t.Fatal("a needs_input run must not leave its gate registered")
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("gate config must be removed once the run reaches needs_input, stat err = %v", err)
	}

	// A second await must not error, and must not try to double-close an
	// already-closed gate.
	second, out2, err := b.awaitAgent(context.Background(), nil, awaitInput{RunID: spec.RunID, TimeoutS: 1})
	if err != nil {
		t.Fatalf("second awaitAgent: %v", err)
	}
	if out2.State != string(run.StateNeedsInput) || out2.SessionHandle != out1.SessionHandle {
		t.Fatalf("second await result diverged: %+v vs %+v", out1, out2)
	}
	if resultText(second) != resultText(first) {
		t.Fatalf("second await returned different text:\nfirst:  %q\nsecond: %q",
			resultText(first), resultText(second))
	}
}

// TestSweepStaleGateConfigsLeavesALiveGateAlone pins I3: a gate-*.json whose
// socket is still being listened on belongs to a process that may be a
// second, concurrently running bridge, and the sweep must not delete it —
// deleting it would break that process's live run mid-flight.
func TestSweepStaleGateConfigsLeavesALiveGateAlone(t *testing.T) {
	dir := t.TempDir()
	runtimeDir := shortTempDir(t)

	// A live socket: something is actually listening, standing in for a
	// second bridge process's own gate. Its path must be the exact one
	// gateConfigLive computes for run-live's own filename (Important 3), or
	// the sweep now treats it as unproven and leaves it alone without
	// dialing at all — which would also pass this assertion, but for the
	// wrong reason, so the path has to match to keep the test honest.
	liveSocket := filepath.Join(runtimeDir, "gate", "gate-run-live.sock")
	if err := os.MkdirAll(filepath.Dir(liveSocket), 0o700); err != nil {
		t.Fatalf("mkdir gate dir: %v", err)
	}
	ln, err := platform.NewControlEndpoint().Listen(liveSocket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	// A dead socket path: nothing is listening, standing in for a config left
	// behind by a crashed process. Same requirement: it must be run-dead's
	// own computed path so the sweep proves ownership and actually dials it.
	deadSocket := filepath.Join(runtimeDir, "gate", "gate-run-dead.sock")

	livePath, err := writeGateConfig(dir, "run-live", "/usr/bin/bridge", liveSocket, "tok-live")
	if err != nil {
		t.Fatalf("writeGateConfig live: %v", err)
	}
	deadPath, err := writeGateConfig(dir, "run-dead", "/usr/bin/bridge", deadSocket, "tok-dead")
	if err != nil {
		t.Fatalf("writeGateConfig dead: %v", err)
	}

	sweepStaleGateConfigs(dir, runtimeDir, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := os.Stat(livePath); err != nil {
		t.Fatalf("a gate config whose socket is still live must survive the sweep: %v", err)
	}
	if _, err := os.Stat(deadPath); !os.IsNotExist(err) {
		t.Fatalf("a gate config whose socket answers no one must be removed, stat err = %v", err)
	}
}

// TestSweepStaleGateConfigsNeverDialsAPlantedOutOfTreeSocket pins Important
// 3: a gate-*.json whose recorded socket does NOT match
// <runtimeDir>/gate/gate-<runID>.sock (the one path this process would ever
// compute for that run id) must never be dialed, no matter what it points
// at — a confined write run's worktree lives inside the same state dir
// (cmd/bridge/tools.go, worktree.Create), so a delegated agent could plant
// such a file naming an arbitrary unix socket and use the sweep as a
// dial-anywhere oracle. The planted socket here is listened on (so a naive
// dial-what-the-file-says implementation would call it live and leave it —
// coincidentally the same outcome, but for the wrong reason) and the
// canary records whether it was ever connected to at all: a fixed
// implementation must never touch it, live or dead.
func TestSweepStaleGateConfigsNeverDialsAPlantedOutOfTreeSocket(t *testing.T) {
	dir := t.TempDir()
	runtimeDir := t.TempDir()

	plantedDir := shortTempDir(t) // NOT <runtimeDir>/gate — simulates a planted, out-of-tree path
	planted := filepath.Join(plantedDir, "anywhere.sock")
	dialed := make(chan struct{}, 1)
	ln, err := platform.NewControlEndpoint().Listen(planted)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case dialed <- struct{}{}:
			default:
			}
			_ = conn.Close()
		}
	}()

	path, err := writeGateConfig(dir, "run-planted", "/usr/bin/bridge", planted, "tok")
	if err != nil {
		t.Fatalf("writeGateConfig: %v", err)
	}

	sweepStaleGateConfigs(dir, runtimeDir, slog.New(slog.NewTextHandler(io.Discard, nil)))

	select {
	case <-dialed:
		t.Fatal("the sweep dialed a socket outside <runtimeDir>/gate — attacker-steerable dial (Important 3)")
	default:
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("an out-of-tree gate config's ownership cannot be proven; it must be left alone, not removed: %v", err)
	}
}

// TestWatcherOnOneRunDoesNotClaimAnotherRunsQuestion pins I1: gateconfig.go's
// resolver wiring must use control.WatchersAttachedTo(runID), scoped to the
// one run it serves, never the server-wide WatchersAttached. With a watcher
// attached only to run A, run B's resolver has no operator channel at all
// (this test wires no elicit fallback either) and must fail closed almost
// immediately. The old, server-wide wiring would instead see "some watcher
// is attached" as true for run B too, take the TUI path, and block for the
// full question timeout waiting for an answer that will never come — so the
// regression this guards against shows up as elapsed time, not just as a
// wrong boolean.
func TestWatcherOnOneRunDoesNotClaimAnotherRunsQuestion(t *testing.T) {
	b, _ := newTestBridge(t, self(t), stubArgs("--sleep", "30s"))
	b.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	b.cfg.Defaults.QuestionTimeoutS = 5 // long enough that a wrongly-blocked resolve is unmistakable

	ask := b.makeAsk("echoer")
	_, outA, err := ask(context.Background(), nil, askInput{Prompt: "a"})
	if err != nil {
		t.Fatalf("ask A: %v", err)
	}
	_, outB, err := ask(context.Background(), nil, askInput{Prompt: "b"})
	if err != nil {
		t.Fatalf("ask B: %v", err)
	}
	t.Cleanup(b.runs.CancelAll)

	srv, err := control.Listen(shortTempDir(t), platform.NewControlEndpoint(), b, b.log)
	if err != nil {
		t.Fatalf("control.Listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	b.control = srv

	c, err := control.Dial(srv.Socket())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if resp, err := c.Do(control.Request{Verb: control.VerbAttach, RunID: outA.RunID}); err != nil || !resp.OK {
		t.Fatalf("attach to run A: resp=%+v err=%v", resp, err)
	}

	start := time.Now()
	reply := b.resolver(outB.RunID).Resolve(context.Background(), gate.Ask{
		Tool:  "AskUserQuestion",
		Input: []byte(`{"question":"red or blue?"}`),
	})
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("run B's question took %s to resolve; a watcher on run A must not make it wait "+
			"for a TUI that will never show it (I1)", elapsed)
	}
	if reply.Behavior != "deny" {
		t.Fatalf("reply = %+v, want a fail-closed deny", reply)
	}

	rA, err := b.runs.Get(outA.RunID)
	if err != nil {
		t.Fatalf("get run A: %v", err)
	}
	if s := rA.Snapshot().State; s == run.StateNeedsInput {
		t.Fatal("run A, the one with the attached watcher, must be unaffected by run B's resolve")
	}
}

// TestInteractiveEnabledConsultsRuntimeToggleNotJustConfig pins I2:
// interactiveEnabled must ask the policy engine's FeatureEnabled, which
// applies BOTH the config ceiling and any runtime toggle, not the raw config
// value alone. Reading only the config ceiling would make `bridge disable
// interactive` inert while `bridge status` (which does go through the
// engine, see cmd/bridge/controlhandler.go Status) reports the feature off —
// the operator control would lie.
func TestInteractiveEnabledConsultsRuntimeToggleNotJustConfig(t *testing.T) {
	guard := platform.NewPathGuard()
	root := t.TempDir()
	canonicalRoot, err := guard.Canonicalise(root)
	if err != nil {
		t.Fatal(err)
	}
	agents := map[string]*config.Adapter{
		"codex": {ID: "codex", Mode: config.ModeReadOnly, Worktree: config.WorktreeRequired},
	}
	a := interactiveAdapter(t, true)

	// Config allows the feature; a runtime toggle must be able to narrow it,
	// and re-enabling within the ceiling must restore it.
	cfgOn := &config.Config{
		Version: 1, AllowedRoots: []string{canonicalRoot},
		Defaults: config.Defaults{MaxPromptBytes: 1024},
		Features: featureOn(true), Agents: agents,
	}
	engine, err := policy.New(cfgOn, guard, "claude", 0)
	if err != nil {
		t.Fatalf("policy.New: %v", err)
	}
	toggles := policy.NewToggles()
	engine.SetToggles(toggles)
	b := &bridge{cfg: cfgOn, engine: engine}

	if !b.interactiveEnabled(a) {
		t.Fatal("config on, no runtime toggle: interactive must be enabled")
	}
	toggles.Disable("interactive")
	if b.interactiveEnabled(a) {
		t.Fatal("config on + runtime disabled must gate interactive off (a runtime toggle may only narrow)")
	}
	toggles.Enable("interactive")
	if !b.interactiveEnabled(a) {
		t.Fatal("re-enabling within the ceiling must restore the gate")
	}

	// Config forbids the feature; no runtime state may widen past that
	// ceiling.
	off := false
	cfgOff := &config.Config{
		Version: 1, AllowedRoots: []string{canonicalRoot},
		Defaults: config.Defaults{MaxPromptBytes: 1024},
		Features: config.Features{Interactive: &off}, Agents: agents,
	}
	engineOff, err := policy.New(cfgOff, guard, "claude", 0)
	if err != nil {
		t.Fatalf("policy.New off: %v", err)
	}
	togglesOff := policy.NewToggles()
	togglesOff.Enable("interactive") // an attempt to widen at runtime
	engineOff.SetToggles(togglesOff)
	bOff := &bridge{cfg: cfgOff, engine: engineOff}

	if bOff.interactiveEnabled(a) {
		t.Fatal("config off must gate interactive off regardless of runtime state")
	}
}
