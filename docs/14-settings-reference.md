# Settings reference

Every key the bridge accepts, with its type, default, effect, and the rule that rejects a
bad value. The normative shape is [`schema/config.schema.json`](../schema/config.schema.json);
the defaults and behaviour below are read from the implementation
(`internal/config/load.go`, `internal/config/semantics.go`, `internal/config/types.go`).

Where the file lives, how to register the bridge in a host agent, worked config files,
placeholder binding and the operator commands are in [`04`](04-config-schema.md). This page
is the key-by-key companion to it.

## 1. How the file is read

The loader runs in a fixed order, and every failure below is a load error that stops the
server. Nothing here is a warning.

1. The config directory is resolved through symlinks and refused if it is owned by another
   user or is group- or world-writable.
2. The file is opened once, and ownership and mode are checked on that descriptor: it must
   be a regular file owned by the running user, not group- or world-writable. On Windows
   the equivalent DACL check is not implemented, so the config is refused outright unless
   the check is skipped (`internal/config/perm_windows.go`).
3. The file is read with a 1 MiB cap. A larger file is refused before parsing.
4. The YAML must be exactly one document. Duplicate mapping keys are an error, so a
   security setting cannot be shadowed by a later copy of itself.
5. The document is validated against the JSON Schema.
6. It is decoded into typed structs with unknown-field rejection: a misspelled key is a
   fault, never a silently ignored setting.
7. Defaults are applied (section 3).
8. The 21 semantic rules run (section 9).
9. Each adapter's `command` is resolved on `PATH`, last, so a structurally invalid config
   never touches the filesystem.
10. The adapter whose `agent_id` equals `--host` is dropped, along with any adapter whose
    resolved executable matches the host's.

A repository-local config file is never read, on any path. That is not configurable.

## 2. Top level

| Key | Type | Required | Meaning |
|---|---|---|---|
| `version` | integer, const `1` | yes | Config format version. Any other value is a schema error |
| `defaults` | object | no | Limits and ceilings that apply to every run (section 3) |
| `features` | object | no | Global feature switches (section 4) |
| `allowed_roots` | array of 1–64 strings | yes | Directories a delegated run may work in (section 5) |
| `env_allowlist` | array of ≤64 names | no | Extra environment variables passed to the child (section 6) |
| `audit` | object | no | Audit log location and retention (section 7) |
| `agents` | map of 1–32 adapters | yes | The delegated agents (section 8) |

Unknown top-level keys are rejected.

## 3. `defaults`

| Key | Type | Default | Range | Effect |
|---|---|---|---|---|
| `timeout_s` | integer | 900 | 10–86400 | Wall-clock budget per run. On expiry the child's whole process group is killed and the caller gets a sanitized timeout result |
| `max_output_bytes` | integer | 32768 | 1024–1048576 | Cap on the text returned to the calling agent, applied by the sanitizer on every return path |
| `max_prompt_bytes` | integer | 30720 | 256–1048576 | Cap on the caller's prompt. A larger prompt is refused with `prompt_too_large` before anything spawns |
| `max_event_bytes` | integer | 262144 | 1024–4194304 | Cap on a single parsed stream event. A longer line is truncated rather than buffered without bound |
| `max_transcript_bytes` | integer | 52428800 | 65536–1073741824 | Cap on one run's JSONL transcript |
| `max_depth` | integer | 1 | 0–4 | Delegation depth this server permits. `0` disables delegation: the server exposes no delegation tools at all. Absent means 1 — see the tri-state note below |
| `max_continuations` | integer | 3 | 0–50 | Links a `needs_input` continuation chain may have. `0` means a stopped run may never be continued |
| `max_concurrent_runs` | integer | 4 | 1–32 | Live runs allowed at once. A run past the cap is refused with `too many concurrent runs` |
| `max_agent_steers` | integer | 3 | 0–50 | Steer messages **agent A** may inject into one run. Operator steers are uncapped |
| `max_turns` | integer | 40 | 1–1000 | Value bound to `{{max_turns}}`. It reaches the child only if the adapter's `invoke.args` reference that placeholder |
| `question_timeout_s` | integer | 300 | 10–3600 | How long the gate waits for an operator answer to a delegated agent's question, and how long a run then rests in `needs_input` before failing |
| `run_retention_s` | integer | 3600 | 60–604800 | How long a finished run stays resolvable for `await_agent`, `get_changes` and `list_runs` |
| `rate_per_minute` | integer | 20 | 1–600 | Runs a caller may start per rolling minute, counted independently of concurrency so a start-and-cancel loop still trips a limit |
| `on_concurrency_limit` | `reject` \| `queue` | `reject` | — | What to do at the concurrency cap. See section 10: `queue` is accepted but behaves as `reject` |
| `queue_depth` | integer | 0 | 0–64 | Queue length for `on_concurrency_limit: queue`. Must be ≥ 1 when that policy is set (rule 14). See section 10 |
| `allow_write_mode` | boolean | `false` | — | Ceiling for `mode: write`. No adapter may declare write mode while this is false (rule 6) |
| `allow_unconfined_write` | boolean | `false` | — | Second ceiling, required together with `allow_write_mode` for `worktree: off` (rule 7) |
| `agent_acceptance` | boolean | `false` | — | Whether the calling agent may apply a write run's diff itself. False means only the operator's `bridge accept` can land it |

**Tri-state integers.** `max_depth` and `max_continuations` are pointers in the loader, so
absent and `0` are different: absent takes the default (1 and 3), an explicit `0` means
"none". Every other integer above is zero-defaulted, so writing `0` is the same as omitting
the key and yields the default.

## 4. `features`

Global switches. Each is tri-state on purpose: absent means "use the default below", not
`false`. A per-adapter declaration and a runtime `bridge disable` may narrow a feature
further; nothing widens it (`internal/policy/policy.go`'s `FeatureEnabled` consults the
config ceiling first).

| Key | Default | Effect when on |
|---|---|---|
| `stream` | `true` | A `tier: full` adapter's output is parsed into events and transcribed. With it off, a full-tier run falls back to buffered output |
| `watch` | `true` | Declared for the operator TUI. Accepted and reportable, consulted by no code path in v1 — section 10 |
| `interactive` | `false` | Turns interactive mode on. A run gets a gate socket and per-run mcp-config only when this is on **and** the adapter declares `capabilities.interactive` |
| `operator_steering` | `true` | Declared for `bridge steer`. Accepted and reportable, consulted by no code path in v1 — section 10 |
| `agent_steering` | `false` | Exposes the `steer_agent` tool to the calling agent. Off by default: it removes the human from the loop |
| `sessions` | `true` | Declared for session resumption. Accepted and reportable, consulted by no code path in v1 — section 10 |
| `audit` | `true` | Opens the audit log. With it off, no audit writer exists and every audit call is a no-op |
| `progress_notifications` | `true` | Sends MCP `notifications/progress` when the host supplied a progress token |

An unknown feature name is off: `Features.Enabled` denies by default, and
`bridge enable`/`disable` reject a name that is not in the list above.

## 5. `allowed_roots`

Absolute paths, or `~`-prefixed. 1 to 64 entries; the key is required.

Each root is canonicalised at startup, and a root that does not resolve is a startup
failure, not a per-call surprise. A run's `cwd` must canonicalise to a path contained by
one of these roots; a request outside them is refused with `root_violation` and a message
that does not reveal whether the path exists. A call that omits `cwd` gets the **first**
root, never the bridge's own inherited working directory.

## 6. `env_allowlist`

Names matching `^[A-Za-z_][A-Za-z0-9_]{0,63}$`, up to 64 of them. Default empty.

This list **extends** a built-in per-platform baseline (`HOME`, `PATH`, `TERM`, `LANG` on
POSIX; `SystemRoot`, `USERPROFILE`, `APPDATA`, `LOCALAPPDATA`, `PATHEXT`, `TEMP`, `COMSPEC`
on Windows); it does not replace it. Everything else is stripped from the child's
environment.

A credential-shaped name is a load error, not a warning: the loader refuses any name
matching `API_KEY`, `_TOKEN`, `TOKEN`, `SECRET`, `PASSWORD`, `CREDENTIAL`, `_KEY`,
`SESSION_KEY` or `AUTH`, case-insensitively. The delegated agent authenticates with its own
existing login; the bridge never hands a credential to a subprocess (SR-1).

## 7. `audit`

| Key | Type | Default | Effect |
|---|---|---|---|
| `path` | string | `<state dir>/audit.jsonl` | Append-only JSONL log, opened `0600`, its directory created `0700` |
| `bodies` | boolean | `false` | `true` stores prompt and response text in the log. The default stores keyed digests only |
| `hmac_key_file` | string | `<state dir>/audit.key` | Per-install HMAC key for those digests, created on first use. Digests are keyed because an unsalted hash of a short prompt is a dictionary oracle for anyone who can read the log |
| `rotate_bytes` | integer | 104857600 | Size at which the log rotates. 1 MiB–1 GiB |
| `retain_files` | integer | 10 | Rotated files kept. 1–100 |

The whole block is inert when `features.audit` is off.

## 8. `agents.<agent_id>`

The map key is the `agent_id`: `^[a-z][a-z0-9_]{0,31}$`. It becomes an MCP tool name
(`ask_<agent_id>`) and part of a state path, which is why it is constrained at load. One to
32 adapters.

Required in every adapter: `command`, `tier`, `mode`, `invoke`.

| Key | Type | Default | Effect |
|---|---|---|---|
| `description` | string ≤600 chars | — | Reaches the calling model's context every session; keep it short |
| `command` | string | — | Executable name or path, resolved via `PATH` to an absolute regular file. Shell metacharacters are refused by pattern; `.bat`, `.cmd` and `.ps1` are refused after resolution (rule 9) |
| `tier` | `basic` \| `full` | — | `basic` is pure YAML and forces every capability false (rule 1). `full` requires a `stream.Parser` registered under the same id, checked at server startup (rule 2) |
| `mode` | `read-only` \| `write` | — | Selects which sandbox flag list is emitted. It is not confinement. `write` requires `defaults.allow_write_mode` (rule 6) |
| `worktree` | `required` \| `off` | `required` | `required` runs a write-mode run in a disposable git worktree and stops in `awaiting_acceptance` with a diff. `off` writes straight into the caller's tree and needs both write ceilings (rule 7); such a run is labelled UNCONFINED everywhere it appears |
| `sandbox_enforced` | boolean | `true` | Declares whether the vendor enforces anything for this mode. `false` is how an adapter is allowed to ship with no sandbox flags, and it is surfaced as UNSANDBOXED (rule 5) |
| `allowed_models` | array ≤32 of `^[A-Za-z0-9._:-]{1,64}$` | — | Model names a caller may select. A request outside the list is refused with `model_not_allowed`. Absent means the caller may not choose a model |

### 8.1 `capabilities`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `stream` | boolean | `false` | The CLI emits a parseable event stream |
| `resume` | boolean | `false` | The CLI can resume its own session. Requires `resume_invoke` (rule 3), and `resume_sandbox` wherever the adapter has a sandbox and the vendor's resume surface differs (rule 15) |
| `steer` | `true` \| `false` \| `queued` | `false` | `true` is mid-turn delivery, `queued` is delivery at the next turn boundary. Either requires a `steer` block; `false` forbids one (rules 4 and 17) |
| `interactive` | boolean | `false` | The CLI can route a question to the operator through the gate. Requires `stream: true` (13), a non-empty `interactive` flag list for the mode (18), `{{gate_flags}}` in `resume_invoke.args` when that block exists (19), `mode: read-only` (20), and the literal `AskUserQuestion` somewhere in the adapter's flags for its mode (21) |
| `structured_output` | boolean | `false` | The CLI can emit a schema-validated final result. It says nothing about the intermediate stream |

`tier: basic` forces all five false (rule 1).

### 8.2 Flag lists

Four maps keyed by mode (`read-only`, `write`), each a list of up to 64 whole argv
elements:

| Key | Purpose |
|---|---|
| `sandbox` | The vendor flags that confine a run, expanded into `{{sandbox_flags}}`. Re-emitted on every invocation, never persisted in session state |
| `resume_sandbox` | The same policy expressed for the vendor's resume subcommand, expanded into `{{resume_sandbox_flags}}`. Exists because a resume surface that silently drops the sandbox runs writable |
| `interactive` | The flags that point the child at this bridge's gate, expanded into `{{gate_flags}}` |

An empty or absent `sandbox` list for the adapter's own mode requires
`sandbox_enforced: false` (rule 5). No element of any list may be a `--dangerously*`,
`--allow-dangerously*`, `--yolo` or `--allow-all` flag (rule 8), and every `{{placeholder}}`
must be one the expander knows (rule 11).

### 8.3 `invoke` and `resume_invoke`

| Key | Type | Default | Effect |
|---|---|---|---|
| `args` | array ≤64 of strings | required | Argv template. Each element is one argv element; a placeholder binds whole-element, except the `--flag={{placeholder}}` form. No caller-supplied value may change the element count |
| `prompt` | `stdin` \| `stdin_stream_json` \| `argv` | `stdin` | How the prompt reaches the child. `argv` exposes it in process listings and crash reports and is a last resort; it is capped at `max_prompt_bytes` |
| `close_stdin_after` | `result` \| `never` | `result` | When to close the child's stdin. Required with `stdin_stream_json` (rule 16), because such a child blocks on stdin EOF instead of exiting when its turn completes |

`resume_invoke` has the same shape and is required when `capabilities.resume` is true.

### 8.4 `steer`

| Key | Type | Effect |
|---|---|---|
| `mode` | `command` \| `stdin` | required. `command` spawns the vendor's own steer subcommand; `stdin` writes a record into the running child's stdin |
| `args` | array ≤64 | Required for `mode: command` |
| `record` | `user_message` | Required for `mode: stdin`. The record is built by a typed JSON encoder; a string template is a load error, because interpolating into JSON is an injection primitive |

### 8.5 `stream`

| Key | Type | Effect |
|---|---|---|
| `format` | `jsonl` | The only supported stream format |
| `source` | `stdout` \| `stderr` | Which pipe carries the event stream |
| `vendor_session_id_path` | string | JSON path at which the vendor's own session id appears. That id is recorded by the bridge and never returned to the caller |
| `final_message` | object of `event`, `where`, `text` | Where the final assistant text lives when it is not a distinct event type |
| `usage` | string | JSON path to the token-usage object |

## 9. Load rules

The 21 numbered semantic rules are listed in full in the `$comment` block of
[`schema/config.schema.json`](../schema/config.schema.json) and enforced in
`internal/config/semantics.go`, except rule 2 (a `tier: full` adapter must have a registered
parser), which is enforced in `cmd/bridge/serve.go` so the loader need not depend on the
stream package. Each is a load error with a test.

`bridge validate --config <path>` exercises an adapter declaration against a stub agent
with no vendor CLI, no credentials and no network, and prints one PASS/FAIL/SKIP line per
check. `bridge doctor --host <id> --config <path>` reports this machine's setup instead:
paths, permissions, adapter resolution and host detection.

## 10. Settings accepted but not yet acted on

Each of these loads, validates and appears in `bridge status`, and no code consults it.
They are documented here rather than removed so an operator does not read behaviour into a
key that has none.

- `defaults.on_concurrency_limit: queue` and `defaults.queue_depth`. Rule 14 enforces that
  `queue` comes with a `queue_depth` of at least 1, but the run registry has one admission
  path and it refuses at the cap (`run.ErrConcurrencyLimit`). A config asking to queue
  behaves as `reject`.
- `features.watch`, `features.operator_steering` and `features.sessions`. All three are
  valid names for `bridge enable`/`disable` and are reported by `bridge status` and
  `bridge doctor`, but no behaviour is gated on them: the TUI, operator steering and
  session resumption all run regardless. Turning one off changes what `status` prints,
  nothing more.
- `defaults.max_turns` reaches a child only through the `{{max_turns}}` placeholder. An
  adapter whose `invoke.args` never reference it ignores the setting entirely.
