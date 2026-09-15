package policy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/config"
	"github.com/oxcom/agents-mcp-bridge/internal/platform"
)

func testEngine(t *testing.T, mutate func(*config.Config)) (*Engine, string) {
	t.Helper()
	root := t.TempDir()
	guard := platform.NewPathGuard()
	canonicalRoot, err := guard.Canonicalise(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Version:      1,
		AllowedRoots: []string{canonicalRoot},
		Defaults: config.Defaults{
			MaxDepth:       intPtr(1),
			MaxPromptBytes: 1024,
		},
		Features: config.Features{},
		Agents: map[string]*config.Adapter{
			"codex": {
				ID:       "codex",
				Mode:     config.ModeReadOnly,
				Worktree: config.WorktreeRequired,
			},
		},
	}
	if mutate != nil {
		mutate(cfg)
	}
	e, err := New(cfg, guard, "claude", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, canonicalRoot
}

func refusalReason(t *testing.T, err error) Reason {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("expected a policy refusal, got %v", err)
	}
	return r.Reason
}

func TestOmittedCWDUsesFirstAllowedRootNotInheritedCWD(t *testing.T) {
	// The inherited cwd is the HOST's cwd and may lie outside every root, so it
	// must never be the default.
	e, root := testEngine(t, nil)
	d, err := e.Authorise(Request{AgentID: "codex", Prompt: "hi"})
	if err != nil {
		t.Fatalf("authorise: %v", err)
	}
	if d.CWD != root {
		t.Fatalf("CWD = %q, want the first allowed root %q", d.CWD, root)
	}
}

func TestCWDOutsideRootsRefused(t *testing.T) {
	e, _ := testEngine(t, nil)
	outside := t.TempDir()
	_, err := e.Authorise(Request{AgentID: "codex", Prompt: "hi", CWD: outside})
	if got := refusalReason(t, err); got != ReasonRootViolation {
		t.Fatalf("reason = %q, want %q", got, ReasonRootViolation)
	}
}

func TestCWDTraversalRefused(t *testing.T) {
	e, root := testEngine(t, nil)
	for _, p := range []string{
		filepath.Join(root, ".."),
		filepath.Join(root, "..", ".."),
		"/etc",
	} {
		if _, err := e.Authorise(Request{AgentID: "codex", Prompt: "hi", CWD: p}); err == nil {
			t.Errorf("traversal to %q was allowed", p)
		}
	}
}

func TestSymlinkOutOfRootRefused(t *testing.T) {
	e, root := testEngine(t, nil)
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// The link is inside the root; its target is not. Canonicalisation must
	// reveal that before containment is tested.
	_, err := e.Authorise(Request{AgentID: "codex", Prompt: "hi", CWD: link})
	if got := refusalReason(t, err); got != ReasonRootViolation {
		t.Fatalf("symlink escape allowed; reason = %q", got)
	}
}

func TestRefusalDoesNotLeakRoots(t *testing.T) {
	// An untrusted model learns nothing about the filesystem from a refusal.
	e, root := testEngine(t, nil)
	_, err := e.Authorise(Request{AgentID: "codex", Prompt: "hi", CWD: "/etc/ssh"})
	if err == nil {
		t.Fatal("expected refusal")
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "/etc/ssh") {
		t.Fatalf("refusal leaks filesystem detail: %v", err)
	}
}

func TestSelfCallRefused(t *testing.T) {
	e, _ := testEngine(t, func(c *config.Config) {
		c.Agents["claude"] = &config.Adapter{ID: "claude", Mode: config.ModeReadOnly}
	})
	_, err := e.Authorise(Request{AgentID: "claude", Prompt: "hi"})
	if got := refusalReason(t, err); got != ReasonSelfCall {
		t.Fatalf("reason = %q, want %q", got, ReasonSelfCall)
	}
}

func TestDepthExhaustionRefusesEverything(t *testing.T) {
	cfg := &config.Config{
		AllowedRoots: []string{t.TempDir()},
		Defaults:     config.Defaults{MaxDepth: intPtr(1), MaxPromptBytes: 1024},
		Agents:       map[string]*config.Adapter{"codex": {ID: "codex", Mode: config.ModeReadOnly}},
	}
	e, err := New(cfg, platform.NewPathGuard(), "claude", 1) // already at max depth
	if err != nil {
		t.Fatal(err)
	}
	if !e.Exhausted() {
		t.Fatal("engine at max depth must report exhausted")
	}
	if _, err := e.Authorise(Request{AgentID: "codex", Prompt: "hi"}); refusalReason(t, err) != ReasonDepthExceeded {
		t.Fatalf("expected depth refusal, got %v", err)
	}
}

func TestMaxDepthZeroDisablesDelegation(t *testing.T) {
	cfg := &config.Config{
		AllowedRoots: []string{t.TempDir()},
		Defaults:     config.Defaults{MaxDepth: intPtr(0), MaxPromptBytes: 1024},
		Agents:       map[string]*config.Adapter{"codex": {ID: "codex"}},
	}
	e, err := New(cfg, platform.NewPathGuard(), "claude", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Exhausted() {
		t.Fatal("max_depth 0 must disable delegation entirely")
	}
}

func TestPerCallSandboxMayNarrowButNotWiden(t *testing.T) {
	e, _ := testEngine(t, func(c *config.Config) {
		c.Agents["writer"] = &config.Adapter{ID: "writer", Mode: config.ModeWrite, Worktree: config.WorktreeRequired}
	})

	// Narrowing a write adapter to read-only is allowed.
	d, err := e.Authorise(Request{AgentID: "writer", Prompt: "hi", Sandbox: "read-only"})
	if err != nil {
		t.Fatalf("narrowing refused: %v", err)
	}
	if d.Mode != config.ModeReadOnly {
		t.Errorf("mode = %q, want read-only", d.Mode)
	}

	// Widening a read-only adapter to write is not.
	_, err = e.Authorise(Request{AgentID: "codex", Prompt: "hi", Sandbox: "write"})
	if got := refusalReason(t, err); got != ReasonSandboxNotAllowed {
		t.Fatalf("widening allowed; reason = %q", got)
	}
}

func TestOversizedPromptRefused(t *testing.T) {
	e, _ := testEngine(t, nil)
	_, err := e.Authorise(Request{AgentID: "codex", Prompt: strings.Repeat("x", 2048)})
	if got := refusalReason(t, err); got != ReasonPromptTooLarge {
		t.Fatalf("reason = %q", got)
	}
}

func TestModelOverrideRequiresAllowlist(t *testing.T) {
	e, _ := testEngine(t, nil)
	if _, err := e.Authorise(Request{AgentID: "codex", Prompt: "hi", Model: "gpt-5"}); refusalReason(t, err) != ReasonModelNotAllowed {
		t.Fatal("a model override must be refused when no allowlist is declared")
	}

	e2, _ := testEngine(t, func(c *config.Config) {
		c.Agents["codex"].AllowedModels = []string{"o3"}
	})
	if _, err := e2.Authorise(Request{AgentID: "codex", Prompt: "hi", Model: "gpt-5"}); refusalReason(t, err) != ReasonModelNotAllowed {
		t.Fatal("a model outside the allowlist must be refused")
	}
	if _, err := e2.Authorise(Request{AgentID: "codex", Prompt: "hi", Model: "o3"}); err != nil {
		t.Fatalf("an allowlisted model must be accepted: %v", err)
	}
}

func TestUnknownAgentRefused(t *testing.T) {
	e, _ := testEngine(t, nil)
	if _, err := e.Authorise(Request{AgentID: "nope", Prompt: "hi"}); refusalReason(t, err) != ReasonUnknownAgent {
		t.Fatal("expected unknown_agent")
	}
}

func TestUnknownFeatureIsDeniedByDefault(t *testing.T) {
	e, _ := testEngine(t, nil)
	if e.FeatureEnabled("teleportation") {
		t.Fatal("an unknown feature must be off")
	}
	if !e.FeatureEnabled("stream") {
		t.Fatal("a configured feature must be on")
	}
}

func TestMissingRootIsAStartupFailure(t *testing.T) {
	cfg := &config.Config{
		AllowedRoots: []string{filepath.Join(t.TempDir(), "absent")},
		Defaults:     config.Defaults{MaxDepth: intPtr(1)},
	}
	if _, err := New(cfg, platform.NewPathGuard(), "claude", 0); err == nil {
		t.Fatal("a root that does not exist must fail at startup, not per call")
	}
}

func TestUnconfinedAdapterIsMarked(t *testing.T) {
	e, _ := testEngine(t, func(c *config.Config) {
		c.Agents["codex"].Worktree = config.WorktreeOff
		c.Agents["codex"].Mode = config.ModeWrite
	})
	d, err := e.Authorise(Request{AgentID: "codex", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Confined {
		t.Fatal("worktree: off must be reported as unconfined so every surface can label it")
	}
}

func intPtr(i int) *int { return &i }

func TestRuntimeToggleCanNarrowButNotWiden(t *testing.T) {
	e, _ := testEngine(t, func(c *config.Config) {
		off := false
		c.Features.Interactive = &off // the config forbids it
	})

	if e.FeatureEnabled("interactive") {
		t.Fatal("a feature the config forbids must be off")
	}
	// Turning it on at runtime must not exceed the ceiling.
	e.Toggles().Enable("interactive")
	if e.FeatureEnabled("interactive") {
		t.Fatal("a runtime toggle widened past the config ceiling")
	}

	// A feature the config allows can be narrowed at runtime, and restored.
	if !e.FeatureEnabled("stream") {
		t.Fatal("stream should default on")
	}
	e.Toggles().Disable("stream")
	if e.FeatureEnabled("stream") {
		t.Fatal("the runtime toggle did not take effect")
	}
	e.Toggles().Enable("stream")
	if !e.FeatureEnabled("stream") {
		t.Fatal("re-enabling within the ceiling failed")
	}
}

func TestTogglesReportWhatIsOff(t *testing.T) {
	e, _ := testEngine(t, nil)
	e.Toggles().Disable("watch")
	got := e.Toggles().Snapshot()
	if len(got) != 1 || got[0] != "watch" {
		t.Fatalf("snapshot = %v", got)
	}
}

func TestUnknownFeatureNameIsNotAFeature(t *testing.T) {
	// The operator gets a typo rejected rather than silently accepted.
	if KnownFeature("stram") {
		t.Fatal("a misspelt feature was accepted")
	}
	if !KnownFeature("stream") {
		t.Fatal("a real feature was rejected")
	}
}

func TestRateLimitRefusesABurst(t *testing.T) {
	// A caller that starts and cancels in a loop never trips a concurrency
	// ceiling, so starts are bounded independently.
	e, _ := testEngine(t, func(c *config.Config) { c.Defaults.RatePerMinute = 3 })
	for i := 0; i < 3; i++ {
		if _, err := e.Authorise(Request{AgentID: "codex", Prompt: "hi"}); err != nil {
			t.Fatalf("request %d refused: %v", i, err)
		}
	}
	_, err := e.Authorise(Request{AgentID: "codex", Prompt: "hi"})
	if got := refusalReason(t, err); got != ReasonRateLimited {
		t.Fatalf("reason = %q, want %q", got, ReasonRateLimited)
	}
}

func TestRateLimitWindowExpires(t *testing.T) {
	e, _ := testEngine(t, func(c *config.Config) { c.Defaults.RatePerMinute = 1 })
	if _, err := e.Authorise(Request{AgentID: "codex", Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Authorise(Request{AgentID: "codex", Prompt: "hi"}); err == nil {
		t.Fatal("the limit was not enforced")
	}
	// Move the clock on rather than sleeping a minute in a test.
	e.rate.nowFunc = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, err := e.Authorise(Request{AgentID: "codex", Prompt: "hi"}); err != nil {
		t.Fatalf("the window never expired: %v", err)
	}
}

func TestRateLimitOffWhenUnset(t *testing.T) {
	e, _ := testEngine(t, func(c *config.Config) { c.Defaults.RatePerMinute = 0 })
	for i := 0; i < 50; i++ {
		if _, err := e.Authorise(Request{AgentID: "codex", Prompt: "hi"}); err != nil {
			t.Fatalf("unlimited config refused request %d: %v", i, err)
		}
	}
}
