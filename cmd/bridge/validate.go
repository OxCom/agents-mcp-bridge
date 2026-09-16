package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/oxcom/agents-mcp-bridge/internal/adapter"
	"github.com/oxcom/agents-mcp-bridge/internal/audit"
	"github.com/oxcom/agents-mcp-bridge/internal/config"
	"github.com/oxcom/agents-mcp-bridge/internal/gate"
	"github.com/oxcom/agents-mcp-bridge/internal/platform"
	"github.com/oxcom/agents-mcp-bridge/internal/policy"
	"github.com/oxcom/agents-mcp-bridge/internal/run"
	"github.com/oxcom/agents-mcp-bridge/internal/stream"
)

// Check statuses. A FAIL is the only one that changes the exit code: SKIP
// means the check does not apply to this adapter, which is not a fault.
const (
	statusPass = "PASS"
	statusFail = "FAIL"
	statusSkip = "SKIP"
	// statusWarn is not a fault and never changes the exit code. It exists so
	// that a check the operator disabled is stated in the report rather than
	// being absent from it.
	statusWarn = "WARN"
)

// check is one reported result. Detail is one line: the report is meant to be
// read in a terminal and diffed in CI, not paged.
type check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type adapterReport struct {
	Agent  string  `json:"agent"`
	Tier   string  `json:"tier"`
	Mode   string  `json:"mode"`
	Checks []check `json:"checks"`
}

type validateReport struct {
	OK       bool            `json:"ok"`
	Config   string          `json:"config"`
	Checks   []check         `json:"checks"` // config-level, before any adapter
	Adapters []adapterReport `json:"adapters"`
}

func (r *validateReport) failures() int {
	n := 0
	for _, c := range r.Checks {
		if c.Status == statusFail {
			n++
		}
	}
	for _, a := range r.Adapters {
		for _, c := range a.Checks {
			if c.Status == statusFail {
				n++
			}
		}
	}
	return n
}

func pass(name, format string, a ...any) check {
	return check{Name: name, Status: statusPass, Detail: fmt.Sprintf(format, a...)}
}

func fail(name, format string, a ...any) check {
	return check{Name: name, Status: statusFail, Detail: fmt.Sprintf(format, a...)}
}

func skip(name, format string, a ...any) check {
	return check{Name: name, Status: statusSkip, Detail: fmt.Sprintf(format, a...)}
}

func warn(name, format string, a ...any) check {
	return check{Name: name, Status: statusWarn, Detail: fmt.Sprintf(format, a...)}
}

// probePrompt is what the stub is asked to echo back. It is deliberately
// unlike anything an adapter's own flags contain, so finding it in the echo
// record proves delivery rather than coincidence.
const probePrompt = "bridge-validate-probe-7f3a1c"

// hostilePrompt is the second argv-build input. Every character in it is one a
// shell would act on, which is exactly why the bridge has no shell: the check
// is that none of them changes the number of argv elements.
const hostilePrompt = "a b\"c'd\n; rm -rf / $(id) `id` --allow-all | tee /tmp/x"

// runValidate exercises each adapter in a config against the stub agent, so a
// third-party adapter declaration can be proven — or disproven — without a
// vendor CLI, credentials or a network.
//
// The substitution is deliberately narrow: only the adapter's resolved command
// changes, to this binary plus `stub-agent`. The author's flags, placeholders,
// prompt mode, sandbox lists, tier and mode are used exactly as written, and
// every run goes through the same internal/policy, internal/run and
// internal/sanitize path a real delegation does. Nothing here bypasses a
// decision point; where a check cannot be expressed without changing one, the
// report says so instead.
func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file to validate (required)")
	agentID := fs.String("agent", "", "validate only this agent id")
	fixture := fs.String("stream-fixture", "", "recorded vendor stream to drive a tier: full adapter's parser")
	asJSON := fs.Bool("json", false, "print the report as one JSON object")
	// Windows refuses every config load until the DACL check lands (v1.1), so
	// without this there is no way to exercise an adapter there at all. It is
	// validate-only and never silent: serve has no such flag, and the report
	// carries a WARN line naming the file that was trusted without proof.
	skipPerm := fs.Bool("insecure-skip-permission-check", false,
		"load the config without checking that only its owner can write it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return fmt.Errorf("validate needs --config <path>")
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot locate this binary, which is the stub agent: %w", err)
	}

	report := buildValidateReport(*configPath, self, *agentID, *fixture, *skipPerm)
	report.OK = report.failures() == 0

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		printValidateReport(os.Stdout, report)
	}
	if n := report.failures(); n > 0 {
		return fmt.Errorf("%d check(s) failed", n)
	}
	return nil
}

func buildValidateReport(configPath, self, only, fixture string, skipPerm bool) *validateReport {
	report := &validateReport{Config: configPath}
	if skipPerm {
		report.Checks = append(report.Checks, warn("config-perm",
			"permission check skipped: %s is trusted without proof that only its owner can write it", configPath))
	}

	// The real loader, so all of the schema and every semantic rule applies.
	// Only the executable resolution is substituted: an adapter is validated
	// for what it declares, and the machine running `bridge validate` is not
	// required to have the vendor CLI installed at all. `bridge doctor` is
	// what checks the real binary.
	cfg, err := config.Load(configPath, config.Options{
		LookPath:            func(string) (string, error) { return self, nil },
		SkipPermissionCheck: skipPerm,
	})
	if err != nil {
		report.Checks = append(report.Checks, fail("config", "%v", err))
		return report
	}

	ids := make([]string, 0, len(cfg.Agents))
	for id := range cfg.Agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var missing []string
	for _, id := range ids {
		if _, err := exec.LookPath(cfg.Agents[id].Command); err != nil {
			missing = append(missing, cfg.Agents[id].Command)
		}
	}
	detail := fmt.Sprintf("loaded; %d adapter(s); schema and all semantic rules applied", len(ids))
	if len(missing) > 0 {
		detail += fmt.Sprintf("; not on this PATH (not needed here, see bridge doctor): %s",
			strings.Join(missing, ", "))
	}
	report.Checks = append(report.Checks, pass("config", "%s", detail))

	if only != "" {
		if _, ok := cfg.Agents[only]; !ok {
			report.Checks = append(report.Checks,
				fail("agent", "no adapter %q in this config (after self-exclusion)", only))
			return report
		}
		ids = []string{only}
	}

	scratch, err := os.MkdirTemp("", "bridge-validate-")
	if err != nil {
		report.Checks = append(report.Checks, fail("config", "scratch directory: %v", err))
		return report
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	v := &validator{cfg: cfg, self: self, scratch: scratch, fixture: fixture}
	for _, id := range ids {
		report.Adapters = append(report.Adapters, v.validateAdapter(cfg.Agents[id]))
	}
	return report
}

// validator holds what every adapter's checks share.
type validator struct {
	cfg     *config.Config
	self    string
	scratch string
	fixture string
	// argvOK records whether the current adapter's argv check passed. Every
	// later check starts a real run, which fails for the same reason and
	// reports it against the expanded argv's indices rather than the
	// template's; without this they would print one fault as several,
	// numbered differently each time.
	argvOK bool
}

// errArgvAlreadyReported makes a dependent check defer to the argv check
// instead of restating its failure with a different index base.
var errArgvAlreadyReported = errors.New("argv did not build; see the argv check above")

func (v *validator) validateAdapter(a *config.Adapter) adapterReport {
	rep := adapterReport{Agent: a.ID, Tier: string(a.Tier), Mode: string(a.Mode)}

	argvCheck, built := v.checkArgv(a)
	v.argvOK = argvCheck.Status == statusPass
	rep.Checks = append(rep.Checks, argvCheck)
	rep.Checks = append(rep.Checks, v.checkSandbox(a, built))

	promptCheck, runCheck := v.checkPromptAndRun(a)
	rep.Checks = append(rep.Checks, promptCheck, runCheck)
	rep.Checks = append(rep.Checks, v.checkTimeout(a))
	rep.Checks = append(rep.Checks, v.checkExit(a))
	rep.Checks = append(rep.Checks, v.checkParser(a))
	rep.Checks = append(rep.Checks, v.checkInteractive(a))
	return rep
}

// checkArgv proves the two properties SR-4 rests on: every placeholder the
// author used expands, and no caller-supplied value can change the shape of
// the command line. It returns the argv built with the benign prompt, which
// the sandbox check then inspects.
func (v *validator) checkArgv(a *config.Adapter) (check, []string) {
	const name = "argv"
	if a.Invoke == nil {
		return fail(name, "no invoke block"), nil
	}
	values := func(prompt string) adapter.Values {
		return adapter.Values{
			Prompt:       prompt,
			CWD:          v.cfg.AllowedRoots[0],
			SandboxFlags: a.Sandbox[string(a.Mode)],
		}
	}
	benign, err := adapter.BuildArgs(a.Invoke.Args, values(probePrompt))
	if err != nil {
		return fail(name, "%v", err), nil
	}
	hostile, err := adapter.BuildArgs(a.Invoke.Args, values(hostilePrompt))
	if err != nil {
		return fail(name, "with a hostile prompt: %v", err), benign
	}
	if len(benign) != len(hostile) {
		return fail(name, "a caller-supplied prompt changed the argv element count (%d then %d)",
			len(benign), len(hostile)), benign
	}
	carriers := 0
	for _, arg := range hostile {
		if strings.Contains(arg, hostilePrompt) {
			carriers++
		}
	}
	if a.Invoke.Prompt == "argv" && carriers != 1 {
		return fail(name, "prompt: argv, but a hostile prompt landed in %d argv element(s), not 1", carriers), benign
	}
	return pass(name, "%d template element(s) -> %d argv element(s); a hostile prompt changes neither the count nor the element boundaries",
		len(a.Invoke.Args), len(benign)), benign
}

// checkSandbox reports whether the flags the author declared for this
// adapter's mode actually reach the child, and states an undeclared sandbox
// loudly rather than quietly passing it.
func (v *validator) checkSandbox(a *config.Adapter, built []string) check {
	const name = "sandbox"
	mode := string(a.Mode)
	if !a.SandboxEnforcedOrDefault() {
		return pass(name, "UNSANDBOXED: sandbox_enforced is false, so the vendor confines mode %s not at all; "+
			"only allowed_roots and the worktree stand between this adapter and the tree", mode)
	}
	flags := a.Sandbox[mode]
	if len(flags) == 0 {
		return fail(name, "no sandbox flags for mode %s and sandbox_enforced is not false (rule 5)", mode)
	}
	if built == nil {
		return skip(name, "argv did not build, so the flags could not be located in it")
	}
	present := make(map[string]bool, len(built))
	for _, arg := range built {
		present[arg] = true
	}
	var absent []string
	for _, f := range flags {
		if !present[f] {
			absent = append(absent, f)
		}
	}
	if len(absent) > 0 {
		return fail(name, "sandbox.%s declares %s, which never reaches the argv; does invoke.args reference {{sandbox_flags}}?",
			mode, strings.Join(absent, " "))
	}
	return pass(name, "all %d sandbox flag(s) for mode %s are present in the built argv", len(flags), mode)
}

// checkPromptAndRun starts one real run and reads two things off it: that the
// prompt arrived by the declared mode, and that the result came back
// sanitized and enveloped.
func (v *validator) checkPromptAndRun(a *config.Adapter) (check, check) {
	const pname, rname = "prompt-delivery", "run-envelope"

	res, err := v.exercise(a, nil, probePrompt, 0, false)
	if err != nil {
		return skip(pname, "%v", err), skip(rname, "%v", err)
	}

	runCheck := pass(rname, "state %s; output sanitized and wrapped in untrusted_agent_output", res.state)
	switch {
	case !strings.Contains(res.text, "<untrusted_agent_output") ||
		!strings.HasSuffix(strings.TrimSpace(res.text), "</untrusted_agent_output>"):
		runCheck = fail(rname, "the result was not enveloped: %s", firstLine(res.text))
	case res.state != string(run.StateCompleted):
		runCheck = fail(rname, "run state %s, want completed: %s", res.state, tail(res.text))
	}

	echo, ok := parseStubEcho(res.text)
	if !ok {
		return fail(pname, "the stub's echo record never reached the caller; the prompt cannot be traced: %s",
			tail(res.text)), runCheck
	}
	mode := a.Invoke.Prompt
	switch mode {
	case "argv":
		for _, arg := range echo.Argv {
			if strings.Contains(arg, probePrompt) {
				return pass(pname, "prompt: argv — the probe arrived as an argv element"), runCheck
			}
		}
		return fail(pname, "prompt: argv — the probe is in none of the %d argv element(s) the child received",
			len(echo.Argv)), runCheck
	case "stdin":
		if echo.Stdin == probePrompt {
			return pass(pname, "prompt: stdin — the child read exactly the %d prompt byte(s) and nothing else",
				echo.StdinBytes), runCheck
		}
		return fail(pname, "prompt: stdin — the child read %d byte(s) that are not the prompt", echo.StdinBytes), runCheck
	case "stdin_stream_json":
		if strings.Contains(echo.Stdin, probePrompt) {
			return pass(pname, "prompt: stdin_stream_json — the probe arrived inside the %d byte(s) of the opening record",
				echo.StdinBytes), runCheck
		}
		return fail(pname, "prompt: stdin_stream_json — the probe is absent from the %d byte(s) on stdin",
			echo.StdinBytes), runCheck
	default:
		return fail(pname, "unknown prompt mode %q", mode), runCheck
	}
}

// checkTimeout proves a slow child is killed at the deadline and reported,
// rather than hanging the caller. The deadline is clamped to 2s for this check
// alone: the plumbing under test is spec.Timeout reaching the process group,
// and waiting out a production timeout_s would make validation unusable.
func (v *validator) checkTimeout(a *config.Adapter) check {
	const name = "timeout"
	res, err := v.exercise(a, []string{"--sleep", "60s"}, probePrompt, 2, false)
	if err != nil {
		return skip(name, "%v", err)
	}
	if res.state != string(run.StateFailed) {
		return fail(name, "a child that outlived the deadline ended in state %s, not failed", res.state)
	}
	if !strings.Contains(res.text, "timed out after") {
		return fail(name, "the run failed but the caller was not told it was a timeout: %s", tail(res.text))
	}
	return pass(name, "a child sleeping past timeout_s (clamped to 2s here) was killed and reported as a timeout")
}

// checkExit proves a non-zero exit is a failed run, not a completed one with
// empty output.
func (v *validator) checkExit(a *config.Adapter) check {
	const name = "exit-code"
	res, err := v.exercise(a, []string{"--exit", "3"}, probePrompt, 0, false)
	if err != nil {
		return skip(name, "%v", err)
	}
	if res.state != string(run.StateFailed) {
		return fail(name, "a child exiting 3 ended in state %s, not failed", res.state)
	}
	if !strings.Contains(res.text, "exited with status 3") {
		return fail(name, "the run failed but the exit status was not reported: %s", tail(res.text))
	}
	return pass(name, "exit status 3 reported to the caller as a failed run, with its partial output kept")
}

// checkParser covers the tier: full promise. A full-tier adapter whose parser
// is missing would silently degrade to buffered output while claiming to
// stream, which is why it is fatal at startup — this reports it before an
// operator hits it.
func (v *validator) checkParser(a *config.Adapter) check {
	const name = "parser"
	if a.Tier != config.TierFull {
		return skip(name, "tier: basic — buffered output, no parser to register")
	}
	if _, ok := stream.ParserFor(a.ID); !ok {
		return fail(name, "tier: full, but this build registers no stream.Parser for agent id %q", a.ID)
	}
	if v.fixture == "" {
		return pass(name, "stream.Parser registered for %q; pass --stream-fixture <file> to drive it from a recorded stream", a.ID)
	}
	if !v.cfg.Features.Enabled("stream") {
		return skip(name, "parser registered, but features.stream is off in this config, so no run parses anything")
	}
	fixture, err := filepath.Abs(v.fixture)
	if err != nil {
		return fail(name, "--stream-fixture: %v", err)
	}
	res, err := v.exercise(a, []string{"--emit", fixture}, probePrompt, 0, true)
	if err != nil {
		return skip(name, "%v", err)
	}
	if res.transcript == "" {
		return fail(name, "the run recorded no transcript, so the parser produced nothing to read")
	}
	events, err := stream.ReadTranscript(res.transcript)
	if err != nil {
		return fail(name, "transcript unreadable: %v", err)
	}
	if len(events) == 0 {
		return fail(name, "%s produced no events through this parser", filepath.Base(fixture))
	}
	kinds := map[stream.Kind]int{}
	for _, e := range events {
		kinds[e.Kind]++
	}
	if kinds[stream.KindUnknown] == len(events) {
		return fail(name, "%s parsed into %d event(s), every one of them vendor.raw: the fixture does not match this parser",
			filepath.Base(fixture), len(events))
	}
	return pass(name, "%s parsed into %d event(s) without error (%s)",
		filepath.Base(fixture), len(events), describeKinds(kinds))
}

// checkInteractive re-asserts rules 13 and 18-21 here rather than trusting
// that the load implied them, so the report names the rule an interactive
// adapter would trip. The loader enforces them as load errors; a disagreement
// between the two is itself reportable.
func (v *validator) checkInteractive(a *config.Adapter) check {
	const name = "interactive"
	if !a.Capabilities.Interactive {
		return skip(name, "capabilities.interactive is false")
	}
	mode := string(a.Mode)
	switch {
	case !a.Capabilities.Stream:
		return fail(name, "rule 13: interactive: true requires stream: true — a question cannot be detected in an unparsed stream")
	case len(a.Interactive[mode]) == 0:
		return fail(name, "rule 18: no interactive flag list for mode %s, so questions have no route in", mode)
	case a.ResumeInvoke != nil && !mentionsInFlags([][]string{a.ResumeInvoke.Args}, "{{gate_flags}}"):
		return fail(name, "rule 19: resume_invoke.args does not reference {{gate_flags}}; a resumed run loses its gate routing")
	case a.Mode == config.ModeWrite:
		return fail(name, "rule 20: interactive: true with mode: write — v1 denies every approval, so every write call would be refused")
	case !mentionsInFlags(interactiveFlagLists(a), "AskUserQuestion"):
		return fail(name, "rule 21: AskUserQuestion appears in no flag list for mode %s, so the child has no tool to ask with", mode)
	}
	return pass(name, "rules 13, 18, 19, 20 and 21 hold for mode %s", mode)
}

func interactiveFlagLists(a *config.Adapter) [][]string {
	mode := string(a.Mode)
	lists := [][]string{a.Sandbox[mode], a.ResumeSandbox[mode], a.Interactive[mode]}
	if a.Invoke != nil {
		lists = append(lists, a.Invoke.Args)
	}
	if a.ResumeInvoke != nil {
		lists = append(lists, a.ResumeInvoke.Args)
	}
	return lists
}

func mentionsInFlags(lists [][]string, want string) bool {
	for _, list := range lists {
		for _, arg := range list {
			if strings.Contains(arg, want) {
				return true
			}
		}
	}
	return false
}

// runResult is what one exercised run tells the validator. text is the
// caller-facing tool result: sanitized, enveloped, exactly what a delegating
// model would have received.
type runResult struct {
	state      string
	text       string
	transcript string
}

// exercise runs the adapter once against the stub, through the real ask and
// await path.
//
// lead is prepended to the adapter's own args as stub-agent flags; timeoutS,
// when non-zero, clamps defaults.timeout_s for this run only; wantStream
// leaves the stream feature as the config has it, and otherwise the feature is
// narrowed off so the stub's echo record reaches the caller instead of being
// consumed by a parser that has never seen it. Narrowing a feature is always
// permitted; nothing here widens one.
func (v *validator) exercise(a *config.Adapter, lead []string, prompt string, timeoutS int, wantStream bool) (runResult, error) {
	b, cleanup, err := v.newStubBridge(a, lead, timeoutS, wantStream)
	if err != nil {
		return runResult{}, err
	}
	defer cleanup()

	ctx := context.Background()
	_, started, err := b.makeAsk(a.ID)(ctx, nil, askInput{Prompt: prompt})
	if err != nil {
		// The argv check reports the same defect against the template's own
		// element indices; repeating the refusal here would print a second,
		// differently-numbered index for one fault and read like two.
		if !v.argvOK {
			return runResult{}, errArgvAlreadyReported
		}
		return runResult{}, fmt.Errorf("the run was refused before it started: %w", err)
	}
	transcript := ""
	if r, err := b.runs.Get(started.RunID); err == nil {
		transcript = r.TranscriptPath
	}
	res, out, err := b.awaitAgent(ctx, nil, awaitInput{RunID: started.RunID, TimeoutS: 60})
	if err != nil {
		return runResult{}, fmt.Errorf("await failed: %w", err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return runResult{state: out.State, text: sb.String(), transcript: transcript}, nil
}

// newStubBridge wires the real components — policy engine, run registry,
// audit writer, sanitizer — around one adapter whose command has been replaced
// by this binary's stub agent. It is the production bridge struct: the checks
// go through the same code a delegation from a host agent does.
func (v *validator) newStubBridge(a *config.Adapter, lead []string, timeoutS int, wantStream bool) (*bridge, func(), error) {
	cfg := *v.cfg // Defaults and Features are values; copying Config copies them
	if timeoutS > 0 {
		cfg.Defaults.TimeoutS = timeoutS
	}
	if !wantStream {
		off := false
		cfg.Features.Stream = &off
	}

	stub := *a
	invoke := *a.Invoke
	args := make([]string, 0, 1+len(lead)+len(a.Invoke.Args))
	args = append(args, stubAgentVerb)
	args = append(args, lead...)
	args = append(args, a.Invoke.Args...)
	invoke.Args = args
	stub.Invoke = &invoke
	stub.ResolvedCommand = v.self
	cfg.Agents = map[string]*config.Adapter{a.ID: &stub}

	engine, err := policy.New(&cfg, platform.NewPathGuard(), "", 0)
	if err != nil {
		return nil, nil, fmt.Errorf("the policy engine will not start for this config: %w", err)
	}

	dir, err := os.MkdirTemp(v.scratch, "run-")
	if err != nil {
		return nil, nil, err
	}
	stateDir := filepath.Join(dir, "state")
	transcriptDir := filepath.Join(dir, "transcripts")
	for _, d := range []string{stateDir, transcriptDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, nil, err
		}
	}
	aw, err := audit.New(audit.Options{
		Path:    filepath.Join(dir, "audit.jsonl"),
		KeyFile: filepath.Join(dir, "audit.key"),
	})
	if err != nil {
		return nil, nil, err
	}

	b := &bridge{
		cfg:           &cfg,
		engine:        engine,
		runs:          run.NewRegistry(2, time.Hour),
		audit:         aw,
		host:          "",
		stateDir:      stateDir,
		runtimeDir:    dir,
		transcriptDir: transcriptDir,
		changes:       newChangeStore(),
		gates:         make(map[string]*gate.Server),
		awaiting:      make(map[string]*mcp.CallToolRequest),
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	cleanup := func() {
		b.runs.CancelAll()
		b.changes.discardAll()
		_ = aw.Close()
	}
	return b, cleanup, nil
}

// parseStubEcho pulls the stub's one-line record out of the enveloped result.
// The envelope's preamble and the adapter's own output surround it, so the
// line is located by its marker rather than by position.
func parseStubEcho(enveloped string) (stubEcho, bool) {
	for _, line := range strings.Split(enveloped, "\n") {
		if !strings.Contains(line, stubMarker) {
			continue
		}
		start := strings.IndexByte(line, '{')
		if start < 0 {
			continue
		}
		var e stubEcho
		if err := json.Unmarshal([]byte(line[start:]), &e); err == nil && e.StubAgent == 1 {
			return e, true
		}
	}
	return stubEcho{}, false
}

func describeKinds(kinds map[stream.Kind]int) string {
	names := make([]string, 0, len(kinds))
	for k := range kinds {
		names = append(names, string(k))
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s×%d", n, kinds[stream.Kind(n)]))
	}
	return strings.Join(parts, ", ")
}

// tail returns the end of a result for an error line, where the interesting
// part (the failure note the envelope appends) lives.
func tail(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "</untrusted_agent_output>")
	s = strings.TrimSpace(s)
	if len(s) > 160 {
		s = "…" + s[len(s)-160:]
	}
	return strings.ReplaceAll(s, "\n", " ")
}

// printValidateReport renders the plain report, in the same register as
// bridge doctor's: one line per finding, status first.
func printValidateReport(w io.Writer, r *validateReport) {
	_, _ = fmt.Fprintf(w, "config  %s\n\n", r.Config)
	for _, c := range r.Checks {
		_, _ = fmt.Fprintf(w, "  %-4s  %-16s  %s\n", c.Status, c.Name, c.Detail)
	}
	total, passed, failed, skipped := 0, 0, 0, 0
	for _, c := range r.Checks {
		total, passed, failed, skipped = tally(c, total, passed, failed, skipped)
	}
	for _, a := range r.Adapters {
		_, _ = fmt.Fprintf(w, "\nadapter %s  (tier %s, mode %s)\n", a.Agent, a.Tier, a.Mode)
		for _, c := range a.Checks {
			_, _ = fmt.Fprintf(w, "  %-4s  %-16s  %s\n", c.Status, c.Name, c.Detail)
			total, passed, failed, skipped = tally(c, total, passed, failed, skipped)
		}
	}
	_, _ = fmt.Fprintf(w, "\n%d adapter(s), %d check(s): %d passed, %d failed, %d skipped\n",
		len(r.Adapters), total, passed, failed, skipped)
	if failed == 0 {
		_, _ = fmt.Fprintln(w, "\nno failures")
	}
}

func tally(c check, total, passed, failed, skipped int) (int, int, int, int) {
	// A WARN is a notice about how validate was invoked, not a check result, so
	// it is printed but does not move the counts.
	if c.Status == statusWarn {
		return total, passed, failed, skipped
	}
	total++
	switch c.Status {
	case statusPass:
		passed++
	case statusFail:
		failed++
	default:
		skipped++
	}
	return total, passed, failed, skipped
}
