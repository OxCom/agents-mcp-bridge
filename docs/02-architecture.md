# Architecture

## 1. Shape

One Go binary, three roles selected by argv:

```
bridge serve --host <agent_id>     # MCP server over stdio; spawned by the host agent
bridge watch [run_id]              # operator TUI; connects to a serve instance
bridge <status|enable|disable|stop|runs>   # operator control CLI
```

The same binary is registered in every agent's MCP config. Only `--host` differs.

```
┌─ terminal 1 ──────────────────┐        ┌─ terminal 2 ─────────────┐
│ Claude Code (host agent A)    │        │ bridge watch             │
│   └─ MCP stdio ──┐            │        │   (operator TUI)         │
└──────────────────┼────────────┘        └───────────┬──────────────┘
                   │                                 │
          ┌────────▼─────────────────────────────────▼────────┐
          │ bridge serve --host claude                        │
          │  ├── adapter registry (from user config)          │
          │  ├── policy engine (roots, sandbox, depth, caps)  │
          │  ├── run manager (spawn, stream, steer, reap)     │
          │  ├── sanitizer (envelope, strip, cap)             │
          │  ├── audit writer (JSONL, hashes only)            │
          │  ├── gate (per-run socket; B's one address)       │
          │  └── control socket (unix, 0600, SO_PEERCRED)     │
          └────────┬──────────────────────────────────────────┘
                   │ argv list, no shell
          ┌────────▼────────────────┐
          │ codex exec --json …     │  ← target agent B
          │   sandbox: read-only    │
          │   cwd: allowed root     │
          └─────────────────────────┘
```

## 2. Components

### 2.1 Adapter registry

Loads the user config, validates it against the schema, and resolves each adapter's
`command` to an absolute path via `PATH`. Drops the adapter whose `agent_id` equals
`--host` before tools are registered, so the excluded agent never appears in
`tools/list`; resolved-executable comparison is a secondary check that can only add an
exclusion (FR-6.3).

Adapters come in two tiers. A **basic** adapter is entirely described by YAML and is
served by a generic one-shot runner: spawn, wait, buffer, sanitize, return. A **full**
adapter additionally binds to a Go implementation of `VendorStream`, registered in-tree
under the same `agent_id`:

```go
type VendorStream interface {
    Parse(line []byte) (Event, error)   // one vendor JSONL line -> normalised event
    SessionID(Event) (string, bool)     // extract the vendor session id
    Question(Event) (*Question, bool)   // does this event mean "B is asking"?
    Steer(h Handle, msg string) error   // inject, per the adapter's declared mode
}
```

The registry refuses a `tier: full` adapter with no registered implementation, and
refuses a `tier: basic` adapter that declares any streaming capability.

Emits one MCP tool per surviving adapter, with a description assembled from the adapter's
`description`, its capability flags and its current sandbox mode — so the model can see
that `ask_codex` is read-only without calling it.

### 2.2 Policy engine

The only component that answers "is this allowed". Every decision is a pure function of
(config, runtime toggles, request) and is logged with a reason on refusal.

Ordered checks for a delegation request:

1. host ≠ target (belt and braces; the tool should not exist)
2. depth marker < `max_depth`
3. feature enabled: global default → adapter override → runtime toggle
4. `cwd` resolves inside `allowed_roots`
5. sandbox mode permitted for this adapter
6. concurrency and rate within limits
7. prompt within size cap

No step is skippable by a tool argument. There is no "force" parameter anywhere in the
tool surface.

### 2.3 Run manager

Owns the lifecycle of a run.

- **Isolate (write mode only).** With `worktree: required` — the default — creates a git
  worktree under the state directory from the caller's `HEAD` and uses it as the child's
  working directory, diffs it on completion, sanitizes the patch and holds it pending
  acceptance. Removal happens on acceptance, rejection or expiry. With `worktree: off`
  the child runs in the validated `cwd` and edits it in place; there is no diff and no
  acceptance step, and the run is marked `confinement: none` everywhere it is displayed
  or recorded. Path validation against `allowed_roots` applies identically in both cases.

- **Spawn.** Builds argv as a list from the adapter template. Sets the child's working
  directory to the validated `cwd` — this, not a vendor flag, is the authoritative
  mechanism, because five surveyed agents have no working-directory flag at all and
  `--add-dir` widens rather than moves. Sets the child into its own process group — a POSIX process group,
  or a Windows job object with `KILL_ON_JOB_CLOSE`. Passes a deny-by-default environment plus the depth marker
  (`AGENTS_BRIDGE_DEPTH`) and a run id.
- **Stream.** Reads the adapter's declared stream format (JSONL on stdout for both
  reference adapters), normalises each vendor event into the bridge's internal event type,
  and appends it to the run transcript.
- **Steer.** Holds the adapter's steer handle — for Codex, the thread id parsed from the
  event stream; for Claude, the child's stdin pipe. Injection is a method on the handle.
- **Interact.** Recognises the vendor events that mean "B is asking" and routes them to
  TUI → elicitation → fail-closed, in that order. The TUI branch is taken only when a
  watcher is attached to *this run specifically* — a watcher on another run, or a picker
  that has not chosen a run yet, does not count, and the question falls through to
  elicitation instead. `cmd/bridge/gateconfig.go`'s `resolver(runID)` wires
  `internal/control.Server.WatchersAttachedTo(runID)` into `gate.Channels.TUIAttached`, so
  the per-run check is what runs at runtime, not the server-wide `WatchersAttached`.
  `bridge watch` (`cmd/bridge/watch.go`'s `runWatch`) is the only client that ever attaches:
  it first resolves exactly one run — there is no in-TUI run picker — then sends a single
  `VerbAttach` naming that run's id (`attachToRun`), for the life of the watch process, and
  detaches on every exit path. An attach with no run id is the server-side picker convention
  above; nothing in this codebase ever sends one. The operator-visible consequence: a
  question is offered to the TUI only while `bridge watch <run_id>` for that exact run is
  live; with no one watching, it goes to elicitation, or fails closed to `needs_input`.
- **Close.** A stream-json child does **not** exit when its turn completes — it blocks on
  stdin EOF (VERIFIED for `claude -p`, [`12`](12-spike-results.md) C1). The manager treats
  the vendor's terminal record, not process exit, as turn completion, and closes the
  child's stdin once no further steer is possible. A manager that waits on exit first
  deadlocks until the run timeout.
- **Reap.** On timeout, cancel, or server shutdown, kills the process group (POSIX) or
  terminates the job object (Windows), waits, and records the terminal state. No run
  outlives its server — on Windows this holds even on a hard kill of the bridge, because
  `KILL_ON_JOB_CLOSE` is enforced by the kernel.

### 2.3a Continuation (`needs_input` → `superseded`)

A `needs_input` run has no vendor resume path: `03-threat-model.md`'s fail-closed transition
kills the child and its gate, so an operator's answer can never reach the original blocked
tool call. What the operator can do instead is **continue** the run — start a new run seeded
with reconstructed context. This is not `codex exec resume`/`claude --resume`; it does not
touch `capabilities.resume` or `{{vendor_session_id}}` at all.

**Trigger.** `bridge answer <run_id> "<text>"` on a run resting in `needs_input`, or the
TUI's question panel answering a question the operator can see is no longer live (`docs/11`
§3 invariant 3), both routed through `cmd/bridge/continuation.go`'s `continueRun` via the
control channel's `VerbContinue`. `registerTools` never references `continueRun`: no MCP
tool reaches it, so a delegated agent can observe a continuation happening but never trigger
one (`08` Phase 3's operator-only rule extended to this surface).

**State transition.** `continueRun` starts the successor first, while the predecessor still
rests untouched in `needs_input` — `Registry.Start`'s admission check excludes the
predecessor's own id from the concurrency count (`liveCountExcludingLocked`), so the
successor is admitted against room the predecessor's `needs_input` rest state already
occupies. If `Start` fails, nothing has moved: the predecessor's continuation record is
restored and the operator's answer is not lost.

Once `Start` succeeds, the predecessor's chain-owned state — its change-store entry, its own
`Run`-level worktree handle, and its gate — retires **unconditionally**, in the same call,
regardless of what `Run.Supersede(successorID)` itself reports. `Supersede` is still attempted
and still labels the predecessor `superseded` (`docs/11` §3 invariant 7) on the ordinary path,
clearing its continuation record and writing `run.superseded` with the successor's id; the
successor's own `run.admitted` entry carries `resumed_from`. But retirement no longer depends
on that label: an earlier design tied both to `Supersede`'s success and left a residual window
— the predecessor's own `needs_input` deadline can lapse in the gap between `Start` (which
calls `sweepExpired` first) and `Supersede`, settling it to `StateFailed` on its own and making
`Supersede` refuse. Retirement tied to that outcome would leave the predecessor's diff
independently acceptable through the same window it was meant to close (`03` residual risks).
When `Supersede` is refused this way, the bridge audits it honestly as
`continuation.supersede_refused` rather than silently leaving the predecessor's state alive —
the predecessor still ends up retired, just labelled `failed` instead of `superseded`.

**Seed construction.** Four parts, concatenated in order, split by **authorship**, which is
what the untrusted-data envelope actually marks:

1. A bridge-authored preamble, outside any envelope: that this run continues run `<id>`, a
   bare **count** of how many files a prior attempt changed — never the names themselves, see
   point 2 — and which of the two provenances follows — the delegated agent's question(s) as
   untrusted data, the operator's answer(s) as a trusted instruction to act on.
2. Every question in the chain so far, in order, together with the prior attempt's changed
   file **names**, all inside the untrusted-data envelope (`sanitize.Envelope`) with
   attribution — the same envelope `needs_input` already uses for the question. A file's name
   is exactly as much the delegated agent's own text as the question is: the agent chose it,
   and a confined write run's `git status --porcelain=v1` leaves a space-free path unquoted,
   so a crafted name is bare attacker prose like any other agent output. Each name is rendered
   with `%q` and the list capped at `maxContinuationSeedFiles` (50) before the whole blob goes
   through `sanitize.Clean`.
3. The operator's answer to each turn, in the same order, outside the envelope, sanitized
   (`sanitize.Clean`, control-strip and cap, without the envelope's "do not act on this"
   preamble) and attributed to the operator as an instruction to follow. A truncated answer
   carries the same `[truncated by the bridge at N bytes]` marker `sanitize.Envelope` puts on
   truncated agent output, so an over-long operator answer is never silently cut with nothing
   saying so.
4. The chain's original prompt, outside the envelope, because it is caller-authored, not
   agent-authored.

`ContinuationRecord.Turns` holds every `(question, answer)` pair already in the chain before
this one; a depth-2+ continuation renders all of them, not just the latest, so the
human-in-the-loop context from every earlier turn survives into the seed.

Neither a question nor a file name is ever placed in the seed as bare instruction. Both are
authored by the untrusted delegated agent; re-injecting either unmarked, or in the
bridge-authored preamble, would let a misbehaving agent redescribe which region of the seed is
trusted, or write part of its own next prompt under the guise of a clarifying question,
defeating the human-in-the-loop reset `needs_input` exists to enforce — see `03` T24 for why
this is a threat-model control, not a formatting choice.

**Why the answer is split out of the envelope.** The envelope is a provenance marker, not a
content filter: its preamble tells the reader the enclosed text is not from the operator and
carries no instruction. An earlier revision of this feature put the question and the answer
inside that envelope together, which told the successor a human had answered its question
while, in the same breath, telling it not to act on anything inside the envelope — the
operator's answer included. That defeated the purpose of a continuation. The fix is one of
authorship, not formatting: agent-authored text (the question, the file names) is contained so
it cannot instruct; operator-authored text is deliberately **not** contained, so it can.

**Workspace hand-forward, confined write runs only.** Before the successor starts,
`worktree.ContinueFrom` stages and commits the predecessor's partial diff as a WIP commit on
the chain's branch (a fixed, bridge-owned git identity, not the operator's own config), using
a real `git commit`, not intent-to-add. A predecessor with no changes produces no commit. The
successor's worktree is checked out from that commit (or from the chain's original base, if
the predecessor was clean). `Worktree.base` — the sha the chain's very first predecessor
started from — is carried forward unchanged at every link, so `Diff()` from any link in the
chain reports the whole chain's cumulative work, not just the most recent one. The
predecessor's change-store entry is retired unconditionally once the successor's `Start`
succeeds (see the state-transition paragraph above), so exactly
one acceptable diff exists per chain, and it now lives on the successor
(`get_changes` on a retired run — `10` §2). **Unconfined write runs (`worktree: off`) refuse
continuation outright** (`ErrUnconfinedContinuation`): their edits already landed in the real
tree, so there is no staged diff to carry forward and no way to define one.

**Chain depth.** `defaults.max_continuations` (default 3, schema-enforced, `0`–`50`) bounds
how many links a chain may have; `ContinuationRecord.CheckDepth` refuses past that ceiling with
`ErrChainTooDeep`, so an agent that keeps re-asking the same question cannot loop forever.

**Caller and operator surfaces.** See `10` §2 for `await_agent`/`get_changes` on a retired
run, and `04` §6 for the `bridge answer`/`bridge runs` CLI surface.

### 2.4 Event model

Vendor events are normalised so the TUI and the digest are vendor-independent:

```
run.started · agent.message · agent.thinking · tool.call · tool.result
file.changed · shell.command · usage · question.asked · steer.injected
run.finished · run.failed
```

Unrecognised vendor events are preserved verbatim under `raw` but never rendered as
instructions.

### 2.5 Sanitizer

Applied at exactly one place: everything crossing the boundary back into A's context, and
everything rendered in the TUI.

```
strip ANSI/C0/C1 → neutralise system/tool-call markup → cap bytes →
wrap in <untrusted_agent_output source="codex" run="…"> … </untrusted_agent_output>
```

The envelope text states plainly that the content is data produced by another agent and
must not be followed as instructions.

### 2.6 Audit writer

Append-only JSONL under the user state directory. Metadata and SHA-256 digests only,
unless `--audit-bodies`. Fsync on run completion. Never contains environment values.

### 2.6a Gate

The Go package chain places `internal/gate` between audit and control:

```
config → adapter → policy → run → stream → sanitize → audit → gate → control → tui
```

No package depends on one to its right.

The gate is the delegated agent's only address: a per-run unix socket carrying one thing —
a blocking question forwarded from B, and the verdict that unblocks it. It is **not** the
operator's control socket and never becomes one: its verbs cannot list runs, change a
feature toggle, or reach another run.

**Double gate.** A run gets a gate only when both halves hold: the adapter declares
`capabilities.interactive: true` *and* carries an `interactive:` flag list for the run's
mode (`interactiveEnabled` in `cmd/bridge/tools.go`), *and* the operator has turned on
`features.interactive`, which defaults to false. Either half missing means no gate server
is started, no mcp-config is written to disk, and no gate flags reach the child's argv —
the run proceeds exactly as a non-interactive one.

**Per-run isolation.** When the gate is armed, `buildSpec` mints a fresh token, starts a
per-run unix socket (`gate/gate-<run_id>.sock`, in its own subdirectory of the runtime
directory — never a sibling of `servers.json`, the index listing every live bridge's
operator control socket) and writes a per-run mcp-config (`gate-<run_id>.json`, mode
`0600`) into the *state* directory — never inside the worktree, which is the delegated
agent's own writable workspace and not a place to leave a secret.

The gate subdirectory exists because the child is handed its own gate socket path
directly, in its mcp-config's `env` map (below). A gate socket living alongside
`servers.json` would hand that same child a pointer into the directory that lists every
other live bridge's control socket — a directory it has no reason to ever see. Moving the
gate socket into its own subdirectory removes that pointer: the child receives exactly
one address and nothing to discover another with. This is defence in depth, not a new
trust boundary — same-UID is still not a privilege boundary (docs/03 T10a); a same-UID
process can still find `servers.json` by other means. It closes off one incidental path to
it, no more.

**How the child learns the address.** The socket path, token and run id reach the child
only through that mcp-config's own per-server `env` map (`bridge_gate` → `env` in
`writeGateConfig`), which the vendor CLI applies to the one MCP server it spawns from that
file. They are never placed in the child's inherited process environment, and the child is
never told where the operator's own control socket lives. The precise invariant on the
child's process environment (`internal/adapter/env.go`) is: no runtime directory, no run
id, no gate secrets; inherited `AGENTS_BRIDGE_*` variables are stripped, and the bridge
deliberately sets one of its own — `AGENTS_BRIDGE_DEPTH`, a best-effort recursion marker,
never a control (docs/12 S6). The gate address is a further, narrow exception on top of
that: it is scoped to a single per-server env map inside a file that names nothing else,
carries a token good for this run only, and grants reach to the gate socket alone — never
to the control socket, another run's gate, or any operator verb.

**Lifecycle.** The gate is closed and its mcp-config removed (`closeGate`) when the run
reaches a terminal state (`recordCompletion`), when a run settles into `needs_input`
(`await_agent`'s `StateNeedsInput` branch calls `closeGate` before returning: the child is
already gone once a run reaches `needs_input`, so its gate has nothing left to serve, and
`needs_input` is not terminal — it stays visible to the operator (`bridge runs`) rather than
pruned like a finished run, but there is no resume path — so this is a fourth, distinct
teardown path, not a special case of `recordCompletion`), when `buildSpec` starts a gate
but then fails before the run is spawned — a `defer` in `buildSpec` tracks `gateStarted`
against a `succeeded` flag so a half-built run cannot leave a gate behind — and when the
server itself shuts down (`closeAllGates`, deferred in `serve.go` alongside worktree and
run cleanup). `closeGate` is idempotent, so a run that closes its gate on entering
`needs_input` and later reaches a terminal state does not double-close. A gate socket or
mcp-config file surviving past any of these points is a fault, not a leak that resolves
itself.

**Fail closed.** If `startGate` cannot start the socket or write the mcp-config,
`buildSpec` does not fall back to running the adapter non-interactively — it returns an
error and the run does not proceed at all.

**Startup sweep.** A crash skips both `recordCompletion` and the shutdown `defer`, so a
`gate-<run_id>.json` from a killed process can outlive it. `sweepStaleGateConfigs` runs
once at startup, before any run of the new process can start its own gate, and removes
`gate-*.json` files from the state directory — but only files it can prove are orphaned.
The state directory is shared by every bridge instance on the machine, so a file found
there is not necessarily this process's own: a second bridge live right now has its own
`gate-*.json` sitting alongside it, and deleting that file would break that bridge's child
mid-run. Ownership is established, not assumed, and the check dials only a path this
process computed itself: `gateConfigLive` takes the run id from the trusted filename and
derives the ONE socket path a legitimate gate for that run could ever be listening on
(inside the gate socket subdirectory, §2.6 above), then dials only that path — never the
socket path recorded inside the file. The file's own socket field is read solely to check
it agrees; any mismatch, along with an unparseable, unreadable, or out-of-tree entry, means
ownership cannot be proven, so the file is left alone — never dialed, and never deleted. A
confined write run's worktree lives inside this same state directory, so a delegated agent
could otherwise plant a `gate-*.json` naming an arbitrary unix socket and make the sweep
dial it as a liveness oracle; computing the path itself instead of trusting the file closes
that off. Only a dial to the process's own computed path that actively refuses the
connection is treated as proof of an orphan. This is best-effort and fails toward **not
deleting**: unproven ownership never deletes, and a live run broken by an over-eager sweep
is worse than a harmless stale file left on disk.

### 2.7 Control channel

One interface, two implementations selected at build time:

| | Linux / macOS | Windows |
|---|---|---|
| Endpoint | `$XDG_RUNTIME_DIR/agents-bridge/<pid>.sock` | `\\.\pipe\agents-bridge-<user-sid>-<pid>` |
| Access control | mode `0600` in a `0700` directory | explicit DACL: current user SID only, `FILE_FLAG_FIRST_PIPE_INSTANCE` |
| Peer check | `SO_PEERCRED` / `LOCAL_PEERCRED`, UID equality | client process token SID equality |

**This is not an operator boundary, and the docs must not call it one.** Agent B runs as
the same UID, so a same-UID peer check authenticates the Unix account, not the human.
Rejecting peers whose PID lies inside a run's process group or job is worthwhile defence
in depth, but B can enlist another same-UID process to defeat it. See
[`03`](03-threat-model.md) T10.

Both are backed by an index file listing live servers so `bridge watch` with no arguments
can offer a picker. A failed peer check is refused and logged.

**Runtime directory resolution.** `internal/platform.NewPaths` (POSIX) resolves the runtime
directory in this order: `$XDG_RUNTIME_DIR/agents-bridge` when `XDG_RUNTIME_DIR` is set,
otherwise `<base>/agents-bridge-<uid>`, where `<base>` is `os.TempDir()` except on darwin,
where it is `/tmp`. A unix socket path has a hard 104-byte `sun_path` limit (`endpoint_posix.go`,
`sunPathMax`), and macOS leaves `XDG_RUNTIME_DIR` unset, so without the darwin case the fallback
base would be `TMPDIR`, a long per-process `/var/folders/<random>/T` path (~45-50 bytes) that
left too little headroom for a gate socket path once `gate/gate-<run_id>.sock` was appended —
CI observed one at 128 bytes, past the limit. `/tmp` is short and, unlike `XDG_RUNTIME_DIR`, not
already per-user on darwin, so the `<uid>` suffix and the `0700` mode on the created directory
are what give the fallback its per-user isolation. This darwin branch is reasoned from source and
CI's Linux-observed failure, not yet exercised by a macOS CI run.

Protocol: newline-delimited JSON. Verbs: `subscribe <run_id>`, `steer`, `answer`,
`toggle`, `status`, `stop`. All operator-side; none of them are reachable from MCP.

## 3. Tool surface

| Tool | Purpose |
|---|---|
| `ask_<agent>` | Start a run. Returns `run_id`, opaque `session_handle`, watch hint. Never a filesystem path. |
| `await_agent` | Bounded wait; returns result or progress digest, in both the text content and `structuredContent` (FR-13). `vendor_errors` counts the error events the run's stream carried, including on a clean exit; it is a count, not a failure verdict (FR-14). Re-callable. |
| `steer_agent` | Inject guidance into a live run (counted, capped). |
| `cancel_agent` | Kill the run's process group. |
| `list_runs` | Runs owned by this server. A `superseded` run reports its successor's id (`superseded_by`). |
| `get_changes` | Confined run (`worktree: required`): the sanitized diff, changed-file list and per-file digests. Unconfined run (`worktree: off`): `no_diff_unconfined` plus the run's cwd and the files B touched, taken from the event stream — there is no git state to diff. |
| `accept_changes` | Confined run: apply the diff to the caller's tree with `git apply --3way`, refusing on conflict. Unconfined run: `not_applicable_unconfined` — the changes are already in the tree. Present only when `agent_acceptance` is enabled; off by default, in which case only the operator's `bridge accept` can apply. |

`ask_<agent>` parameters: `prompt`, optional `cwd`, optional `session_handle`, optional
`model`, optional `sandbox` (narrowing only — a write-enabled adapter can be asked for a
read-only run; the reverse is refused). Nothing that can widen policy.

`cwd` is not optional in effect: when omitted it defaults to the **first allowed root**,
never to the bridge's inherited working directory, which is the host's cwd and may lie
outside every root.

Session handles are opaque and bridge-issued. A vendor session id is never accepted from
the caller and never returned to it: `codex exec resume --last` is a valid vendor id that
would resume the operator's most recent interactive session, and any real id read from
`~/.codex/sessions` or `~/.claude` would leak private history into A.

## 4. Two reference adapters (verified locally)

### Claude Code — `claude 2.1.270`

| Need | Mechanism |
|---|---|
| headless | `-p/--print` |
| event stream | `--output-format stream-json --verbose`, `--include-partial-messages` |
| steering | `--input-format stream-json` — user messages written to stdin mid-run |
| steer ack | `--replay-user-messages` |
| session | `--session-id <uuid>`, `--resume`, `--fork-session` |
| sandbox | `--permission-mode plan\|manual\|acceptEdits`, `--restricted`, `--tools`, `--allowed-tools`, `--disallowed-tools` |
| unattended denial | `--permission-prompts none` |
| workspace | `--add-dir` |
| cost cap | `--max-budget-usd` |
| structured result | `--output-format json`, `--json-schema` |
| MCP isolation | `--strict-mcp-config` (prevents the bridge from re-entering via B's own MCP config) |

### Codex — `codex-cli 0.154.0`

| Need | Mechanism |
|---|---|
| headless | `codex exec` |
| event stream | `--json` (JSONL events on stdout) |
| steering | `codex queue --thread <id> --message <text>` |
| session | `codex exec resume <id>`, `codex exec fork` |
| sandbox | `-s read-only\|workspace-write\|danger-full-access` |
| workspace | `-C/--cd`, `--add-dir` |
| config isolation | `--ignore-user-config`, `--ignore-rules`, `--strict-config` |
| structured result | `--output-schema <file>`, `-o/--output-last-message <file>` |
| ephemeral | `--ephemeral` |

Codex exposes a richer session model than the bridge uses (`app-server`, `agents`,
`remote-control`). v1 deliberately uses only `exec` + `queue`, which are stable public
CLI surfaces.

**The cost of that choice, stated plainly:** `codex exec` is non-interactive and runs
with approval policy `never`. A blocked action is denied and the denial is returned to
the model; there is no correlated approval request to surface, so **interactive mode is
unavailable for Codex in v1** and the Codex adapter declares `interactive: false`. The
correlated request/response protocol exists in Codex's app-server; adopting it is a
post-1.0 decision that would reverse the stable-surface rule above, and it is the only
way to get a Codex approval UI.

## 4a. Platform abstraction

Exactly four interfaces carry every OS difference, plus `DialControl`, the client half of
`ControlEndpoint` (a unix socket on POSIX, a named pipe whose server SID the client verifies on
Windows). Nothing else in the codebase is platform-aware. Their POSIX implementations are
tested on Linux and macOS in CI; the Windows ones execute there too — CI's unit job runs
`go test -race ./...` on `windows-latest`, so the build-tagged Windows tests do run. Windows
behaviour that no test covers is declared, not enforced, and Windows stays unsupported until
v1.1:

```go
type ControlEndpoint interface { Listen(name string) (net.Listener, error); VerifyPeer(net.Conn) error }
type ProcessGroup    interface { Attach(*exec.Cmd) error; KillAll() error }
// Optional, implemented by ProcessGroup where the OS needs two-phase binding:
type PostStarter     interface { AfterStart() error }
type Paths           interface { Config() string; State() string; Runtime() string }
type PathGuard       interface { Canonicalise(string) (string, error); Contains(root, p string) bool }
```

## 5. Degradation matrix

| Adapter declares | Behaviour |
|---|---|
| `stream: false` | Transcript records start/end/exit + buffered output; TUI shows a spinner and the final result. |
| `resume: false` | `session_id` parameter is omitted from the tool schema. |
| `steer: false` | `steer_agent` returns `unsupported_capability`; TUI hides the input box. |
| `interactive: false` | B's questions are treated as `needs_input` failures immediately. |
| `sandbox_enforced: false` | The adapter has no vendor sandbox flags for that mode. The run proceeds **unsandboxed**, and that word appears in the tool description, in `bridge status`, and in every audit record for the run. Path confinement still applies; nothing else does. |

Every degradation is visible in `bridge status` and in the tool description. The bridge
never silently pretends a capability exists.
