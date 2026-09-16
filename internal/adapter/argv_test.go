package adapter

import (
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/oxcom/agents-mcp-bridge/internal/config"
)

// The property that matters: no caller-supplied value, however hostile, can
// change the NUMBER of argv elements. If it cannot add an element, it cannot
// add a flag.
func TestHostilePromptCannotAddAnArgument(t *testing.T) {
	template := []string{"exec", "--json", "{{cwd}}", "--", "{{prompt}}"}
	hostile := []string{
		"hello world",
		"--dangerously-skip-permissions",
		"; rm -rf /",
		"$(rm -rf /)",
		"`id`",
		"a b c\nd e f",
		"--sandbox danger-full-access",
		"'; drop table users; --",
		strings.Repeat("x", 4096),
		"",
	}
	for _, p := range hostile {
		got, err := BuildArgs(template, Values{Prompt: p, CWD: "/tmp"})
		if err != nil {
			t.Fatalf("prompt %q: %v", p, err)
		}
		if len(got) != len(template) {
			t.Fatalf("prompt %q produced %d argv elements, want %d: %#v", p, len(got), len(template), got)
		}
		if got[4] != p {
			t.Fatalf("prompt %q was altered to %q", p, got[4])
		}
	}
}

func TestSandboxFlagsExpandAsAList(t *testing.T) {
	got, err := BuildArgs(
		[]string{"exec", "{{sandbox_flags}}", "-"},
		Values{SandboxFlags: []string{"--sandbox", "read-only", "-c", `approval_policy="never"`}},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--sandbox", "read-only", "-c", `approval_policy="never"`, "-"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestFlagBindingFormKeepsOneElement(t *testing.T) {
	// codex queue --message -foo fails, because the next token parses as a flag.
	got, err := BuildArgs([]string{"queue", "--message={{message}}"}, Values{Message: "-foo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != "--message=-foo" {
		t.Fatalf("got %#v", got)
	}
}

func TestPartialInterpolationRefused(t *testing.T) {
	// This is the shape that turns one value into two arguments.
	for _, bad := range []string{
		"prefix-{{prompt}}",
		"{{prompt}}-suffix",
		"{{cwd}}/{{prompt}}",
		"--flag {{prompt}}",
	} {
		if _, err := BuildArgs([]string{bad}, Values{Prompt: "x", CWD: "/tmp"}); err == nil {
			t.Errorf("%q was accepted; partial interpolation must be refused", bad)
		}
	}
}

func TestUnknownPlaceholderRefused(t *testing.T) {
	if _, err := BuildArgs([]string{"{{session_id}}"}, Values{}); err == nil {
		t.Fatal("{{session_id}} no longer exists and must be refused")
	}
}

func TestOptionalPlaceholdersVanishWhenEmpty(t *testing.T) {
	got, err := BuildArgs([]string{"exec", "{{model}}", "{{max_turns}}", "-"}, Values{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"exec", "-"}) {
		t.Fatalf("got %#v, want the optional elements omitted", got)
	}
}

func TestEmptyPromptStillProducesAnElement(t *testing.T) {
	// An empty prompt is a real argument; dropping it would shift later flags.
	got, err := BuildArgs([]string{"run", "{{prompt}}", "--last"}, Values{Prompt: ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[1] != "" {
		t.Fatalf("got %#v, want an empty element preserved", got)
	}
}

func TestResumeSandboxFallsBackButNeverToNothing(t *testing.T) {
	a := &config.Adapter{
		ID:      "codex",
		Sandbox: map[string][]string{"read-only": {"--sandbox", "read-only"}},
	}
	// No resume_sandbox declared: fall back to the run flags rather than none.
	got, err := ResumeSandboxFlags(a, config.ModeReadOnly)
	if err != nil || len(got) == 0 {
		t.Fatalf("resume must never fall back to an empty sandbox: %v %#v", err, got)
	}

	a.ResumeSandbox = map[string][]string{"read-only": {"-c", `sandbox_mode="read-only"`}}
	got, err = ResumeSandboxFlags(a, config.ModeReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "-c" {
		t.Fatalf("declared resume flags ignored: %#v", got)
	}
}

func TestSandboxFlagsRefuseSilentlyUnsandboxedRun(t *testing.T) {
	a := &config.Adapter{ID: "x", Sandbox: map[string][]string{}}
	if _, err := SandboxFlags(a, config.ModeReadOnly); err == nil {
		t.Fatal("an adapter claiming enforcement with no flags must error, not run unsandboxed")
	}
	no := false
	a.SandboxEnforced = &no
	if _, err := SandboxFlags(a, config.ModeReadOnly); err != nil {
		t.Fatalf("an explicitly unsandboxed adapter is allowed: %v", err)
	}
}

// hasEnv reports whether env carries name=value, comparing the name the way
// the platform does. Asserting on the exact spelling would be wrong on Windows,
// where the process block decides the case, not the test.
func hasEnv(env []string, name, value string) bool {
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		if envKey(kv[:eq]) == envKey(name) && kv[eq+1:] == value {
			return true
		}
	}
	return false
}

func TestBuildEnvDeniesByDefault(t *testing.T) {
	// Deny-by-default is the claim on every platform; the baseline that
	// survives it is per-platform, so assert against this OS's own list.
	t.Setenv("SECRET_TOKEN", "swordfish")

	type kv struct{ name, value string }
	var want []kv
	if runtime.GOOS == "windows" {
		want = []kv{
			{"SystemRoot", `C:\Windows`},
			{"PATH", `C:\Windows\System32`},
			{"PATHEXT", ".COM;.EXE;.BAT"},
		}
	} else {
		want = []kv{
			{"HOME", "/home/test"},
			{"PATH", "/usr/bin"},
		}
	}
	for _, w := range want {
		t.Setenv(w.name, w.value)
	}

	env := BuildEnv(nil, 0)
	if strings.Contains(strings.Join(env, "\n"), "swordfish") {
		t.Fatal("an unlisted variable reached the child")
	}
	for _, w := range want {
		if !hasEnv(env, w.name, w.value) {
			t.Fatalf("baseline variable %s missing: %v", w.name, env)
		}
	}
}

// Windows environment names are case-insensitive and a real process block
// spells them "Path", "ComSpec", "Temp". Matching the baseline case-sensitively
// there hands the child no PATH at all.
func TestBuildEnvMatchesTheBaselineTheWayThePlatformDoes(t *testing.T) {
	t.Setenv("SECRET_TOKEN", "swordfish")

	if runtime.GOOS == "windows" {
		t.Setenv("Path", `C:\Windows\System32`)
		env := BuildEnv(nil, 0)
		if !hasEnv(env, "PATH", `C:\Windows\System32`) {
			t.Fatalf("a differently-cased baseline name was dropped: %v", env)
		}
		return
	}

	// POSIX names are case-sensitive: "path" is not PATH and must be denied.
	t.Setenv("path", "/usr/bin/lowercase")
	joined := strings.Join(BuildEnv(nil, 0), "\n")
	if strings.Contains(joined, "/usr/bin/lowercase") {
		t.Fatalf("a case variant widened the POSIX allowlist:\n%s", joined)
	}
	if strings.Contains(joined, "swordfish") {
		t.Fatal("an unlisted variable reached the child")
	}
}

func TestBuildEnvHonoursAllowlist(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy:8080")
	env := BuildEnv([]string{"HTTPS_PROXY"}, 0)
	if !strings.Contains(strings.Join(env, "\n"), "HTTPS_PROXY=http://proxy:8080") {
		t.Fatal("allowlisted variable was dropped")
	}
}

func TestBuildEnvNeverForwardsBridgeState(t *testing.T) {
	// Handing the child its own run id and our runtime directory is what would
	// let it answer its own questions on the control socket.
	t.Setenv(RunIDEnvVar, "run-42")
	t.Setenv("AGENTS_BRIDGE_ANYTHING", "leak")
	env := BuildEnv([]string{RunIDEnvVar, "AGENTS_BRIDGE_ANYTHING"}, 0)
	for _, kv := range env {
		if strings.HasPrefix(kv, RunIDEnvVar+"=") || strings.HasPrefix(kv, "AGENTS_BRIDGE_ANYTHING=") {
			t.Fatalf("bridge state leaked to the child: %q", kv)
		}
	}
}

func TestDepthMarkerIncrements(t *testing.T) {
	env := BuildEnv(nil, 2)
	want := DepthEnvVar + "=3"
	if !strings.Contains(strings.Join(env, "\n"), want) {
		t.Fatalf("expected %q in %v", want, env)
	}
}

func TestDepthFromEnvTreatsGarbageAsZero(t *testing.T) {
	// The marker is best-effort: a missing or corrupt value means "no evidence
	// of nesting", never "refuse to run".
	for _, v := range []string{"", "banana", "-5"} {
		os.Setenv(DepthEnvVar, v)
		if got := DepthFromEnv(); got != 0 {
			t.Errorf("DepthFromEnv(%q) = %d, want 0", v, got)
		}
	}
	os.Setenv(DepthEnvVar, "3")
	if got := DepthFromEnv(); got != 3 {
		t.Errorf("DepthFromEnv(3) = %d", got)
	}
	os.Unsetenv(DepthEnvVar)
}

func TestGateFlagsExpandAsWholeElements(t *testing.T) {
	got, err := BuildArgs([]string{"-p", "{{gate_flags}}", "--", "{{prompt}}"}, Values{
		Prompt:    "hi",
		GateFlags: []string{"--permission-prompts", "host"},
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	want := []string{"-p", "--permission-prompts", "host", "--", "hi"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestGateFlagsAbsentCollapseToNothing(t *testing.T) {
	got, err := BuildArgs([]string{"-p", "{{gate_flags}}", "--", "{{prompt}}"}, Values{Prompt: "hi"})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	want := []string{"-p", "--", "hi"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestGateConfigExpandsAsAWholeElement(t *testing.T) {
	got, err := BuildArgs([]string{"--mcp-config", "{{gate_config}}"}, Values{GateConfig: "/run/agents-bridge/gate-123.json"})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	want := []string{"--mcp-config", "/run/agents-bridge/gate-123.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestInteractiveFlagsReturnsFlagsForMode(t *testing.T) {
	a := &config.Adapter{
		ID: "claude",
		Interactive: map[string][]string{
			"read-only": {"--permission-prompt-tool", "mcp__bridge_gate__ask"},
		},
	}
	got, err := InteractiveFlags(a, config.ModeReadOnly, "")
	if err != nil {
		t.Fatalf("InteractiveFlags: %v", err)
	}
	want := []string{"--permission-prompt-tool", "mcp__bridge_gate__ask"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("InteractiveFlags = %q, want %q", got, want)
	}
}

func TestInteractiveFlagsAbsentIsLegal(t *testing.T) {
	a := &config.Adapter{ID: "codex"}
	got, err := InteractiveFlags(a, config.ModeReadOnly, "")
	if err != nil {
		t.Fatalf("InteractiveFlags: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("InteractiveFlags = %q, want empty", got)
	}
}

func TestInteractiveFlagsExpandsGateConfigPlaceholder(t *testing.T) {
	a := &config.Adapter{
		ID: "claude",
		Interactive: map[string][]string{
			"read-only": {"--mcp-config", "{{gate_config}}", "--permission-prompts", "host"},
		},
	}
	got, err := InteractiveFlags(a, config.ModeReadOnly, "/run/agents-bridge/gate-123.json")
	if err != nil {
		t.Fatalf("InteractiveFlags: %v", err)
	}
	want := []string{"--mcp-config", "/run/agents-bridge/gate-123.json", "--permission-prompts", "host"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("InteractiveFlags = %q, want %q", got, want)
	}
}

func TestInteractiveFlagsRefusesPartialInterpolation(t *testing.T) {
	a := &config.Adapter{
		ID: "claude",
		Interactive: map[string][]string{
			"read-only": {"--mcp-config={{gate_config}}x"},
		},
	}
	if _, err := InteractiveFlags(a, config.ModeReadOnly, "/run/agents-bridge/gate-123.json"); err == nil {
		t.Fatal("expected a partial-interpolation error, got none")
	}
}

func TestCredentialsInTheParentEnvironmentNeverReachTheChild(t *testing.T) {
	// The bridge itself may well be running with these set; none of them is on
	// the baseline, so none may be inherited.
	for _, name := range []string{
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY", "GH_TOKEN", "GITHUB_TOKEN", "NPM_TOKEN",
	} {
		t.Setenv(name, "leaked-value-"+name)
	}
	joined := strings.Join(BuildEnv(nil, 0), "\n")
	if strings.Contains(joined, "leaked-value-") {
		t.Fatalf("a credential from the parent environment reached the child:\n%s", joined)
	}
}
