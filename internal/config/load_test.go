package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLookPath resolves any command to a real file so tests exercise the rules
// rather than the host's PATH.
func fakeLookPath(t *testing.T) func(string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	return func(name string) (string, error) {
		p := filepath.Join(dir, filepath.Base(name))
		if _, err := os.Stat(p); err != nil {
			if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
				return "", err
			}
		}
		return p, nil
	}
}

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func load(t *testing.T, body string) (*Config, error) {
	t.Helper()
	return Load(write(t, body), Options{SkipPermissionCheck: true, LookPath: fakeLookPath(t)})
}

func mustFail(t *testing.T, body, wantSubstring string) {
	t.Helper()
	_, err := load(t, body)
	if err == nil {
		t.Fatalf("expected a load error mentioning %q, got none", wantSubstring)
	}
	if !strings.Contains(err.Error(), wantSubstring) {
		t.Fatalf("error = %q, want it to mention %q", err, wantSubstring)
	}
}

const minimal = `
version: 1
allowed_roots: [/tmp]
agents:
  demo:
    command: demo
    tier: basic
    mode: read-only
    sandbox:
      read-only: ["--safe"]
    invoke:
      args: ["run"]
`

func TestLoadMinimal(t *testing.T) {
	cfg, err := load(t, minimal)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	a := cfg.Agents["demo"]
	if a == nil {
		t.Fatal("adapter not loaded")
	}
	if a.ID != "demo" {
		t.Errorf("ID = %q", a.ID)
	}
	if a.Worktree != WorktreeRequired {
		t.Errorf("worktree default = %q, want required", a.Worktree)
	}
	if a.Capabilities.Steer != SteerNone {
		t.Errorf("steer default = %q, want false", a.Capabilities.Steer)
	}
	if cfg.Defaults.TimeoutS != 900 {
		t.Errorf("timeout default = %d", cfg.Defaults.TimeoutS)
	}
	if a.ResolvedCommand == "" {
		t.Error("command was not resolved to an absolute path")
	}
}

func TestUnknownKeyIsFatal(t *testing.T) {
	// A typo in a security setting must stop the server, not be ignored.
	mustFail(t, strings.Replace(minimal, "  demo:", "  demo:\n    sandbx: {}", 1), "sandbx")
}

func TestDuplicateKeyIsFatal(t *testing.T) {
	// A shadowed setting is how a permissive value hides under a strict one.
	body := minimal + "\nallowed_roots: [/etc]\n"
	mustFail(t, body, "already defined")
}

func TestMultipleDocumentsRefused(t *testing.T) {
	mustFail(t, minimal+"\n---\nversion: 1\n", "multiple YAML documents")
}

func TestCredentialInEnvAllowlistRefused(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GH_TOKEN", "MY_SECRET", "DB_PASSWORD"} {
		body := strings.Replace(minimal, "allowed_roots: [/tmp]",
			"allowed_roots: [/tmp]\nenv_allowlist: ["+name+"]", 1)
		mustFail(t, body, "credential")
	}
}

func TestBypassFlagRefused(t *testing.T) {
	for _, flag := range []string{
		"--dangerously-skip-permissions",
		"--dangerously-bypass-approvals-and-sandbox",
		"--yolo",
		"--allow-all",
	} {
		body := strings.Replace(minimal, `      args: ["run"]`,
			`      args: ["run", "`+flag+`"]`, 1)
		mustFail(t, body, "bypass flag")
	}
}

func TestUnknownPlaceholderRefused(t *testing.T) {
	body := strings.Replace(minimal, `      args: ["run"]`,
		`      args: ["run", "{{session_id}}"]`, 1)
	// {{session_id}} was deliberately removed: vendor ids never cross the MCP
	// boundary. A config still using it must fail loudly.
	mustFail(t, body, "unknown placeholder")
}

func TestBasicTierCannotDeclareCapabilities(t *testing.T) {
	body := strings.Replace(minimal, "    mode: read-only",
		"    mode: read-only\n    capabilities:\n      stream: true", 1)
	mustFail(t, body, "tier: basic")
}

func TestEmptySandboxRequiresExplicitDeclaration(t *testing.T) {
	body := strings.Replace(minimal, `      read-only: ["--safe"]`, `      read-only: []`, 1)
	mustFail(t, body, "sandbox_enforced: false")
}

func TestEmptySandboxAllowedWhenDeclared(t *testing.T) {
	body := strings.Replace(minimal, `      read-only: ["--safe"]`, `      read-only: []`, 1)
	body = strings.Replace(body, "    mode: read-only", "    mode: read-only\n    sandbox_enforced: false", 1)
	if _, err := load(t, body); err != nil {
		t.Fatalf("an explicitly unsandboxed adapter must load: %v", err)
	}
}

func TestWriteModeNeedsCeiling(t *testing.T) {
	body := strings.Replace(minimal, "    mode: read-only", "    mode: write", 1)
	body = strings.Replace(body, `      read-only: ["--safe"]`, `      write: ["--rw"]`, 1)
	mustFail(t, body, "allow_write_mode")
}

func TestUnconfinedWriteNeedsBothCeilings(t *testing.T) {
	body := strings.Replace(minimal, "version: 1",
		"version: 1\ndefaults:\n  allow_write_mode: true", 1)
	body = strings.Replace(body, "    mode: read-only", "    mode: write\n    worktree: off", 1)
	body = strings.Replace(body, `      read-only: ["--safe"]`, `      write: ["--rw"]`, 1)
	// allow_write_mode alone must not be enough.
	mustFail(t, body, "allow_unconfined_write")

	ok := strings.Replace(body, "  allow_write_mode: true",
		"  allow_write_mode: true\n  allow_unconfined_write: true", 1)
	if _, err := load(t, ok); err != nil {
		t.Fatalf("both ceilings set must load: %v", err)
	}
}

func TestSteerBlockWithoutCapabilityRefused(t *testing.T) {
	body := strings.Replace(minimal, `      args: ["run"]`,
		`      args: ["run"]
    steer:
      mode: command
      args: ["queue"]`, 1)
	mustFail(t, body, "looking functional")
}

func TestInteractiveRequiresStream(t *testing.T) {
	body := `
version: 1
allowed_roots: [/tmp]
agents:
  demo:
    command: demo
    tier: full
    mode: read-only
    capabilities:
      interactive: true
    sandbox:
      read-only: ["--safe"]
    invoke:
      args: ["run"]
`
	mustFail(t, body, "requires stream")
}

func TestResumeRequiresResumeInvoke(t *testing.T) {
	body := `
version: 1
allowed_roots: [/tmp]
agents:
  demo:
    command: demo
    tier: full
    mode: read-only
    capabilities:
      resume: true
    sandbox:
      read-only: ["--safe"]
    invoke:
      args: ["run"]
`
	mustFail(t, body, "resume_invoke")
}

func TestResumeWithoutSandboxFlagsRefused(t *testing.T) {
	// The codex trap: a resume path that omits the sandbox runs writable.
	body := `
version: 1
allowed_roots: [/tmp]
agents:
  demo:
    command: demo
    tier: full
    mode: read-only
    capabilities:
      resume: true
    sandbox:
      read-only: ["--safe"]
    invoke:
      args: ["run", "{{sandbox_flags}}"]
    resume_invoke:
      args: ["resume", "{{vendor_session_id}}"]
`
	mustFail(t, body, "runs writable")
}

func TestStreamJSONRequiresCloseStdin(t *testing.T) {
	body := `
version: 1
allowed_roots: [/tmp]
agents:
  demo:
    command: demo
    tier: full
    mode: read-only
    capabilities:
      stream: true
    sandbox:
      read-only: ["--safe"]
    invoke:
      args: ["run"]
      prompt: stdin_stream_json
`
	mustFail(t, body, "close_stdin_after")
}

func TestQueuePolicyNeedsDepth(t *testing.T) {
	body := strings.Replace(minimal, "version: 1",
		"version: 1\ndefaults:\n  on_concurrency_limit: queue", 1)
	mustFail(t, body, "queue_depth")
}

func TestHostAdapterIsDropped(t *testing.T) {
	body := minimal + `
  other:
    command: other
    tier: basic
    mode: read-only
    sandbox:
      read-only: ["--safe"]
    invoke:
      args: ["run"]
`
	cfg, err := Load(write(t, body), Options{
		SkipPermissionCheck: true,
		LookPath:            fakeLookPath(t),
		Host:                "demo",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := cfg.Agents["demo"]; ok {
		t.Fatal("the host's own adapter must not be exposed: an agent may never delegate to itself")
	}
	if _, ok := cfg.Agents["other"]; !ok {
		t.Fatal("a different adapter must survive")
	}
}

func TestHostExclusionAlsoDropsAliasSharingTheExecutable(t *testing.T) {
	// Executable identity can only ADD an exclusion. An alias pointing at the
	// same binary under another id must go too.
	body := minimal + `
  demo_alias:
    command: demo
    tier: basic
    mode: read-only
    sandbox:
      read-only: ["--safe"]
    invoke:
      args: ["run"]
`
	cfg, err := Load(write(t, body), Options{
		SkipPermissionCheck: true,
		LookPath:            fakeLookPath(t),
		Host:                "demo",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := cfg.Agents["demo_alias"]; ok {
		t.Fatal("an alias resolving to the host executable must also be dropped")
	}
}

func TestAbsentMaxDepthDefaultsToOne(t *testing.T) {
	// Absent must not mean 0: that would silently expose no delegation tools.
	cfg, err := load(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Defaults.MaxDepthOrDefault(); got != 1 {
		t.Fatalf("absent max_depth = %d, want 1", got)
	}
}

func TestMaxDepthZeroIsPreserved(t *testing.T) {
	// 0 means delegation disabled; a naive default would silently re-enable it.
	body := strings.Replace(minimal, "version: 1", "version: 1\ndefaults:\n  max_depth: 0", 1)
	cfg, err := load(t, body)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Defaults.MaxDepthOrDefault(); got != 0 {
		t.Fatalf("max_depth = %d, want an explicit 0 preserved", got)
	}
}

func TestAbsentMaxContinuationsDefaultsToThree(t *testing.T) {
	cfg, err := load(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Defaults.MaxContinuationsOrDefault(); got != 3 {
		t.Fatalf("absent max_continuations = %d, want 3", got)
	}
}

func TestMaxContinuationsZeroIsPreserved(t *testing.T) {
	// 0 means no needs_input run may ever be continued; a naive default would
	// silently re-enable continuation.
	body := strings.Replace(minimal, "version: 1", "version: 1\ndefaults:\n  max_continuations: 0", 1)
	cfg, err := load(t, body)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Defaults.MaxContinuationsOrDefault(); got != 0 {
		t.Fatalf("max_continuations = %d, want an explicit 0 preserved", got)
	}
}

func TestNegativeMaxContinuationsIsRefused(t *testing.T) {
	body := strings.Replace(minimal, "version: 1", "version: 1\ndefaults:\n  max_continuations: -1", 1)
	mustFail(t, body, "defaults.max_continuations")
}

func TestOversizedConfigRefused(t *testing.T) {
	big := minimal + "\n# " + strings.Repeat("x", maxConfigBytes)
	mustFail(t, big, "exceeds")
}

func TestGroupWritableConfigRefused(t *testing.T) {
	path := write(t, minimal)
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path, Options{LookPath: fakeLookPath(t)})
	if err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("expected refusal of a group-writable config, got %v", err)
	}
}

func TestGroupWritableParentDirRefused(t *testing.T) {
	// Write access to the directory is write access to the config, whatever the
	// file's own mode says.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path, Options{LookPath: fakeLookPath(t)})
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("expected refusal of a group-writable config directory, got %v", err)
	}
}

func TestSymlinkedParentIsJudgedByItsTarget(t *testing.T) {
	// A parent path that traverses a symlink into a world-writable directory
	// must be judged by the target, not by the link.
	hostile := filepath.Join(t.TempDir(), "hostile")
	if err := os.Mkdir(hostile, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hostile, 0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(hostile, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	path := filepath.Join(link, "config.yaml")
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path, Options{LookPath: fakeLookPath(t)})
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("expected the symlink target's permissions to be judged, got %v", err)
	}
}

func TestNonRegularConfigRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "config.yaml")
	if err := syscallMkfifo(fifo); err != nil {
		t.Skipf("fifo unavailable: %v", err)
	}
	// Opening a fifo for reading blocks without a writer, so a writer is
	// attached; the point is that the loader refuses a non-regular file.
	go func() {
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err == nil {
			_, _ = w.WriteString(minimal)
			_ = w.Close()
		}
	}()
	_, err := Load(fifo, Options{LookPath: fakeLookPath(t)})
	if err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected refusal of a non-regular config, got %v", err)
	}
}

func TestUnresolvableCommandRefused(t *testing.T) {
	// fakeLookPath always succeeds, so the real failure path needs its own test.
	_, err := Load(write(t, minimal), Options{
		SkipPermissionCheck: true,
		LookPath: func(string) (string, error) {
			return "", os.ErrNotExist
		},
	})
	if err == nil || !strings.Contains(err.Error(), "does not resolve on PATH") {
		t.Fatalf("expected refusal when the adapter command cannot be resolved, got %v", err)
	}
}

func TestScriptLauncherCommandRefused(t *testing.T) {
	// cmd.exe re-parses a .cmd launcher's arguments, which would reintroduce
	// injection. The rule is enforced on every platform so the config is
	// portable and the refusal is testable here.
	dir := t.TempDir()
	shim := filepath.Join(dir, "demo.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Load(write(t, minimal), Options{
		SkipPermissionCheck: true,
		LookPath:            func(string) (string, error) { return shim, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "script launcher") {
		t.Fatalf("expected refusal of a .cmd launcher, got %v", err)
	}
}

func TestDefaultModeIsNotWidened(t *testing.T) {
	// SR-2: an adapter that says nothing about write must not become writable,
	// and the write ceiling must stay off unless explicitly enabled.
	cfg, err := load(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agents["demo"].Mode != ModeReadOnly {
		t.Errorf("mode = %q, want read-only", cfg.Agents["demo"].Mode)
	}
	if cfg.Defaults.AllowWriteMode || cfg.Defaults.AllowUnconfinedWrite {
		t.Error("write ceilings must default to false")
	}
	if cfg.Agents["demo"].Worktree != WorktreeRequired {
		t.Error("confinement must default to required")
	}
}

func TestAbsentFeaturesUseTheirDefaultsNotFalse(t *testing.T) {
	// The zero value of a bool is false, so a feature nobody mentioned would
	// silently be off. Everything defaults on except agent_steering and
	// interactive.
	cfg, err := load(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"stream", "watch", "operator_steering", "sessions", "audit", "progress_notifications"} {
		if !cfg.Features.Enabled(name) {
			t.Errorf("feature %q defaulted to off", name)
		}
	}
	if cfg.Features.Enabled("agent_steering") {
		t.Error("agent_steering must be opt-in: it removes the human from the loop")
	}
	// interactive routes a vendor permission prompt to the operator over MCP;
	// it must stay off until an operator asks for it (fail closed), and until
	// this change it gated nothing at all in production code.
	if cfg.Features.Enabled("interactive") {
		t.Error("interactive must be opt-in: it must default off, not on")
	}
	if cfg.Features.Enabled("teleportation") {
		t.Error("an unknown feature must be off")
	}
}

func TestInteractiveCapabilityWithoutFlagsIsALoadError(t *testing.T) {
	body := `
version: 1
allowed_roots: ["/tmp"]
agents:
  claude:
    command: /bin/cat
    tier: basic
    mode: read-only
    capabilities:
      interactive: true
    invoke:
      args: ["-p", "{{prompt}}"]
      prompt: argv
`
	mustFail(t, body, "interactive")
}

func TestInteractiveCapabilityWithoutFlagsForModeIsALoadError(t *testing.T) {
	// tier: full so rule 1 does not fire first; this isolates rule 18's first
	// half (the flag list, not the capability gate).
	body := `
version: 1
allowed_roots: ["/tmp"]
agents:
  claude:
    command: /bin/cat
    tier: full
    mode: read-only
    capabilities:
      stream: true
      interactive: true
    sandbox:
      read-only: ["--safe"]
    invoke:
      args: ["-p", "{{prompt}}"]
      prompt: argv
`
	mustFail(t, body, "interactive")
}

// A sandbox list and an interactive list may set the same vendor flag with
// different values: VERIFIED against claude 2.1.272 (docs/12-spike-results.md
// C8), the last occurrence on the actual argv wins, so this is not ambiguous
// and must not be a load error. {{sandbox_flags}} then {{gate_flags}} in
// invoke.args is what makes the interactive value the one that lands last.
func TestInteractiveFlagsMayRepeatASandboxFlagName(t *testing.T) {
	body := `
version: 1
allowed_roots: ["/tmp"]
agents:
  claude:
    command: /bin/cat
    tier: full
    mode: read-only
    capabilities:
      stream: true
      interactive: true
    sandbox:
      read-only: ["--permission-prompts", "none"]
    interactive:
      read-only: ["--permission-prompts", "host", "--tools", "Read,AskUserQuestion"]
    invoke:
      args: ["-p", "{{sandbox_flags}}", "{{gate_flags}}", "{{prompt}}"]
      prompt: argv
`
	if _, err := load(t, body); err != nil {
		t.Fatalf("expected a clean load (last flag wins, not ambiguous), got %v", err)
	}
}

func TestInteractiveFlagsWithoutCollisionLoadCleanly(t *testing.T) {
	body := `
version: 1
allowed_roots: ["/tmp"]
agents:
  claude:
    command: /bin/cat
    tier: full
    mode: read-only
    capabilities:
      stream: true
      interactive: true
    sandbox:
      read-only: ["--sandbox", "read-only"]
    interactive:
      read-only: ["--permission-prompt-tool", "mcp__bridge_gate__ask", "--tools", "Read,AskUserQuestion"]
    invoke:
      args: ["-p", "{{sandbox_flags}}", "{{gate_flags}}", "{{prompt}}"]
      prompt: argv
`
	if _, err := load(t, body); err != nil {
		t.Fatalf("expected a clean load, got %v", err)
	}
}

func TestInteractiveResumeInvokeWithoutGateFlagsIsALoadError(t *testing.T) {
	body := `
version: 1
allowed_roots: ["/tmp"]
agents:
  claude:
    command: /bin/cat
    tier: full
    mode: read-only
    capabilities:
      stream: true
      resume: true
      interactive: true
    sandbox:
      read-only: ["--sandbox", "read-only"]
    interactive:
      read-only: ["--permission-prompt-tool", "mcp__bridge_gate__ask", "--tools", "Read,AskUserQuestion"]
    invoke:
      args: ["-p", "{{sandbox_flags}}", "{{gate_flags}}", "{{prompt}}"]
      prompt: argv
    resume_invoke:
      args: ["-p", "--resume", "{{vendor_session_id}}", "{{sandbox_flags}}"]
      prompt: argv
`
	mustFail(t, body, "gate_flags")
}

// TestInteractiveWriteModeIsALoadError pins I5/rule 20: v1 denies every
// approval wholesale (docs/12 spike C7) and routes only AskUserQuestion to a
// human, so a write-mode adapter with capabilities.interactive: true would
// have every tool call that actually writes refused by the gate while
// looking, from the config alone, fully wired up for interactive use.
func TestInteractiveWriteModeIsALoadError(t *testing.T) {
	body := `
version: 1
allowed_roots: ["/tmp"]
defaults:
  allow_write_mode: true
agents:
  claude:
    command: /bin/cat
    tier: full
    mode: write
    worktree: required
    capabilities:
      stream: true
      interactive: true
    sandbox:
      write: ["--sandbox", "workspace-write"]
    interactive:
      write: ["--permission-prompt-tool", "mcp__bridge_gate__ask"]
    invoke:
      args: ["-p", "{{sandbox_flags}}", "{{gate_flags}}", "{{prompt}}"]
      prompt: argv
`
	mustFail(t, body, "mode is write")
}

func TestInteractiveResumeInvokeWithGateFlagsLoadsCleanly(t *testing.T) {
	body := `
version: 1
allowed_roots: ["/tmp"]
agents:
  claude:
    command: /bin/cat
    tier: full
    mode: read-only
    capabilities:
      stream: true
      resume: true
      interactive: true
    sandbox:
      read-only: ["--sandbox", "read-only"]
    interactive:
      read-only: ["--permission-prompt-tool", "mcp__bridge_gate__ask", "--tools", "Read,AskUserQuestion"]
    invoke:
      args: ["-p", "{{sandbox_flags}}", "{{gate_flags}}", "{{prompt}}"]
      prompt: argv
    resume_invoke:
      args: ["-p", "--resume", "{{vendor_session_id}}", "{{sandbox_flags}}", "{{gate_flags}}"]
      prompt: argv
`
	if _, err := load(t, body); err != nil {
		t.Fatalf("expected a clean load, got %v", err)
	}
}

// TestInteractiveWithoutTheQuestionToolIsALoadError pins rule 21: the gate
// transport can be wired correctly and still never receive anything if the
// flag that bounds the child's tool surface drops AskUserQuestion. Observed
// live: the model reported the tool unavailable and asked in plain text,
// which no parser sees, so the run hung until its timeout.
func TestInteractiveWithoutTheQuestionToolIsALoadError(t *testing.T) {
	body := `
version: 1
allowed_roots: ["/tmp"]
agents:
  claude:
    command: /bin/cat
    tier: full
    mode: read-only
    capabilities:
      stream: true
      interactive: true
    sandbox:
      read-only: ["--tools", "Read,Glob,Grep"]
    interactive:
      read-only: ["--permission-prompt-tool", "mcp__bridge_gate__ask"]
    invoke:
      args: ["-p", "{{sandbox_flags}}", "{{gate_flags}}", "{{prompt}}"]
      prompt: argv
`
	mustFail(t, body, "AskUserQuestion")
}

// The tool may be granted by the sandbox list instead of the interactive one:
// which flag carries the roster is vendor knowledge the loader does not have,
// so rule 21 accepts the name anywhere in the flags the adapter passes.
func TestQuestionToolInTheSandboxListSatisfiesRule21(t *testing.T) {
	body := `
version: 1
allowed_roots: ["/tmp"]
agents:
  claude:
    command: /bin/cat
    tier: full
    mode: read-only
    capabilities:
      stream: true
      interactive: true
    sandbox:
      read-only: ["--tools", "Read,Glob,Grep,AskUserQuestion"]
    interactive:
      read-only: ["--permission-prompt-tool", "mcp__bridge_gate__ask"]
    invoke:
      args: ["-p", "{{sandbox_flags}}", "{{gate_flags}}", "{{prompt}}"]
      prompt: argv
`
	if _, err := load(t, body); err != nil {
		t.Fatalf("expected a clean load, got %v", err)
	}
}

func TestExplicitFalseBeatsTheDefault(t *testing.T) {
	body := strings.Replace(minimal, "version: 1", "version: 1\nfeatures:\n  stream: false\n  agent_steering: true", 1)
	cfg, err := load(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Features.Enabled("stream") {
		t.Error("an explicit false was ignored")
	}
	if !cfg.Features.Enabled("agent_steering") {
		t.Error("an explicit true was ignored")
	}
}
