# Adapter authoring guide

How to add a new delegated agent. This is a task-oriented companion to
[`04-config-schema.md`](04-config-schema.md), which is normative — where this guide and
that file appear to disagree, `04` and `schema/config.schema.json` win. Ground every
config key you use against the schema; this guide does not invent any.

## 1. Pick a tier

An adapter entry under `agents:` declares `tier: basic` or `tier: full`. The difference
is what config alone can promise.

**`tier: basic`** is pure YAML: `invoke`, sandbox flags, cwd confinement, buffered
output, audit. Every capability (`stream`, `resume`, `steer`, `interactive`,
`structured_output`) is forced `false`; declaring any of them `true` on a basic adapter is
a load error (`schema/config.schema.json` semantic rule 1). No rebuild, no Go code. This
is the whole point of the tier split: config can describe *how to invoke* a CLI, but it
cannot describe how to parse a vendor's event stream or detect that the CLI is asking a
question.

**`tier: full`** additionally requires a Go adapter registered in-tree under the same
`agent_id`, which means a `stream.Parser` registered in `ParserFor` (rule 2). A
`tier: full` entry with no registered parser is a **fatal error at startup**, not a
degraded fallback to buffered output — a full-tier adapter that silently behaved like a
basic one would claim to stream while not streaming. If you are not modifying the Go
source tree, you are writing `tier: basic`.

Start with `tier: basic` even for a CLI you eventually want streaming for. You can try it
against the real binary with no build step, and promote it to `tier: full` once the
one-shot shape works.

## 2. Write a `tier: basic` adapter

The complete, schema-valid `antigravity` example in `docs/04-config-schema.md` §3 is the
reference shape:

```yaml
agents:
  mycli:
    description: One-line description of what this CLI is good for.
    command: mycli
    tier: basic
    mode: read-only
    sandbox_enforced: false   # only if the CLI has no real read-only flag; see §4
    capabilities:
      stream: false
      resume: false
      steer: false
      interactive: false
      structured_output: false
    sandbox:
      read-only: []           # or the CLI's actual read-only flags, if it has any
    invoke:
      args: ["--print", "--"]
      prompt: argv
```

Steps:

1. Find the CLI's headless / non-interactive flag (the thing that makes it run without a
   terminal attached and exit when done). That goes in `invoke.args`.
2. Decide how the prompt reaches the CLI: `invoke.prompt` is `argv`, `stdin`, or
   `stdin_stream_json` (full-tier only, §5). Prefer `stdin` over `argv` when the CLI
   supports it — an argv-bound prompt is capped at `max_prompt_bytes` and is visible in
   process listings.
3. Find the CLI's actual sandbox / permission-restriction flags for read-only use and put
   them in `sandbox.read-only`. If the CLI has no such flags, see §4 — do not leave the
   list empty without also setting `sandbox_enforced: false`.
4. Add the adapter block to your `~/.config/agents-bridge/config.yaml` and run
   `bridge doctor` — it resolves the `command` via `PATH`, checks its `--version` runs,
   and reports any adapter carrying a forbidden flag or executable type.
5. Call `ask_mycli` (or the equivalent tool name) from a host agent, or invoke it directly
   if you have a harness, and confirm you get the output you expect.

No code change, no rebuild, no PR to this repo required for step 4 or 5 — that is the
tier-basic contract.

## 3. Placeholder rules

Every `{{...}}` token you write in `invoke.args` is checked at load and at expansion
(`04-config-schema.md` §4, full rule text there):

- A placeholder expands to **exactly one** argv element. The only exception is
  `{{sandbox_flags}}`, which expands to the fixed list under `sandbox.<mode>`.
- Never concatenate a placeholder into a larger string, with one exception: the
  `--flag={{placeholder}}` binding form is a single argv element and is *required* for
  any free-text value the target CLI would otherwise misparse as a flag.
- An unknown `{{...}}` token is a load error (rule 11) — it is not silently left as
  literal text.
- Placeholders bound to a structured record (e.g. a `steer.mode: stdin` payload) go
  through a typed JSON encoder, never string templating; a string template there is a
  load error.

The available placeholders and what each is bound to are listed in
`04-config-schema.md` §4 — `{{prompt}}`, `{{cwd}}`, `{{vendor_session_id}}`,
`{{message}}`, `{{model}}`, `{{max_turns}}`, `{{sandbox_flags}}`. There is no
`{{session_id}}`: a caller passes an opaque bridge handle, never a vendor session id
directly (see `02-architecture.md` §3).

## 4. Sandbox flags

`sandbox.read-only` and `sandbox.write` are the argv lists substituted for
`{{sandbox_flags}}` for the adapter's active `mode`. Two things to get right:

- **Prove the flag is binding, not merely accepted.** `docs/12-spike-results.md` records
  two cases where a documented-looking flag did not do what it appeared to: Codex's
  `--sandbox read-only` is defeated by `approvals_reviewer="auto_review"` unless
  `approval_policy="never"` is also set, and Claude's `--allowed-tools` does not remove
  `Bash` — only `--tools` does. Do not trust a CLI's own `--help` text for a sandbox
  claim; verify against the real binary the way `docs/12` did, and record what you
  verified.
- **If the CLI has no real sandbox**, set `sandbox_enforced: false` (schema semantic rule
  5: an empty or absent sandbox list for the adapter's mode is a load error unless this is
  set). This surfaces as **UNSANDBOXED** in the tool description, in `bridge status`, and
  in every audit record for the adapter's runs — it does not silently claim a protection
  the CLI cannot provide.

## 5. What `tier: full` additionally needs

Beyond the basic-tier YAML, a full adapter needs, in Go source under `internal/`:

- **A registered `stream.Parser`** in `ParserFor`, turning the CLI's event stream into
  the bridge's internal event set (`docs/02-architecture.md`'s event model). Unknown
  event types are kept as `raw` and never rendered as instructions.
- **`resume_invoke`**, if `capabilities.resume: true` — schema rule 3. If the adapter also
  declares a `sandbox` list, `resume_sandbox` is required too (rule 15): a vendor resume
  surface that silently drops the sandbox is writable by default, which is exactly what
  `codex exec resume` does if you omit this.
- **A `steer` block**, if `capabilities.steer` is `true` or `queued` (rule 4): `mode:
  command` requires `steer.args`, `mode: stdin` requires `steer.record`. Declaring a
  `steer` block while `capabilities.steer` is `false` is *also* a load error (rule 17) —
  an unusable channel cannot sit in config looking functional.
- **An `interactive` flag list for the adapter's mode**, if `capabilities.interactive:
  true` (rule 18), plus `capabilities.stream: true` (rule 13) and `mode: read-only` (rule
  20 — v1 denies every approval wholesale, so a write-mode interactive adapter would have
  every tool call refused by the gate despite looking correctly configured). If the
  adapter also has `resume_invoke`, its `args` must reference `{{gate_flags}}` (rule 19),
  or a resumed run loses its gate routing partway through a session.
- **`AskUserQuestion` present in whatever flag bounds the child's tool surface**, if
  `capabilities.interactive: true` — rule 21, a load error. Spike C5
  (`docs/12-spike-results.md`) established that for Claude, `--tools` is the flag that
  genuinely bounds the surface — `--allowed-tools` is a permission hint and leaves 29
  tools live. If the adapter's `--tools` list omits `AskUserQuestion`, the child has no
  way to raise a question at all: observed live against a real CLI, the model reported the
  tool was unavailable and asked in plain text instead, so interactive mode's transport
  worked but its trigger never fired. The loader matches the name anywhere in the flags
  the adapter passes for its mode — sandbox, resume sandbox, interactive list, or either
  invocation template — because which flag carries the roster is vendor knowledge it does
  not have. Grant it in the `interactive` list rather than the sandbox list, repeating the
  roster flag with the tool appended (`--tools Read,Glob,Grep,AskUserQuestion`): the
  interactive list lands after `{{sandbox_flags}}` and last flag wins (C8), so a
  non-interactive run of the same adapter keeps the narrower surface. This is the shape
  `examples/config.yaml` and `docs/04-config-schema.md` §3 now ship.
- **`close_stdin_after`**, if `invoke.prompt: stdin_stream_json` (rule 16) — such a child
  blocks on stdin EOF rather than exiting when its turn completes; the bridge has to know
  when to close it.

The `claude` and `codex` adapters in `docs/04-config-schema.md` §3 are the two shipped
`tier: full` examples; read them alongside their inline comments, which record the vendor
traps each flag exists to work around.

## 6. Capability flags: what each does NOT imply

Declaring a capability turns on a *mechanism*, never a guarantee about the vendor's
behaviour:

- `capabilities.stream: true` means events are parsed as they arrive. It does not mean
  every event type is understood — unrecognised ones pass through as `raw`.
- `capabilities.resume: true` means a `{{vendor_session_id}}` from a prior run can be fed
  back in via `resume_invoke`. It does not mean the resumed process inherits the original
  sandbox — that is exactly what rule 15 exists to force you to re-assert.
- `capabilities.steer: true` means a mid-run message can be delivered through the CLI's
  own channel. It does not mean the message interrupts the current turn: `steer: queued`
  (the honest value for both shipped adapters where steering exists at all) means
  delivery waits for the next turn boundary. A CLI whose channel is store-and-forward
  only for the *next run*, like Codex's `codex queue`, gets no `steer` block at all —
  declaring `true` there would describe a channel that cannot reach a run in flight.
- `capabilities.interactive: true` means a question raised by the child can reach an
  operator through the gate transport. It does not mean every kind of vendor prompt is
  covered — only the CLI's own approval/permission-prompt tool-call surface, wired
  through `--permission-prompt-tool` or equivalent. A CLI with no correlated approval
  request (Codex's `exec` mode, run with `approval_policy="never"`) cannot declare this
  honestly; it fails closed into `needs_input` instead.
- `capabilities.structured_output: true` means the CLI can emit a schema-validated final
  result on request. It says nothing about the intermediate event stream's shape.

## 7. Semantics rules that refuse a dishonest adapter

`schema/config.schema.json`'s `$comment` lists 20 rules the JSON Schema itself cannot
express, enforced by `internal/config/semantics.go` as **load errors, never warnings** —
a misconfigured adapter fails to start rather than running with a silently narrower
guarantee than it claims. The ones most relevant while authoring a new adapter:

| Rule | What it refuses |
|---|---|
| 1 | A `tier: basic` adapter declaring any capability `true` |
| 2 | A `tier: full` adapter with no registered Go implementation |
| 3, 4 | `resume`/`steer` capabilities declared without the config block they need |
| 5 | An empty sandbox list without `sandbox_enforced: false` |
| 6, 7 | `mode: write` / `worktree: off` without the matching `defaults` ceiling |
| 8 | Any `--dangerously*`, `--allow-dangerously*`, `--yolo` or `--allow-all` flag in `args` |
| 9 | `command` not resolving to a regular executable via `PATH`; `.bat`/`.cmd`/`.ps1` on Windows |
| 10 | `agent_id == --host` (and, as a secondary check, a resolved executable matching the host's) |
| 11 | An unknown `{{placeholder}}` |
| 13, 18, 19, 20 | The `interactive` preconditions in §5 above |
| 15, 16, 17 | The `resume_sandbox` / `close_stdin_after` / stray-`steer`-block cases in §5 above |

Read the full rule text in `schema/config.schema.json`'s `$comment` — it is the
authoritative list; this table is a pointer into it, not a replacement. Every rule has a
test in `internal/config`; `spec-consistency` in CI loads every complete YAML example in
the docs through the real loader, so a documented example that violates one of these
rules fails CI, not just a reader's own attempt.

## 8. Validating your adapter

`bridge validate` exercises a declaration end to end against a stub agent, so an adapter can
be proven before a vendor CLI, an API key or a network is involved:

```bash
bridge validate --config <path> [--agent <id>] [--stream-fixture <file>] [--json]
```

It loads the config through the real loader — schema first, then every semantic rule — and
then, for each adapter (or only `--agent`), replaces the adapter's resolved command with the
bridge binary's own hidden `stub-agent` verb. Nothing else is substituted: your flags,
placeholders, prompt mode, sandbox lists, tier and mode are used exactly as written, and every
run goes through the same `internal/policy`, `internal/run` and `internal/sanitize` path a real
delegation does. The stub echoes the argv and the stdin it was handed, which is what makes
prompt delivery observable.

| Check | What a PASS means |
|---|---|
| `config` | The file loaded: schema and all semantic rules held |
| `argv` | Every placeholder expanded, no partial interpolation, and a hostile prompt changed neither the argv element count nor the element boundaries |
| `sandbox` | Every flag in `sandbox.<mode>` reached the built argv. `sandbox_enforced: false` passes but is reported as **UNSANDBOXED** |
| `prompt-delivery` | The prompt arrived by the mode you declared (`argv`, `stdin` or `stdin_stream_json`), proven by the stub echoing it back |
| `run-envelope` | The run completed and its output came back sanitized and wrapped in `untrusted_agent_output` |
| `timeout` | A child that outlived the deadline was killed and reported as a timeout, not left hanging |
| `exit-code` | A non-zero exit was reported as a failed run, with its partial output kept |
| `parser` | `tier: full` only: a `stream.Parser` is registered for the agent id, and with `--stream-fixture` it parsed that recorded stream into events |
| `interactive` | Interactive adapters only: rules 13, 18, 19, 20 and 21 hold |

Exit status is 0 when nothing failed and 1 when any check failed; `--json` prints the same
report as one object for CI. A check that does not apply is `SKIP`, which is not a fault.

Two limits are worth knowing before you read a green report as a release gate. The command
resolution is stubbed, so `validate` says nothing about whether your `command` exists on this
machine — that is `bridge doctor`. And the stub is not the vendor: it proves your declaration
is internally consistent and that the bridge drives it correctly, never that the real CLI
accepts those flags. Only the gated suites do that.

- `bridge doctor` — checks binary resolution, `--version`, allowed roots, and (per
  `04-config-schema.md` §6) that no adapter carries a forbidden flag or executable type.
- `go test ./internal/config/ -run TestDocumentedExamplesLoad -v` — the same check CI
  runs on every documented YAML example, if you are proposing an addition to the docs.
- `BRIDGE_CONFORMANCE=1 go test ./conformance/` — the only suite that invokes a real
  `claude` or `codex`. It spends credits, is not run by default, and is what `SECURITY.md`'s
  enforced/declared distinction rests on for any claim about real vendor behaviour.
