# Requirements

**Project:** `agents-mcp-bridge` — a single MCP server that lets any CLI coding agent
delegate work to any *other* CLI coding agent, safely, observably, and steerably.

Status: implemented through Phase 5, plus Phase 3 interactive mode. Windows is unsupported
(Phase 5a, targets v1.1). The gated conformance suite (`BRIDGE_CONFORMANCE=1 go test
./conformance/`) is not run by CI at all; it runs by hand, against real vendor CLIs, with
credentials the runner supplies. Interactive mode is Claude Code only, and its properties are
*enforced* as of 2026-09-16 on the evidence of that dated local run against `claude` 2.1.272,
not on a check that repeats. See `SECURITY.md` for the enforced/declared distinction.

---

## 1. Problem statement

Agent A (Claude Code, Codex, Gemini/Antigravity, Cursor, …) is running in a terminal. The
user wants A to hand a sub-task to agent B — a second model with a different strength, a
different context window, or simply a second opinion. Today this is done by ad-hoc
wrappers: a shell command, a hand-rolled MCP server per vendor pair, or copy-paste.

Those wrappers have three recurring defects:

1. **They are trust-blind.** The delegated agent inherits the caller's full filesystem and
   shell authority, and its output is fed back into the caller's context as if it were
   trusted text.
2. **They are opaque.** The caller blocks on a subprocess for minutes with no visibility
   into what B is doing, and no way to intervene.
3. **They are hardcoded.** Adding a new agent means writing code, not configuration.

`agents-mcp-bridge` solves all three with one binary, driven by one config file.

---

## 2. Actors and terms

| Term | Meaning |
|---|---|
| **Host agent (A)** | The CLI agent whose MCP config contains this bridge. It calls tools. |
| **Target agent (B)** | The CLI agent the bridge spawns. Any agent declared in config. |
| **Operator** | The human at the terminal. Owns the config and the runtime toggles. |
| **Adapter** | A config entry describing how to invoke, stream, resume and steer one agent. |
| **Run** | One delegated execution of B, identified by a `run_id`. |
| **Session** | B-side conversation state that survives across runs, identified by B's own session id. |

A and B are **generic**. Nothing in the design is specific to Claude or Codex; those are
simply the two adapters that ship verified.

---

## 3. Functional requirements

### FR-1 — Config-declared agents, in two tiers

Vendor event streams, question detection and session extraction cannot be described by a
config file without inventing a DSL for parsing hostile input. The spec therefore states
plainly what config alone buys:

| Tier | Declared in | Capabilities |
|---|---|---|
| **Basic** | YAML only, ~15 lines, no rebuild | invoke, sandbox flag lists, working-directory confinement, timeout, buffered final output, audit. `stream`, `resume`, `steer`, `interactive` are all `false`. |
| **Full** | YAML + a Go adapter in-tree | Everything above plus event-stream normalisation, session extraction, resume, steering and interactive mode. Requires a release. |

- **FR-1.1** A **basic** agent is added by editing the user-level config. No code change,
  no rebuild. It is immediately usable for one-shot delegation.
- **FR-1.1b** A **full** agent additionally requires a Go adapter implementing the
  `VendorStream` interface. The config declares `tier: full` and names it; a `tier: full`
  adapter with no registered implementation is a load error.
- **FR-1.2** Each declared agent becomes exactly one MCP tool, named `ask_<agent_id>`.
- **FR-1.3** An adapter declares its capabilities explicitly: `stream`, `resume`, `steer`,
  `structured_output`, `sandbox_modes`. Capabilities the target CLI lacks are declared
  `false`, and the bridge degrades predictably rather than pretending.
- **FR-1.4** The argv template uses named placeholders (`{{prompt}}`, `{{cwd}}`,
  `{{vendor_session_id}}`, `{{model}}`). Each placeholder binds to exactly one argv
  element, except the `--flag={{placeholder}}` binding form. There is no `{{session_id}}`:
  vendor session ids never cross the MCP boundary ([`11`](11-domain-model.md) §2).

### FR-2 — Delegation

- **FR-2.1** `ask_<agent>` starts a run and returns immediately with a `run_id`, an
  opaque `session_handle`, and the `bridge watch` command. It does **not** return the
  transcript path: A's own file tools would then read the raw, un-enveloped vendor
  stream and bypass the sanitizer entirely.
- **FR-2.2** `await_agent(run_id, timeout_s)` blocks for a bounded slice and is
  re-callable. It returns either the final result or a "still running" status with a
  progress digest.
- **FR-2.3** `cancel_agent(run_id)` terminates the run's whole process group.
- **FR-2.4** `list_runs()` returns the operator-visible runs owned by this server instance.
- **FR-2.5** A run may continue a previous B session when the adapter declares
  `resume: true`. The caller passes the opaque `session_handle` from FR-2.1, never a
  vendor session id. The bridge resolves the handle to a vendor id it created and
  recorded itself, bound to host, adapter, canonical cwd, sandbox mode and server
  instance. A handle it did not issue is refused.
- **FR-2.6** A finished run stays resolvable in the registry for `run_retention_s`
  after completion, so a late `await_agent` returns the result rather than an error.

### FR-3 — Live visibility

- **FR-3.1** The bridge captures B's native event stream and persists it as JSONL, one
  file per run, as the single source of truth.
- **FR-3.2** `bridge watch [run_id]` is a TUI, run by the operator in a second terminal or
  tmux pane, that renders the live stream: tool calls, file edits, shell commands, tokens,
  and B's messages.
- **FR-3.3** With no `run_id`, `bridge watch` shows a run picker across all live runs for
  this user.
- **FR-3.4** For adapters with `stream: false`, the transcript contains start, end, exit
  code and buffered output only — the TUI degrades, it does not fail.
- **FR-3.5** MCP `notifications/progress` are emitted best-effort for hosts that request a
  `progressToken`. They are an extra, never the primary channel. *(Claude Code does not
  send a progressToken today; the design must not depend on it.)*

### FR-4 — Interactive mode

- **FR-4.1** A question never appears on B's stdout (docs/12 C7). It arrives as an MCP
  `tools/call` on a per-run gate server the bridge supplies to the child, and the bridge
  surfaces it in the watch TUI, where the operator selects an option or types an answer.
- **FR-4.2** The answer returns as the gate's reply to that same call:
  `behavior: "deny"` carrying the answer text as `message`. `allow` does not work —
  it discards the answer and the child receives "The user did not answer the questions."
  (docs/12 C7). This gate reply is a distinct mechanism from the adapter's steer channel
  (FR-5), which injects unsolicited guidance into a live session rather than answering a
  blocked call.
- **FR-4.3** If no TUI is attached and the host supports MCP elicitation, the bridge
  raises `elicitation/create` so the operator is prompted inside host A's own UI.
  *(Claude Code CLI supports elicitation from v2.1.76.)* This fallback is unreachable
  above MCP protocol version 2026-07-28: the SDK refuses server-initiated elicitation at
  that version and later (docs/10 §4-5).
- **FR-4.4** If neither channel is available, the run **fails closed**: the child process
  is killed, the tool result reports `needs_input` with B's question wrapped in the
  untrusted-data envelope, and the envelope carries `session_handle` (the run id) so the
  caller can name the stopped run. **No VENDOR resume path exists** — the gate and the
  child are both gone, so an answer can never reach the original blocked tool call.
  `needs_input` clears the pending question as part of the same transition that kills the
  child; the run rests in `needs_input` only until `question_timeout_s` elapses, then
  becomes `failed` (docs/11 §3 invariant 3). What an operator CAN do instead is
  **continue** the run: a new run seeded with reconstructed context (original prompt,
  prior question/answer pairs), tracked as `resumed_from`/`resumed_by` and bounded by
  `defaults.max_continuations` — see [`02`](02-architecture.md) §2.3a. As of this change the
  `internal/run`, `internal/worktree` and `internal/config` primitives exist
  (`Run.Supersede`, `Run.TakeContinuation`, `worktree.ContinueFrom`,
  `defaults.max_continuations`); the operator-facing surface (`bridge answer`, the TUI
  question panel, `get_changes`/`await_agent` on a retired run) is a separate, later
  change.
- **FR-4.5** Interactive mode is a per-adapter capability, not a global feature, gated
  twice over. An adapter may only declare `capabilities.interactive: true` if its vendor
  surface actually blocks and exposes a correlated question, and only when it also
  declares a non-empty `interactive:` flag list for the mode in use (config schema rule
  18) — the flags that route the child to the bridge's gate server. Even then, the
  feature is off until the operator opts in: `features.interactive` defaults to false.
  **In v1 the vendor surface is Claude Code only.** `codex exec` is non-interactive with
  approval policy `never`: escalation is denied and returned to the model rather than
  surfaced, so the Codex adapter declares `interactive: false` and the approval UI for
  Codex is out of scope until the app-server surface is adopted.
- **FR-4.6** Three distinct things are **not** conflated, and each is its own capability
  with its own state transitions: a **question** (B asks the human something), a
  **permission approval** (B blocks on a correlated request that needs a decision bound
  to a thread and turn), and **steering** (unsolicited guidance injected into a running
  conversation). Question and approval share one transport — the same gate `tools/call` —
  and are told apart only by `tool_name`: a human question is `AskUserQuestion`, anything
  else is a permission approval. An approval is denied with a constant message and
  recorded in the audit log; it is never surfaced to the operator. An adapter declaring
  `steer: true` makes no claim about the other two.

### FR-5 — Steering

- **FR-5.1** The operator can inject free-form guidance into a running B from the watch
  TUI at any time.
- **FR-5.2** Host agent A can inject guidance via `steer_agent(run_id, message)`.
- **FR-5.3** Injection uses the adapter's native channel — no screen scraping:
  - Codex: `codex queue --thread <id> --message <text>` against a persisted session.
  - Claude: a `user` message written to the stdin of a
    `claude -p --input-format stream-json --output-format stream-json` process.
- **FR-5.4** Adapters without a native channel declare `steer: false`; `steer_agent`
  returns a typed "unsupported" error rather than silently dropping the message.
- **FR-5.5** Agent-originated steers are counted and capped per run. Operator steers are
  uncapped.

### FR-6 — Self-call exclusion

- **FR-6.1** The bridge knows its host: the MCP server block passes `--host <agent_id>`.
- **FR-6.2** At startup the bridge cross-checks the declaration against the parent
  process chain and host environment variables. Positive contradictory evidence (the
  ancestry says Claude Code, `--host codex` was passed) is **fatal**. Absence of evidence
  is **not**: the bridge starts with a warning, so MCP Inspector, `bridge doctor`, tests
  and unusual launchers keep working.
- **FR-6.3** The adapter whose `agent_id` equals `--host` is **not exposed as a tool at
  all** — absent from `tools/list`, not merely rejected at call time. Executable-identity
  comparison is a secondary check that can only ever *add* an exclusion, never remove
  one: npm-shim installs mean the resolved `command` (a `.js` or `.cmd` launcher) often
  does not match the host's process image (`node`, or a native binary).
- **FR-6.4** The rule is generic: host ≠ target, for any pair. There is no override.

### FR-7 — Configurability and runtime control

- **FR-7.1** Every feature — streaming, watch, interactive, operator steering, agent
  steering, sessions, write mode, audit — is a boolean with a global default and a
  per-adapter override.
- **FR-7.2** The operator can flip toggles on a live server:
  `bridge enable|disable <feature> [agent]`, or from the TUI.
- **FR-7.3** Runtime changes narrow or widen only within the bounds the config file
  permits. Config is the ceiling; runtime toggles cannot exceed it.
- **FR-7.4** No MCP tool can change a toggle, a limit, a sandbox mode or a root. The model
  cannot widen its own authority.
- **FR-7.5** `bridge status` prints effective settings, live runs and active toggles.

### FR-8 — Audit

- **FR-8.1** Every run appends structured JSONL: timestamp, run id, host agent, target
  agent, cwd, sandbox mode (and `sandbox_enforced`), depth, the **resolved argv with any
  prompt or message element replaced by its digest**, steer count and the origin of each
  steer, duration, exit code, byte counts, and a keyed HMAC-SHA-256 of prompt and of
  response. Digests are keyed per install: an unsalted hash of a short predictable prompt
  is a dictionary oracle.
- **FR-8.2** Bodies are **not** written by default. `--audit-bodies` opts in for debugging.
- **FR-8.3** Every refusal (root violation, depth limit, self-call, budget breach) is
  logged with its reason.

### FR-9 — Cross-platform support

- **FR-9.1** The bridge **compiles** for Linux, macOS and Windows on `amd64` and `arm64`
  from Phase 0 onward, and CI builds all six targets on every commit so the platform
  abstraction cannot rot.
- **FR-9.1b** **v1.0 supports Linux and macOS.** Windows is a v1.1 target. The reason is
  concrete rather than effort: npm-installed `claude` and `codex` are `.cmd` shims on
  Windows, which the adapter rules refuse, so the two reference adapters would not work
  via the install path most Windows users have — and resolving that (shim resolution, a
  Windows environment baseline, command-line length limits, race-free job assignment)
  changes the config schema. Promising parity before those are settled would ship a
  schema that has to break.
- **FR-9.2** No feature is designed Linux-only. Where an OS primitive differs, the
  behaviour is to be equivalent, not absent — the Windows implementations are specified
  below and deferred, not omitted.
- **FR-9.3** Platform-specific mechanisms, all behind one internal interface each:

  | Concern | Linux / macOS | Windows |
  |---|---|---|
  | Control channel | Unix domain socket, mode `0600` | Named pipe with an explicit DACL granting only the current user SID |
  | Peer identity check | `SO_PEERCRED` / `LOCAL_PEERCRED`, UID must match | `GetNamedPipeClientProcessId` + token SID must match the server's SID |
  | Child process grouping | `setpgid`, kill the process group | Job object with `KILL_ON_JOB_CLOSE`, terminate the job |
  | Config/state/runtime paths | XDG (`~/.config`, `~/.local/state`, `$XDG_RUNTIME_DIR`) | `%APPDATA%`, `%LOCALAPPDATA%`, `%TEMP%` under the user profile |
  | File permission check | reject group/world-writable | reject a config whose DACL grants write to anyone but the owner, `SYSTEM` and `Administrators` |
  | Path containment | symlink resolution via `EvalSymlinks` | resolve symlinks, junctions, **and** 8.3 short names; compare case-insensitively; reject UNC and device paths (`\\?\`, `\\.\`, `CON`, `NUL`, …) |
  | Executable resolution | `PATH`, must be a regular executable | `PATH` + `PATHEXT`; reject `.bat`/`.cmd`/`.ps1` as an adapter `command` |

- **FR-9.4** The watch TUI runs on Windows Terminal, PowerShell and conhost. Where a
  terminal lacks a capability (true colour, mouse), the TUI degrades and still works.
- **FR-9.5** `bridge doctor` reports the platform-specific paths and permission state in
  the platform's own terms.

### FR-10 — Write mode: confinement, diff acceptance, and opting out

- **FR-10.1** Under `worktree: required` — the default — a `mode: write` run executes in
  a git worktree the bridge creates under its own state directory, from the caller's
  current `HEAD`. The caller's working tree is not the child's working directory.
- **FR-10.2** Under `worktree: required`, a write run returns a **diff**, not a claim of
  success: changed files,
  unified patch, and the digest of each file before and after. The patch is sanitized
  and enveloped like any other output from B.
- **FR-10.3** Under `worktree: required`, nothing reaches the caller's tree without an
  explicit acceptance step:
  `bridge accept <run_id>` (operator) or `accept_changes(run_id)` (agent A, only when
  `agent_acceptance` is enabled, which is off by default). Acceptance applies the patch
  with `git apply --3way` and refuses on conflict.
- **FR-10.4** Under `worktree: required`, the worktree is removed on acceptance,
  rejection, or run expiry. A worktree
  surviving its run is a reported fault, not a silent leak.
- **FR-10.5** Under `worktree: required`, a directory that is not a git repository cannot
  run in write mode: there is nothing to branch from and no way to produce a diff, and
  there is no silent fallback to a direct write. Under `worktree: off` a non-git directory
  **is** permitted — no worktree is created and no diff is produced, so git is not
  involved at all. The operator loses `git` as the undo mechanism along with everything
  else that flag gives up.
- **FR-10.6** An adapter may declare `worktree: off`, permitted only when
  `defaults.allow_write_mode` and `defaults.allow_unconfined_write` are both true. B then
  runs directly in the validated `cwd` and edits it in place. There is no diff, no
  acceptance step and no rollback. The bridge:
  - renders the word **UNCONFINED** in that tool's description, so the calling model sees
    it before choosing the tool;
  - shows `worktree: off` in `bridge status` and in the watch TUI header;
  - records `confinement: none` on every audit line for the run;
  - refuses the combination if the `cwd` is not inside `allowed_roots` — path validation
    is not waivable by this or any other flag.
- **FR-10.7** Under `worktree: off` there is no diff and no acceptance step, so
  `get_changes` returns `no_diff_unconfined` — naming the run, its cwd and the files B
  touched, taken from the event stream rather than from git — and `accept_changes` returns
  `not_applicable_unconfined`. Neither is silently successful: a model that assumes the
  confined workflow gets a typed error, not a false confirmation.
- **FR-10.8** `worktree: off` does not relax anything else. Sandbox flags, the bypass-flag
  refusal, output sanitization, caps and audit all still apply.

### FR-11 — Transcript and audit trust

Transcripts and the audit log are different artefacts with different guarantees, and the
spec keeps them apart rather than claiming one protects the other.

- **FR-11.1** The **transcript** is the run's normalised event stream: full fidelity,
  written to the state directory, and the input to the watch TUI and to replay. It
  contains whatever B read or produced, so it is sensitive by construction.
- **FR-11.2** The **audit log** is the security record: metadata, resolved argv with
  prompt and message elements replaced by digests, and keyed HMAC digests of prompt and
  response. It contains no bodies unless `audit.bodies` is enabled.
- **FR-11.3** Neither is tamper-evident against the user who owns them. Agent B runs as
  the same user; under `worktree: off`, or if a vendor sandbox is bypassed, B can modify
  both. The spec says this rather than claiming an "append-only" guarantee it cannot
  enforce.
- **FR-11.4** Both rotate by size (`audit.rotate_bytes`, `retain_files`) and transcripts
  are capped per run by `max_transcript_bytes`; a run breaching the cap is stopped, not
  silently truncated.
- **FR-11.5** Transcripts are pruned by age. `--no-transcript` disables them entirely, at
  the cost of the watch TUI, replay, and the file list in `get_changes` for unconfined
  runs — each stated, not discovered.
- **FR-11.6** The audit log is fsynced on run completion and on every refusal. A crash
  mid-write may lose the final partial line; it may not corrupt earlier lines.
- **FR-11.7** Neither artefact's path is ever returned to agent A.

### FR-12 — Limits and backpressure

The result byte cap bounds what reaches A. It bounds nothing else, so each stream has its
own limit:

- **FR-12.1** `max_event_bytes` caps a single JSONL line from B. A longer line is
  truncated, recorded as `event.oversized`, and does not grow the parser's buffer.
- **FR-12.2** `max_output_bytes` caps the enveloped result returned to A.
- **FR-12.3** `max_transcript_bytes` caps the run's whole transcript; breaching it ends
  the run with `transcript_limit`.
- **FR-12.4** `rate_per_minute` and `max_concurrent_runs` bound how many runs a caller can
  start. On breach the behaviour is `on_concurrency_limit`: `reject` (default, a typed
  error) or `queue` with a bounded `queue_depth`, FIFO, and a queued run that waits past
  its timeout is rejected rather than started late.
- **FR-12.5** Both stdout and stderr are drained continuously. A slow consumer of the
  transcript never blocks the child — the TUI reads the file, it does not sit in the path.
- **FR-12.6** B's output is validated as UTF-8 before sanitization; invalid sequences are
  replaced, not passed through, and bidi controls, zero-width characters and the Unicode
  tag block are stripped along with ANSI and C0/C1.

### FR-13 — Result delivery to the caller

A tool result has two halves — the text `content` blocks and `structuredContent` — and the
bridge advertises an output schema, so a host may render either half alone. Claude Code
renders `structuredContent` only.

- **FR-13.1** Every caller-facing string `await_agent` produces appears in **both** halves:
  the run's result, a `needs_input` question, the still-running progress digest, and the
  `superseded` pointer. A result carried in one half only is invisible to some hosts.
- **FR-13.2** The structured copy of B's words is the *enveloped* string, byte for byte
  identical to the text block. Raw vendor text never appears in structured output: the
  envelope's provenance marking must travel with the text into whichever half a host
  renders (SR-5).
- **FR-13.3** Bridge-authored text about the run — the progress digest, the `superseded`
  pointer — is carried as `notice`, outside the envelope, because it is not B's words.
- **FR-13.4** `await_agent` reports the same `confinement` and `sandboxed` as the `ask_*`
  that started the run. These describe how the run is contained, so a caller reading only
  the await result must not be told a confined, sandboxed run was neither. The values are
  read off the run, which is why the run itself has to carry them: a decision field left
  unset on the `Spec` makes every downstream surface — `await_agent`, `bridge runs`, the
  TUI header — under-report containment for every run.

### FR-14 — Vendor-reported errors on a clean exit

An agent can report an error in its own event stream and still exit 0. Codex does this on a
usage limit, and also on warnings about the **operator's own** config: a malformed agent role
file, a skill description shortened to fit its budget. A run that answered correctly and
exited 0 was observed emitting three such events (codex 0.154.0), so the presence of an error
event carries no verdict on the run.

- **FR-14.1** Error events in B's stream are recorded on the run and reach the caller even
  when the process exited 0, so what the vendor reported is visible rather than inferred from
  exit status.
- **FR-14.2** The structured field is `vendor_errors`, a **count** of the error events the
  stream carried. It says "the agent's stream reported N errors" and nothing else. A non-zero
  count is **not** a failure verdict: the run's state is the only field that says whether the
  run failed. What counts as an error is the vendor's own event type, never a match against
  error prose.
- **FR-14.3** The messages are B's words and go inside the envelope with the rest of the
  body. They are budgeted **before** the answer, so a run whose output reaches
  `max_output_bytes` shortens the answer and keeps the messages: a count whose evidence was
  truncated away is a signal with nothing behind it.
- **FR-14.4** An error event carrying no message still increments the count. The occurrence
  is what the count reports, not the availability of text to show. Both the count and the
  retained messages are capped per run.
- **FR-14.5** The run's state is unchanged: exit status decides that. `completed` with a
  non-zero `vendor_errors` is a real and distinct outcome, and the common one.

---

## 4. Security requirements

Priority order: **security > correctness > safety > maintainability > convenience > speed.**

### SR-1 — Zero credential handling

The bridge never reads, stores, forwards or logs a credential. B authenticates through the
operator's own pre-existing login (`~/.codex/auth.json`, Claude's keychain/OAuth). Config
**may not** carry API keys; a config containing one is rejected at load. The environment
passed to B is deny-by-default with a named allowlist.

### SR-2 — Sandbox by default, write confined to a worktree by default

- Default sandbox mode is **read-only**, enforced via the target CLI's own flag list
  (see [`04`](04-config-schema.md) §3 for the exact, trap-corrected lists).
- Write mode is opt-in per adapter via `mode: write` in config, never per call. A call
  may narrow to read-only; it can never widen.
- **A write-mode run does not touch the caller's working tree by default.** The bridge
  creates a disposable git worktree from the current `HEAD`, runs B there, and returns a
  diff that must be accepted. See FR-10.
- An adapter may set `worktree: off`, which lets B edit the caller's tree directly. This
  is a deliberate operator choice, not a default, and it removes the only control that
  bounds a write run's filesystem blast radius. The bridge does not refuse it; it makes
  it loud (FR-10.6).
- The bridge never passes a bypass flag (`--dangerously-*`, `--dangerously-skip-permissions`,
  `--yolo`, `--allow-all`) under any configuration. Hard-coded refusal, not config.

### SR-3 — Working-directory validation

- Config declares `allowed_roots`.
- A caller-supplied `cwd` is resolved (symlinks included) and must lie inside an allowed
  root, or the call is refused. Path traversal is a rejected request, not a judgement call.
- **This bounds where B works, not what B can read.** No vendor read-only sandbox confines
  reads to the working directory, so a read-only run can still read every file the
  operator can read. The control is *enforced*; the property it gives you is
  working-directory validation, not filesystem containment
  ([`11`](11-domain-model.md) §6).
- On Windows the resolution additionally collapses junctions and 8.3 short names, compares
  case-insensitively, and rejects UNC paths, device namespaces (`\\?\`, `\\.\`),
  alternate data streams and reserved device names before the containment test. A path
  that cannot be canonicalised is refused, never approximated.
- The adapter passes the workspace to B via the CLI's own flag (`codex -C`, `claude --add-dir`).

### SR-4 — No shell, ever

Argv is constructed as a list. No `sh -c`, no string interpolation into a command line, no
`os/exec` with a shell. Placeholder values are always separate argv elements.

Windows has no argv — the OS passes a single command-line string that each program parses
itself, so "no shell" is not free there. The bridge therefore: sets `SysProcAttr.CmdLine`
explicitly with MSVCRT-correct quoting rather than relying on default joining; refuses
`.bat`, `.cmd` and `.ps1` as an adapter `command`, because `cmd.exe` re-parses their
arguments and reintroduces metacharacter injection; and rejects any argument containing a
character that cannot be safely quoted.

### SR-5 — Untrusted output

B's output is data, never instruction. Before it reaches A:

- wrapped in an explicit untrusted-data envelope naming its source agent,
- ANSI escapes and C0/C1 control characters stripped,
- content resembling system/tool-call markup neutralised,
- truncated at a hard byte cap with an explicit truncation marker,
- never auto-executed, never written to disk on A's behalf.

The same treatment applies to B's *questions* (FR-4) and to anything the watch TUI renders.

### SR-6 — Recursion and cost bounds

- A depth marker is injected into B's environment; a bridge started at or beyond
  `max_depth` exposes no delegation tools. **This is best-effort, not a control.**
  VERIFIED 2026-09-14: Codex does not propagate its own environment to MCP servers it
  spawns — child servers receive a fixed 12-variable allowlist, and
  `shell_environment_policy.inherit=all` does not widen it. The marker reaches a nested
  bridge only if the operator adds it to that server's `mcp_servers.<name>.env` map. The
  **enforced** recursion controls are vendor config isolation (`--ignore-user-config`,
  `--strict-mcp-config`), which stop B from loading a nested bridge at all, plus the
  wall-clock, breadth and concurrency limits.
- Per-run wall-clock timeout with process-group kill.
- Cap on agent-originated steers per run.
- Max-turns passed to B where the CLI supports it.
- Hard output byte cap.

### SR-7 — Config trust

- Only the user-level config is read. A `.agents-bridge.yml` inside a repository is
  **never** read. Cloning a hostile repository cannot add an adapter, grant write access,
  widen a root or change a limit.
- Config is rejected if it is group- or world-writable.

### SR-8 — Control channel

- Transport for MCP is **stdio only**. No TCP listener, ever — on any platform.
- The runtime-control and watch channel is a local IPC endpoint restricted to the current
  user, and the server verifies the connecting peer's identity on every connection:
  - **Linux/macOS:** Unix domain socket, mode `0600`, `SO_PEERCRED`/`LOCAL_PEERCRED`, peer
    UID must equal the server UID.
  - **Windows:** named pipe created with an explicit DACL granting only the current user's
    SID, plus a per-connection check that the client process token SID matches.
    `FILE_FLAG_FIRST_PIPE_INSTANCE` is set so the endpoint name cannot be squatted.
- A peer that fails the identity check is refused and the refusal is audited.

### SR-9 — Supply chain

- Single static Go binary. No package manager resolution at MCP start-up, so no `npx -y`
  fetching code on every launch.
- Releases are tagged, checksummed, signed (cosign) and reproducible; provenance attested
  in CI.

---

## 5. Non-functional requirements

| ID | Requirement |
|---|---|
| NFR-1 | Cold start of the MCP server under 50 ms; it starts on every host launch. |
| NFR-2 | Adding a new agent is a config edit of roughly 10–20 lines, no rebuild. |
| NFR-3 | v1.0 supports Linux and macOS; Windows is v1.1. All six targets compile and are built in CI from Phase 0. See FR-9. |
| NFR-4 | No persistent daemon in v1; runs are owned by the MCP server process. |
| NFR-5 | Transcript files and audit log are append-only and safe to `tail -f`. |
| NFR-6 | Orphan handling is per-platform and stated honestly: **Windows** — job object with `KILL_ON_JOB_CLOSE`, kernel-enforced, survives a hard kill of the bridge. **Linux** — `PR_SET_PDEATHSIG` on the direct child plus a cgroup where available. **macOS** — best effort only; a `SIGKILL`ed bridge can leave descendants, because nothing sends `kill(-pgid)` on its behalf and a child may `setsid()` out of the group. |

---

## 6. Out of scope for v1

- Remote/HTTP MCP transport, and anything requiring network authentication.
- A shared multi-host daemon.
- Credential storage, secret-manager integration, headless CI authentication.
- Automatic answering of B's questions by A without operator involvement.
- OS-level isolation (bubblewrap/landlock/containers) on any platform.
