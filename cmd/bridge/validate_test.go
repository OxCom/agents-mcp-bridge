package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// skipPermCheck is what these tests pass to validate's
// --insecure-skip-permission-check. internal/config/perm_windows.go refuses
// every config file unconditionally until the DACL check lands (v1.1), so no
// fixture can satisfy it there and the flag is the only way to reach the checks
// under test. On POSIX the fixture does satisfy the real check, so it stays on
// and the tests keep exercising the loader's permission path.
var skipPermCheck = runtime.GOOS == "windows"

// validateArgs builds runValidate's argv with the same skip decision.
func validateArgs(path string) []string {
	args := []string{"--config", path}
	if skipPermCheck {
		args = append(args, "--insecure-skip-permission-check")
	}
	return args
}

// TestMain lets the test binary answer to the stub-agent verb, so `bridge
// validate` can spawn it exactly as it spawns the release binary. Without
// this, os.Executable() inside a test is the test binary, whose main() is go
// test's own and knows nothing about the verb.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == stubAgentVerb {
		if err := runStubAgent(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// writeConfig materialises a config the real loader will accept: owner-only
// mode in an owner-only directory, with allowed_roots pointing somewhere that
// exists on this machine.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	// The loader refuses a group- or world-writable config directory, and
	// t.TempDir is 0775 on some machines. Narrowing it is part of the fixture,
	// not a relaxation of the check. POSIX only: on Windows the mode grants
	// nothing and the loader refuses regardless, hence skipPermCheck.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(body, "ROOT", root)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func self(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// findCheck returns the named check from the first adapter, or from the
// config-level list when no adapter was reached.
func findCheck(t *testing.T, r *validateReport, name string) check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	for _, a := range r.Adapters {
		for _, c := range a.Checks {
			if c.Name == name {
				return c
			}
		}
	}
	t.Fatalf("no check named %q in the report", name)
	return check{}
}

const healthyConfig = `
version: 1
allowed_roots:
  - ROOT
defaults:
  timeout_s: 30
  max_output_bytes: 32768
  max_concurrent_runs: 2
agents:
  thirdparty:
    description: A third-party read-only adapter.
    command: thirdparty-cli
    tier: basic
    mode: read-only
    sandbox:
      read-only:
        - "--safe"
        - "--no-write"
    invoke:
      args: ["exec", "{{sandbox_flags}}", "--cd", "{{cwd}}"]
      prompt: stdin
`

func TestValidateReportsAHealthyAdapterAsPassing(t *testing.T) {
	path := writeConfig(t, healthyConfig)
	r := buildValidateReport(path, self(t), "", "", skipPermCheck)

	if n := r.failures(); n != 0 {
		t.Fatalf("%d check(s) failed on a healthy adapter:\n%s", n, renderForTest(r))
	}
	if len(r.Adapters) != 1 || r.Adapters[0].Agent != "thirdparty" {
		t.Fatalf("report does not describe the adapter: %+v", r.Adapters)
	}
	// The three checks that can only pass if a real child actually ran.
	for _, name := range []string{"prompt-delivery", "timeout", "exit-code"} {
		if c := findCheck(t, r, name); c.Status != statusPass {
			t.Errorf("%s: %s %s", name, c.Status, c.Detail)
		}
	}
	if c := findCheck(t, r, "prompt-delivery"); !strings.Contains(c.Detail, "stdin") {
		t.Errorf("the declared prompt mode is not named in the report: %s", c.Detail)
	}
}

// A partially interpolated placeholder is the failure adapter.BuildArgs
// exists to catch: it is the one way a caller-supplied value could become two
// argv elements. The loader accepts it (the placeholder itself is known), so
// only a build proves it wrong.
const brokenInvokeConfig = `
version: 1
allowed_roots:
  - ROOT
defaults:
  timeout_s: 30
  max_output_bytes: 32768
  max_concurrent_runs: 2
agents:
  thirdparty:
    description: An adapter whose invoke template interpolates into a larger string.
    command: thirdparty-cli
    tier: basic
    mode: read-only
    sandbox:
      read-only:
        - "--safe"
    invoke:
      args: ["exec", "{{sandbox_flags}}", "--task={{prompt}}-suffix"]
      prompt: argv
`

func TestValidateFailsAnAdapterWithABrokenInvokeTemplate(t *testing.T) {
	path := writeConfig(t, brokenInvokeConfig)
	r := buildValidateReport(path, self(t), "", "", skipPermCheck)

	c := findCheck(t, r, "argv")
	if c.Status != statusFail {
		t.Fatalf("argv check did not fail:\n%s", renderForTest(r))
	}
	if !strings.Contains(c.Detail, "interpolates a placeholder into a larger string") {
		t.Errorf("the report does not say what is wrong with the template: %s", c.Detail)
	}
	if r.failures() == 0 {
		t.Fatal("a broken template produced no failure")
	}
	// runValidate's error is what main turns into exit status 1.
	if err := runValidate(validateArgs(path)); err == nil {
		t.Fatal("bridge validate exited 0 on a broken adapter")
	}
}

// Rule 21: the child needs a tool to raise a question with. An adapter that
// claims interactive without it looks fully wired in config and has a dead
// trigger at runtime.
const interactiveWithoutQuestionToolConfig = `
version: 1
allowed_roots:
  - ROOT
defaults:
  timeout_s: 30
  max_output_bytes: 32768
  max_concurrent_runs: 2
agents:
  claude:
    description: An adapter claiming interactive with no question tool.
    command: claude-cli
    tier: full
    mode: read-only
    capabilities:
      stream: true
      interactive: true
    sandbox:
      read-only:
        - "--safe"
    interactive:
      read-only:
        - "--mcp-config"
        - "{{gate_config}}"
    invoke:
      args: ["exec", "{{sandbox_flags}}", "{{gate_flags}}"]
      prompt: stdin
`

func TestValidateFailsAnInteractiveAdapterWithNoQuestionTool(t *testing.T) {
	path := writeConfig(t, interactiveWithoutQuestionToolConfig)
	r := buildValidateReport(path, self(t), "", "", skipPermCheck)

	c := findCheck(t, r, "config")
	if c.Status != statusFail {
		t.Fatalf("config check did not fail:\n%s", renderForTest(r))
	}
	if !strings.Contains(c.Detail, "AskUserQuestion") || !strings.Contains(c.Detail, "rule 21") {
		t.Errorf("the report does not name the rule that refused the adapter: %s", c.Detail)
	}
	if err := runValidate(validateArgs(path)); err == nil {
		t.Fatal("bridge validate exited 0 on an adapter the loader refuses")
	}
}

func TestValidateUnknownAgentIsReported(t *testing.T) {
	path := writeConfig(t, healthyConfig)
	r := buildValidateReport(path, self(t), "nosuch", "", skipPermCheck)
	if c := findCheck(t, r, "agent"); c.Status != statusFail {
		t.Fatalf("an unknown --agent was not reported: %+v", c)
	}
}

func TestValidateOnlyRunsTheRequestedAgent(t *testing.T) {
	path := writeConfig(t, healthyConfig)
	r := buildValidateReport(path, self(t), "thirdparty", "", skipPermCheck)
	if len(r.Adapters) != 1 {
		t.Fatalf("--agent did not narrow the report: %d adapter(s)", len(r.Adapters))
	}
	if r.failures() != 0 {
		t.Fatalf("healthy adapter failed:\n%s", renderForTest(r))
	}
}

// The stub ships in the release binary. It must not be advertised: an
// operator has no reason to run it, and a verb in the help text is an
// invitation to point something else at it.
func TestStubAgentIsNotAdvertised(t *testing.T) {
	if strings.Contains(usage, stubAgentVerb) {
		t.Fatal("stub-agent appears in the usage text")
	}
	if !strings.Contains(usage, "bridge validate") {
		t.Fatal("bridge validate is missing from the usage text")
	}
}

func TestStubAgentEchoesArgvAndStdin(t *testing.T) {
	var out strings.Builder
	if err := writeStubEcho([]string{"--flag", "value"}, strings.NewReader("a prompt"), &out); err != nil {
		t.Fatal(err)
	}
	echo, ok := parseStubEcho(out.String())
	if !ok {
		t.Fatalf("the echo record does not parse: %s", out.String())
	}
	if echo.Stdin != "a prompt" || len(echo.Argv) != 2 || echo.Argv[1] != "value" {
		t.Fatalf("echo lost its inputs: %+v", echo)
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("the echo record must be one line so sanitisation keeps it whole: %q", out.String())
	}
}

func renderForTest(r *validateReport) string {
	var sb strings.Builder
	printValidateReport(&sb, r)
	return sb.String()
}
