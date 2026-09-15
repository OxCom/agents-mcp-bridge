package config

import (
	"path/filepath"
	"sort"
	"strings"
)

// forbiddenFlags are refused as exact argv elements. A substring rule would be
// both useless (the command is already not a shell) and harmful: it would forbid
// "--disallowed-tools Bash", the very argument you want.
var forbiddenFlagPrefixes = []string{
	"--dangerously",
	"--allow-dangerously",
	"--yolo",
	"--allow-all",
}

var knownPlaceholders = map[string]bool{
	"{{prompt}}":               true,
	"{{cwd}}":                  true,
	"{{vendor_session_id}}":    true,
	"{{message}}":              true,
	"{{model}}":                true,
	"{{max_turns}}":            true,
	"{{sandbox_flags}}":        true,
	"{{resume_sandbox_flags}}": true,
	"{{gate_flags}}":           true,
	"{{gate_config}}":          true,
}

// validateSemantics enforces the rules JSON Schema cannot express. They are
// numbered to match the $comment block in schema/config.schema.json; every one
// is a load error, never a warning.
func (c *Config) validateSemantics(opts Options) error {
	ids := make([]string, 0, len(c.Agents))
	for id := range c.Agents {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic error ordering

	if err := c.validateEnvAllowlist(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := c.validateAdapter(c.Agents[id], opts); err != nil {
			return err
		}
	}
	// Rule 14: a queueing policy needs somewhere to queue.
	if c.Defaults.OnConcurrencyLimit == "queue" && c.Defaults.QueueDepth < 1 {
		return errf("defaults", "on_concurrency_limit: queue requires queue_depth >= 1")
	}
	return nil
}

// validateEnvAllowlist implements SR-1: the bridge must not become the thing
// that hands a credential to a subprocess, so a credential-shaped variable name
// is refused outright rather than merely discouraged.
func (c *Config) validateEnvAllowlist() error {
	for _, name := range c.EnvAllowlist {
		if credentialPattern.MatchString(name) {
			return errf("env_allowlist",
				"%q looks like a credential; the bridge never forwards credentials (SR-1). "+
					"The target agent authenticates with its own existing login", name)
		}
	}
	return nil
}

func (c *Config) validateAdapter(a *Adapter, opts Options) error {
	base := "agents." + a.ID

	// Rule 1: basic tier is YAML-only and one-shot. Declaring a streaming
	// capability there would promise behaviour no code implements.
	if a.Tier == TierBasic {
		if a.Capabilities.Stream || a.Capabilities.Resume || a.Capabilities.Interactive ||
			a.Capabilities.StructuredOutput || a.Capabilities.Steer != SteerNone {
			return errf(base+".capabilities",
				"tier: basic forces every capability (stream, resume, steer, interactive, structured_output) false; "+
					"declare tier: full and register a Go adapter to use any of them")
		}
	}

	// Rule 3 and 15: resume needs its own invocation, and its own sandbox flags
	// wherever the vendor's resume surface differs. VERIFIED for codex, whose
	// `exec resume` rejects --sandbox and silently runs writable without it.
	if a.Capabilities.Resume {
		if a.ResumeInvoke == nil {
			return errf(base, "capabilities.resume: true requires a resume_invoke block")
		}
		if len(a.Sandbox) > 0 && len(a.ResumeSandbox) == 0 && usesPlaceholder(a.ResumeInvoke.Args, "{{resume_sandbox_flags}}") {
			return errf(base, "resume_invoke uses {{resume_sandbox_flags}} but no resume_sandbox block is declared")
		}
		if len(a.Sandbox) > 0 && len(a.ResumeSandbox) == 0 && !usesPlaceholder(a.ResumeInvoke.Args, "{{sandbox_flags}}") {
			return errf(base, "capabilities.resume: true with a sandbox but no sandbox flags on the resume path; a resume that drops the sandbox runs writable (see docs/12-spike-results.md S2)")
		}
	}

	// Rules 4 and 17: a steer channel must exist exactly when it is claimed.
	switch a.Capabilities.Steer {
	case SteerTrue, SteerQueued:
		if a.SteerSpec == nil {
			return errf(base, "capabilities.steer: %s requires a steer block", a.Capabilities.Steer)
		}
		switch a.SteerSpec.Mode {
		case "command":
			if len(a.SteerSpec.Args) == 0 {
				return errf(base+".steer", "mode: command requires args")
			}
		case "stdin":
			if a.SteerSpec.Record == "" {
				return errf(base+".steer", "mode: stdin requires record; string templates are refused because interpolating into JSON is an injection primitive")
			}
		default:
			return errf(base+".steer", "unknown mode %q", a.SteerSpec.Mode)
		}
	case SteerNone:
		if a.SteerSpec != nil {
			return errf(base, "steer block declared with capabilities.steer: false; an unusable channel must not sit in config looking functional")
		}
	default:
		return errf(base+".capabilities", "unknown steer value %q", a.Capabilities.Steer)
	}

	// Rule 13: you cannot detect a question in a stream you cannot parse.
	if a.Capabilities.Interactive && !a.Capabilities.Stream {
		return errf(base+".capabilities", "interactive: true requires stream: true")
	}

	// Rule 18: capabilities.interactive: true must be backed by a non-empty
	// interactive flag list for the adapter's mode, or the bridge would claim
	// to route questions it has no way to receive.
	if a.Capabilities.Interactive {
		if len(a.Interactive[string(a.Mode)]) == 0 {
			return errf(base, "capabilities.interactive is true but no interactive flag list "+
				"is declared for mode %q: the bridge would claim to route questions it "+
				"cannot receive (rule 18)", a.Mode)
		}
		// Rule 19: a resumed run still needs its gate flags, or a question
		// raised mid-session has nowhere to go. This checks our own
		// placeholder, not a vendor flag, so it belongs in Go.
		if a.ResumeInvoke != nil && !usesPlaceholder(a.ResumeInvoke.Args, "{{gate_flags}}") {
			return errf(base, "capabilities.interactive is true and resume_invoke is declared, "+
				"but resume_invoke.args does not reference {{gate_flags}}: a resumed run would "+
				"lose its gate routing (rule 19)")
		}
		// Rule 20: v1 denies every approval wholesale (docs/12 spike C7) and
		// routes only AskUserQuestion to a human; a write-mode run needs
		// approval for every tool call that writes, so a write adapter with
		// interactive: true would have every one of those calls denied by
		// the gate while looking, from the config, fully wired up.
		if a.Mode == ModeWrite {
			return errf(base, "capabilities.interactive is true and mode is write: v1 denies "+
				"every approval, so a write-mode run would have every tool call refused by the "+
				"gate despite appearing configured for interactive mode (rule 20)")
		}
	}

	// Rule 5: an empty sandbox list for the adapter's mode means the vendor
	// enforces nothing, which must be declared, not discovered at runtime.
	if len(a.Sandbox[string(a.Mode)]) == 0 && a.SandboxEnforcedOrDefault() {
		return errf(base, "sandbox.%s is empty or absent; declare sandbox_enforced: false to run UNSANDBOXED, or supply the vendor's flags", a.Mode)
	}

	// Rules 6 and 7: the ceilings for write and for unconfined write.
	if a.Mode == ModeWrite && !c.Defaults.AllowWriteMode {
		return errf(base, "mode: write requires defaults.allow_write_mode: true")
	}
	if a.Worktree == WorktreeOff {
		if !c.Defaults.AllowWriteMode || !c.Defaults.AllowUnconfinedWrite {
			return errf(base, "worktree: off requires BOTH defaults.allow_write_mode and defaults.allow_unconfined_write; one ceiling is not enough to turn a review tool into an unsupervised editor")
		}
	}

	// Rule 16: a stream-json child blocks on stdin EOF rather than exiting.
	for _, inv := range []struct {
		path string
		v    *Invocation
	}{{base + ".invoke", a.Invoke}, {base + ".resume_invoke", a.ResumeInvoke}} {
		if inv.v == nil {
			continue
		}
		if inv.v.Prompt == "stdin_stream_json" && inv.v.CloseStdinAfter == "" {
			return errf(inv.path, "prompt: stdin_stream_json requires close_stdin_after; such a child blocks on stdin EOF instead of exiting when its turn completes (docs/12-spike-results.md C1)")
		}
		if err := validateArgs(inv.path, inv.v.Args); err != nil {
			return err
		}
	}
	if a.SteerSpec != nil {
		if err := validateArgs(base+".steer", a.SteerSpec.Args); err != nil {
			return err
		}
	}
	for mode, args := range a.Sandbox {
		if err := validateArgs(base+".sandbox."+mode, args); err != nil {
			return err
		}
	}
	for mode, args := range a.ResumeSandbox {
		if err := validateArgs(base+".resume_sandbox."+mode, args); err != nil {
			return err
		}
	}
	for mode, args := range a.Interactive {
		if err := validateArgs(base+".interactive."+mode, args); err != nil {
			return err
		}
	}

	// Rule 9: resolve the executable last, so a structurally broken config never
	// touches the filesystem.
	resolved, err := opts.LookPath(a.Command)
	if err != nil {
		return errf(base, "command %q does not resolve on PATH: %v", a.Command, err)
	}
	if ext := strings.ToLower(filepath.Ext(resolved)); ext == ".bat" || ext == ".cmd" || ext == ".ps1" {
		return errf(base, "command resolves to %s, a script launcher; cmd.exe re-parses its arguments and would reintroduce injection", resolved)
	}
	a.ResolvedCommand = resolved
	return nil
}

// validateArgs enforces rules 8 and 11: no bypass flag may appear as an argv
// element, and every placeholder must be one the expander knows.
func validateArgs(path string, args []string) error {
	for i, arg := range args {
		for _, bad := range forbiddenFlagPrefixes {
			if arg == bad || strings.HasPrefix(arg, bad+"=") || strings.HasPrefix(arg, bad+"-") {
				return errf(path, "args[%d] = %q is a bypass flag; the bridge never emits one under any configuration", i, arg)
			}
		}
		for _, ph := range extractPlaceholders(arg) {
			if !knownPlaceholders[ph] {
				return errf(path, "args[%d] uses unknown placeholder %s", i, ph)
			}
		}
	}
	return nil
}

func extractPlaceholders(arg string) []string {
	var out []string
	for {
		start := strings.Index(arg, "{{")
		if start < 0 {
			return out
		}
		end := strings.Index(arg[start:], "}}")
		if end < 0 {
			return out
		}
		out = append(out, arg[start:start+end+2])
		arg = arg[start+end+2:]
	}
}

func usesPlaceholder(args []string, ph string) bool {
	for _, a := range args {
		if strings.Contains(a, ph) {
			return true
		}
	}
	return false
}
