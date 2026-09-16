# Configuration

Key-by-key documentation of every setting — type, default, range and effect — is in
[`14`](14-settings-reference.md). This page covers where the file lives, how to register the
bridge in a host agent, worked config files, placeholder binding and the operator commands.

## 1. Where config lives

| Role | Linux / macOS | Windows |
|---|---|---|
| Config (the only file read) | `$XDG_CONFIG_HOME/agents-bridge/config.yaml`, default `~/.config/agents-bridge/config.yaml` | `%APPDATA%\agents-bridge\config.yaml` |
| Transcripts + audit log | `$XDG_STATE_HOME/agents-bridge/` (`0700`) | `%LOCALAPPDATA%\agents-bridge\state\` (owner-only DACL) |
| Control endpoints | `$XDG_RUNTIME_DIR/agents-bridge/` (`0700`), or `<TMPDIR>/agents-bridge-<uid>/` (`0700`) when `XDG_RUNTIME_DIR` is unset — `/tmp` in place of `TMPDIR` on macOS, where `TMPDIR` is a long per-process path that can overflow a unix socket's 104-byte limit | `\\.\pipe\agents-bridge-<sid>-<pid>` + index under `%LOCALAPPDATA%` |
| Host registration | MCP server block, supplies `--host <agent_id>` | same |

The config file is refused if it is group- or world-writable (POSIX), or if its DACL grants
write to anyone but the owner, `SYSTEM` and `Administrators` (Windows). `bridge doctor`
prints the effective paths for the platform it is running on.

A `.agents-bridge.yml` inside a repository is **never** read. This is not configurable.

## 2. Host registration

Paths below are POSIX; on Windows use the `.exe` and a Windows path
(`C:\\Users\\you\\bin\\bridge.exe`). Nothing else in the registration changes.

Claude Code (`~/.claude.json` or `claude mcp add`):

```json
{
  "mcpServers": {
    "agents-bridge": {
      "type": "stdio",
      "command": "/home/you/.local/bin/bridge",
      "args": ["serve", "--host", "claude"],
      "env": {}
    }
  }
}
```

Codex (`~/.codex/config.toml`):

```toml
[mcp_servers.agents-bridge]
command = "/home/you/.local/bin/bridge"
args = ["serve", "--host", "codex"]
```

Same binary, same config file, one differing argument. The bridge refuses to start if
`--host codex` is passed but the process ancestry and environment say Claude Code.

## 3. Config file

```yaml
version: 1

defaults:
  timeout_s: 900
  max_output_bytes: 262144
  max_prompt_bytes: 30720     # argv-bound prompts; Windows caps the command line at 32K
  max_transcript_bytes: 52428800
  max_depth: 1                # 0 = delegation disabled; valid range 0-4
  max_continuations: 3        # links a needs_input continuation chain may have; 0 = a
                               # needs_input run may never be continued; valid range 0-50
  max_concurrent_runs: 4
  run_retention_s: 3600       # a finished run stays resolvable this long (FR-2.6)
  max_agent_steers: 3         # operator steers are uncapped
  max_turns: 40
  question_timeout_s: 300     # bounds both the TUI wait and the elicitation wait for an
                               # answer to B's question; no answer within this -> run fails
                               # closed into needs_input, which itself rests for the same
                               # duration before becoming failed
  allow_write_mode: false     # ceiling for every adapter's `mode: write`
  allow_unconfined_write: false  # ceiling for `worktree: off` — direct edits, no diff
  agent_acceptance: false     # may agent A apply a write run's diff, or only the operator

features:                     # global on/off; per-adapter may narrow, never widen
  stream: true
  watch: true
  interactive: true           # default false; set explicitly here to demonstrate the claude
                              # adapter below, which is the only v1 adapter that can ask
                              # anything. Takes effect only where an adapter also declares
                              # capabilities.interactive AND carries an interactive flag list
                              # for its mode (rule 18) — either half missing means no gate, no
                              # mcp-config, no gate flags in the child's argv. Previously
                              # defaulted to true while consumed by no production code; it is
                              # now the single switch that turns interactive mode on, and it
                              # is off until an operator sets it.
  operator_steering: true
  agent_steering: false       # agent-originated steering is opt-in, not default
  sessions: true
  audit: true
  progress_notifications: true

allowed_roots:
  - ~/projects
  - /var/www

# A per-platform baseline (HOME/PATH/TERM/LANG on POSIX; SystemRoot, USERPROFILE,
# APPDATA, LOCALAPPDATA, PATHEXT, TEMP, COMSPEC on Windows) is built in and always
# passed. This list EXTENDS the baseline. Known credential variable names
# (ANTHROPIC_API_KEY, OPENAI_API_KEY, GH_TOKEN, ...) are refused here - see SR-1.
env_allowlist: []

audit:
  path: ~/.local/state/agents-bridge/audit.jsonl
  bodies: false               # true stores prompt/response text; default is digests only

agents:
  codex:
    description: >
      OpenAI Codex CLI. Strong at focused implementation and repo-wide reasoning.
      Use for a second opinion on a diff, or an independent implementation attempt.
    command: codex
    tier: full                # full = a Go adapter provides stream/resume/steer
    mode: read-only           # THE sandbox selector. read-only | write.
                              # `write` also requires defaults.allow_write_mode: true.
    capabilities:
      stream: true
      resume: true
      steer: false            # VERIFIED: `codex queue` cannot reach a turn in flight
      interactive: false      # `codex exec` is non-interactive with approval policy
                              # never: escalation is denied and returned to the model,
                              # not surfaced. No approval UI until app-server is adopted.
      structured_output: true
    sandbox:
      # approval_policy="never" is the governing denial control. approvals_reviewer
      # only routes escalation requests; it does not forbid them. Both are emitted.
      # --ignore-user-config and --ignore-rules are part of the sandbox flag list so
      # they are re-emitted on every turn, resume included.
      read-only:
        - "--sandbox"
        - "read-only"
        - "-c"
        - "approval_policy=\"never\""
        - "-c"
        - "approvals_reviewer=\"user\""
        - "--ignore-user-config"
        - "--ignore-rules"
      write:
        - "--sandbox"
        - "workspace-write"
        - "-c"
        - "approvals_reviewer=\"user\""
        - "--ignore-user-config"
        - "--ignore-rules"
    # `exec resume` rejects --sandbox, so the same policy is expressed as -c settings.
    resume_sandbox:
      read-only:
        - "-c"
        - "sandbox_mode=\"read-only\""
        - "-c"
        - "approval_policy=\"never\""
        - "--ignore-user-config"
        - "--ignore-rules"
      write:
        - "-c"
        - "sandbox_mode=\"workspace-write\""
        - "-c"
        - "approvals_reviewer=\"user\""
        - "--ignore-user-config"
        - "--ignore-rules"
    invoke:
      # prompt goes on stdin (codex exec -), never argv: argv is world-readable
      # through process listings and crash reports.
      args: ["exec", "--json", "--skip-git-repo-check",
             "--cd", "{{cwd}}", "{{sandbox_flags}}", "-"]
      prompt: stdin
    resume_invoke:
      # VERIFIED 2026-09-14 against codex-cli 0.154.0:
      #  * `exec resume` accepts NEITHER -C/--cd NOR --sandbox. Passing either is a
      #    parse error. cwd comes from the child's own working directory; the sandbox
      #    must be re-asserted through -c sandbox_mode.
      #  * resume does NOT inherit the parent thread's read-only sandbox. Omit the
      #    setting and the run is WRITABLE. Re-assert on every single turn.
      args: ["exec", "resume", "{{vendor_session_id}}", "--json",
             "{{resume_sandbox_flags}}", "-"]
      prompt: stdin
    # No steer block. VERIFIED 2026-09-14: `codex queue` succeeds against a
    # CLI-owned thread and returns exit 0, but the message is store-and-forward — it
    # is delivered on the thread's NEXT turn, never mid-turn, and a one-shot
    # `codex exec` exits without ever consuming it. Steering a running Codex run is
    # not possible on the exec surface. See docs/12 S3.
    stream:
      format: jsonl
      source: stdout
      vendor_session_id_path: "thread_id"       # top level of the `thread.started` event
      final_message:                             # NOT a distinct event type
        event: "item.completed"
        where: "item.type == \"agent_message\""
        text: "item.text"
      usage: "turn.completed.usage"

  claude:
    description: >
      Anthropic Claude Code. Strong at multi-file reasoning, review and planning.
    command: claude
    tier: full
    mode: read-only
    capabilities:
      stream: true
      resume: true
      interactive: true       # the only v1 adapter that can ask the operator anything;
                              # requires mode: read-only — mode: write is a load error
                              # (rule 20): v1 denies every approval wholesale, so a
                              # write-mode interactive run would have every tool call
                              # refused by the gate despite looking correctly configured
      steer: queued           # true | queued | false. queued = delivered at the next
                              # turn boundary, not a mid-turn interruption.
      structured_output: true
    sandbox:
      # --tools is what actually bounds the surface: --allowed-tools alone does NOT
      # remove Bash, and Bash is auto-approved under --print.
      # --setting-sources "" is mandatory: without it a hostile repo's
      # .claude/settings.json SessionStart/PreToolUse hook runs arbitrary shell
      # regardless of --tools.
      # VERIFIED 2026-09-14 (correcting an earlier assumption): --permission-mode plan
      # DOES return a real answer and does read files; it does not force a plan-shaped
      # reply. `manual` is used anyway because --tools is the control that actually
      # bounds the surface, and `manual` expresses "no implicit approvals" directly.
      # --setting-sources "" is the ONLY flag that stops a hostile repo's
      # .claude/settings.json SessionStart hook. --strict-mcp-config does NOT: VERIFIED,
      # the hook still fired. It is kept because it scopes MCP servers, which is a
      # different control. Note --setting-sources "" is all-or-nothing: it also drops
      # the operator's own user-level settings.
      read-only:
        - "--permission-mode"
        - "manual"
        - "--permission-prompts"
        - "none"
        - "--tools"
        - "Read,Glob,Grep"
        - "--setting-sources"
        - ""
      write:
        - "--permission-mode"
        - "acceptEdits"
        - "--tools"
        - "Read,Glob,Grep,Edit,Write"
        - "--setting-sources"
        - ""
    # Only reached when features.interactive is on. A question never
    # appears on stdout; it arrives as an MCP tools/call on the server named
    # here. --strict-mcp-config is what keeps this the ONLY server the child
    # loads (docs/12 C7). It repeats --permission-prompts with a different
    # value than the sandbox list above (none vs host) on purpose: last flag
    # wins (VERIFIED against claude 2.1.272, docs/12 C8), and invoke.args
    # below places {{sandbox_flags}} BEFORE {{gate_flags}}, so the fail-closed
    # "none" stands until this feature is switched on and the gate flags land
    # last.
    interactive:
      read-only:
        # Rule 21: the roster flag is repeated here with AskUserQuestion added,
        # and lands after {{sandbox_flags}}, so the read-only run keeps the
        # three-tool surface and only an interactive run gains the question
        # tool. Without it the gate transport works and never receives
        # anything: the child reports the tool unavailable and asks in prose.
        - "--tools"
        - "Read,Glob,Grep,AskUserQuestion"
        - "--strict-mcp-config"
        - "--mcp-config"
        - "{{gate_config}}"
        - "--permission-prompt-tool"
        - "mcp__bridge_gate__ask"
        - "--permission-prompts"
        - "host"
    # {{sandbox_flags}} MUST precede {{gate_flags}}: last flag wins (docs/12
    # C8), so this order is what keeps --permission-prompts none the default.
    invoke:
      # VERIFIED: --session-id NAMES a new session and exits 1 with
      # "Session ID <id> is already in use" if reused. --resume is the only way back in.
      # Also: --tools is variadic and swallows a trailing positional prompt, which is a
      # second reason the prompt goes on stdin rather than argv.
      args: ["-p", "--output-format", "stream-json", "--verbose",
             "--input-format", "stream-json", "--replay-user-messages",
             "--strict-mcp-config", "--add-dir", "{{cwd}}",
             "--session-id", "{{vendor_session_id}}", "{{sandbox_flags}}", "{{gate_flags}}"]
      prompt: stdin_stream_json
      # VERIFIED: the process does NOT exit when the turn completes - it blocks until
      # stdin reaches EOF. Turn completion is the `result` record, never process exit.
      # The run manager closes stdin once no further steer is possible.
      close_stdin_after: result
    resume_invoke:
      # {{gate_flags}} after the resume sandbox flags: capabilities.interactive
      # is true above, and a resumed run keeps every other MCP server blocked
      # by --strict-mcp-config, so without it a question raised mid-session
      # could never reach the operator (rule 19).
      args: ["-p", "--output-format", "stream-json", "--verbose",
             "--input-format", "stream-json", "--replay-user-messages",
             "--strict-mcp-config", "--add-dir", "{{cwd}}",
             "--resume", "{{vendor_session_id}}", "{{sandbox_flags}}", "{{gate_flags}}"]
      prompt: stdin_stream_json
      close_stdin_after: result
    steer:
      mode: stdin
      # No string template. The record is built by a JSON encoder from a typed
      # struct; {{message}} is a value, never interpolated text.
      record: user_message
    stream:
      format: jsonl
      source: stdout
      vendor_session_id_path: "session_id"       # top level of EVERY record

  antigravity:                # example of a degraded adapter
    description: Google Antigravity CLI (`agy`). Successor to Gemini CLI.
    command: agy
    tier: basic               # YAML only: one-shot, buffered output, no Go adapter
    mode: read-only
    sandbox_enforced: false   # REQUIRED when a sandbox mode has no vendor flags.
                              # Surfaced as UNSANDBOXED in the tool description,
                              # in `bridge status`, and in every audit record.
    capabilities:            # tier: basic forces every one of these to false
      stream: false
      resume: false
      steer: false
      interactive: false
      structured_output: false
    sandbox:
      read-only: []           # agy has no read-only flag; see sandbox_enforced
    invoke:
      args: ["--print", "--"]
      prompt: argv
```

**Tiers.** `tier: basic` is YAML-only and forces `stream`, `resume`, `steer`,
`interactive` and `structured_output` to `false`; declaring any of them `true` is a load
error. `tier: full` requires a Go adapter registered in-tree under the same `agent_id`;
a `tier: full` entry with no implementation is a load error. This is the honest split:
config can describe *how to invoke* a CLI, it cannot describe how to parse a vendor's
event stream or detect that it is asking a question.

**Write mode.** `mode: write` requires `defaults.allow_write_mode: true`. By default it
runs in a bridge-created worktree and returns a diff that must be accepted before anything
reaches the caller's tree (FR-10). `mode: write` and `capabilities.interactive: true` are
mutually exclusive — declaring both is a load error (rule 20), because v1 denies every
approval wholesale, and a write-mode run that could never get an approval through would
have every tool call refused by the gate while its config looks fully wired for
interactive mode.

**The question tool.** `capabilities.interactive: true` also requires the literal tool
name `AskUserQuestion` to appear in some flag the adapter passes for its mode (rule 21).
Declaring the gate is not enough: for Claude the flag that actually bounds the child's
tool surface is `--tools` (spike C5), and a `--tools` list without the question tool
leaves the child unable to raise a question at all — observed live, the model reported
the tool unavailable and asked in plain prose, which no parser sees, so the run hung
until its timeout while the gate sat idle. The loader matches the name anywhere in the
adapter's flags, because which flag carries the roster is vendor knowledge it does not
have; the check catches the omission, it is not a security boundary.

```yaml
# A write-enabled adapter, confined (the default and the recommended shape).
# Complete and schema-valid as shown.
version: 1
allowed_roots: [~/projects]

defaults:
  allow_write_mode: true

agents:
  codex_write:
    description: Codex with write access, confined to a disposable worktree.
    command: codex
    tier: full
    mode: write
    worktree: required        # default; may be omitted
    sandbox:
      write: ["--sandbox", "workspace-write", "-c", "approvals_reviewer=\"user\"",
              "--ignore-user-config", "--ignore-rules"]
    invoke:
      args: ["exec", "--json", "--skip-git-repo-check",
             "--cd", "{{cwd}}", "{{sandbox_flags}}", "-"]
      prompt: stdin
```

**Steering needs a session with a next turn.** `close_stdin_after: result` makes a run
one-shot: the child's input closes the moment its turn completes, which is what lets the
process exit at all. Since every shipped adapter delivers guidance *at the next turn
boundary* rather than mid-turn, the steering window for such a run is only "until the
first turn finishes" — for a short task, that window may close before anyone can type.
A genuinely steerable session needs `close_stdin_after: never`, and then the run ends by
completing its budget, being cancelled, or timing out. This is a property of the vendors,
not of the bridge: see [`12`](12-spike-results.md) C1 and C2.

**Unconfined write.** `worktree: off` lets B edit the caller's tree in place: no worktree,
no diff, no acceptance step, no rollback. It requires **both**
`defaults.allow_write_mode: true` and `defaults.allow_unconfined_write: true`, and the
bridge makes it loud rather than refusing it — the tool description carries the word
**UNCONFINED**, `bridge status` and the watch TUI header show `worktree: off`, and every
audit line for the run records `confinement: none`. Working-directory validation against
`allowed_roots` is **not** waived by this flag, or by any flag.

An adapter is dropped at load, with a diagnostic, if: `mode: write` without the
corresponding `defaults` opt-in; `resume: true` without `resume_invoke`; `steer: true`
or `queued` without a `steer` block; a `sandbox.<mode>` list that is empty without
`sandbox_enforced: false`; or `command` resolving to the host agent's binary.

## 4. Placeholder rules

| Placeholder | Bound to |
|---|---|
| `{{prompt}}` | The caller's prompt. Delivered per `invoke.prompt` (`stdin`, `stdin_stream_json`, or `argv`); `argv` is capped at `max_prompt_bytes` |
| `{{cwd}}` | Validated absolute path inside `allowed_roots`; also set as the child's spawn working directory |
| `{{vendor_session_id}}` | The **vendor's own** session id, extracted from B's event stream on the first run. Never supplied by the caller |
| `{{message}}` | Steer text, one argv element or one encoded JSON value |
| `{{model}}` | Optional model name, validated against the adapter's `allowed_models` |
| `{{max_turns}}` | Turn cap, where the vendor CLI accepts one |
| `{{sandbox_flags}}` | Expands to the list under `sandbox.<mode>` for the adapter's `mode` |
| `{{resume_sandbox_flags}}` | Expands to the list under `resume_sandbox.<mode>`, for the vendor's resume surface |
| `{{gate_flags}}` | Expands to the list under `interactive.<mode>`; empty, and omitted, when the run has no gate |
| `{{gate_config}}` | Path to this run's per-run mcp-config, written `0600` outside any worktree and removed when the run ends |

There is no `{{session_id}}`. A caller passes an **opaque bridge handle**, which the
bridge resolves to a vendor session id it created and recorded itself. Vendor ids are
never accepted from, nor returned to, the caller — see [`02`](02-architecture.md) §3.

Rules enforced at load and at expansion:

1. A placeholder expands to **exactly one** argv element, except `{{sandbox_flags}}`,
   which expands to a fixed list from config.
2. No placeholder is concatenated into a larger string in an argv element. The one
   exception is the `--flag={{placeholder}}` binding form, which is a single argv
   element and is **required** for any free-text value the vendor CLI would otherwise
   misparse as a flag (`codex queue --message -foo` fails without it).
3. Placeholders inside a structured record (`steer.mode: stdin`) are encoded by a typed
   JSON encoder, never by string templating. String templates are a load error.
4. `command` is resolved through `PATH` (plus `PATHEXT` on Windows) to an absolute path
   and must be a regular executable file. A `command` containing a shell metacharacter
   is a load error. On Windows, `.bat`, `.cmd` and `.ps1` are refused outright — `cmd.exe`
   re-parses their arguments and would reintroduce injection. *(This blocks npm-shim
   installs on Windows, which is why Windows is unsupported until v1.1.)*
5. `args` may not contain any `--dangerously*` or `--allow-dangerously*` flag, as an
   exact element match. Hard refusal, not a warning.
6. The prompt is preceded by `--` wherever it is argv-bound and the CLI supports it, so
   a prompt beginning with a dash can never be reparsed as flags.
7. Sandbox flags are emitted on **every** invocation including resume, never persisted
   in session state.
8. On Windows the command line is built with Go's own Windows argument quoting; an
   argument that cannot be safely quoted is a hard error.
9. An adapter whose resolved `command` equals the host agent's executable is dropped —
   but this is a secondary check. The primary exclusion is `agent_id == --host`
   ([`01`](01-requirements.md) FR-6.3), because shim installs make executable identity
   unreliable.

## 5. Precedence

Stated canonically in [`11`](11-domain-model.md) §5 and repeated here unchanged:

```
config ceiling  ≥  adapter declaration  ≥  runtime toggle  ≥  per-call parameter
```

Each step may only narrow. An adapter cannot declare what a `defaults` ceiling forbids
(`mode: write` needs `allow_write_mode`; `worktree: off` needs `allow_unconfined_write`
as well). A runtime toggle can only disable what the adapter declared. A per-call
parameter can only select among what the effective settings already permit: the single
one that touches policy is `sandbox`, which narrows `write` to `read-only` and never the
reverse.

## 6. Operator commands

```
bridge status                       # effective config, live runs, toggles
bridge runs                         # run table: id, host, agent, state, mode, age
bridge watch [run_id]               # live TUI; picker when no id given
bridge steer <run_id> "<message>"   # operator steer, uncapped
bridge answer <run_id> "<text>"     # answer B's pending question; on a run resting in
                                     # needs_input this instead creates a continuation — a
                                     # new run seeded with the question, this answer, and
                                     # the original prompt — and prints the successor's run
                                     # id (docs/02 §2.3a)
bridge stop <run_id>                # kill the run's process group
bridge enable|disable <feature> [agent]
bridge doctor                       # validate config, permissions, adapters, host detection
bridge validate --config <path>      # exercise every adapter against a stub agent: no vendor
                                     # CLI, no credentials, no network. Exit 1 on any failure
```

The operator's answer carries a question id so a reply lands on the question it was
actually composed against, never on one that replaced it between display and keypress.
Both answer paths are protected. `bridge watch`'s TUI panel binds the answer to the
question id it rendered (`internal/tui/watch.go`). `bridge answer <run_id> "<text>"`
(`answerRun` in `cmd/bridge/accept.go`) resolves the run's live question id itself over
`control.VerbRuns` before sending the answer, so it carries the same identity check
rather than an empty id meaning "answer whatever is pending." In both cases, an answer
that no longer matches the pending question is refused (`run.ErrQuestionChanged`) rather
than misdelivered, and a run with nothing pending is a hard error
(`"run %s has no question pending to answer"`) rather than a blind send. The lookup and
the send are two separate round trips, so the question can in principle still change
between them; that narrow window is the same one `run.Run.AnswerQuestion`'s id check
exists to close. A delegated agent has no route to the answer verb at all;
`Answer`/`control.VerbAnswer` are reachable only from `cmd/bridge/accept.go` and
`internal/tui/watch.go`, both operator-side.

`answerRun` branches on the run's reported state, not on whether a question id is present:
a `needs_input` run still reports its last question's id (retained for exactly this read),
so the branch is `State == "needs_input"` first. That branch calls `control.VerbContinue`
instead of `control.VerbAnswer` and is a continuation, not a reply — see docs/02 §2.3a and
docs/11 §3 invariant 3 for why no reply is possible on a run in this state. `Continue`/
`control.VerbContinue` are reachable only from the same two operator-side call sites as
`VerbAnswer`; a delegated agent has no route to either.

`bridge validate` is the other half of that pair, and the two answer different questions.
`doctor` asks whether THIS machine is set up: the vendor binary resolves on PATH, `--version`
runs, the runtime directories are owner-only. `validate` asks whether an ADAPTER DECLARATION is
internally consistent and whether the bridge drives it correctly, and it answers that without a
vendor CLI at all: the adapter's `command` is substituted with the binary's own hidden
`stub-agent` verb through the loader's `LookPath` seam, and the run then goes through the real
policy, argv-building, run and sanitize paths. It reports argv construction (including that no
caller value can change the element count), sandbox flag presence, prompt delivery by the
declared mode, enveloping, timeout kill, non-zero exit handling, parser registration for
`tier: full` — with `--stream-fixture <file>`, that the registered parser handles a recorded
stream — and the interactive rules 13/18/19/20/21. What it cannot tell you is whether the
vendor accepts those flags or emits that stream shape; only the gated conformance suite does
that (`docs/12-spike-results.md` records what a flag was observed to do, which is the same
distinction).

`bridge doctor --insecure-skip-permission-check` loads the config without asking whether
only its owner can write it. It exists because Windows has no permission check yet: the
loader refuses every config file there (`internal/config/perm_windows.go`, v1.1), so without
the flag nothing on Windows can reach the loader at all. `serve` has no such flag, and
`doctor` prints a `WARN` line naming the file whenever the flag is used.

`bridge doctor` checks: config file permissions (mode on POSIX, DACL on Windows), every
adapter's binary resolves and `--version` runs, allowed roots exist and are directories and
canonicalise cleanly, state and runtime locations are owner-only, the control endpoint can
be created, host detection agrees with `--host`, and no adapter carries a forbidden flag or
a forbidden executable type.
