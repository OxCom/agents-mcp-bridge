# MCP contract

How the bridge behaves as an MCP server: what each tool returns, when progress and
elicitation are legal, and what the host is and is not expected to do. This document
exists because the asynchronous design (a run outlives the call that started it) does not
fit the protocol's request-scoped features without saying exactly how.

## 1. The asynchronous problem

`ask_<agent>` returns immediately with a handle. B then runs for minutes. Two MCP features
are request-scoped and therefore **cannot** be used from outside a live request:

- **`notifications/progress`** requires a `progressToken` supplied by the client *on a
  specific request*. There is no valid token once that request has been answered.
- **`elicitation/create`** is delivered to the user in the context of an in-flight
  request. A server-initiated prompt with no request open has nowhere to render.

The bridge therefore treats `await_agent` as the request that owns both.

```
ask_codex ──► run starts, returns {run_id, session_handle}      (no progress, no elicitation)
                    │
await_agent ──► THE request that owns progress + elicitation    (bounded slice, re-callable)
                    │
                    ├─ B asks something        ─► TUI first, only if a watcher is attached
                    │                              to THIS run; elicitation only if this
                    │                              call is live
                    ├─ progress notifications   ─► only while this call is live, only if a token was sent
                    └─ returns result | still_running | needs_input
```

## 2. Tool contracts

### `ask_<agent>`

| | |
|---|---|
| Params | `prompt` (required), `cwd`, `session_handle`, `model`, `sandbox` (narrowing only) |
| Returns | `run_id`, `session_handle`, `watch_hint`, `confinement` (`worktree` \| `none`), `sandbox_enforced` |
| Never returns | a filesystem path, a vendor session id, B's output |
| Errors | `root_violation`, `depth_exceeded`, `rate_limited`, `concurrency_limit`, `capability_disabled`, `prompt_too_large`, `unknown_handle` |

`cwd` omitted defaults to the first allowed root, never to the inherited working directory.

### `await_agent`

| | |
|---|---|
| Params | `run_id` (required), `timeout_s` (default 60, max 600) |
| Returns | one of `completed` (with the enveloped result), `still_running` (with a progress digest), `needs_input` (with B's sanitized question), `superseded` (the run was continued; carries the successor's run id), `failed` (with reason), `cancelled` |
| Re-callable | yes, any number of times, until `run_retention_s` after the run ends |

A `needs_input` result (`cmd/bridge/tools.go`, `askOutput`) carries B's question in exactly
one place: enveloped in the tool result text, the codebase's one provenance-marked channel
for untrusted vendor output. Structured output carries no `question` field — a second,
unmarked copy of the same text outside the envelope would be a channel with no provenance
marking, so it was removed. Structured output does carry the run id, the state, and a
`session_handle` that identifies the stopped run. The run's child process is killed and its
pending question cleared as part of the transition into `needs_input`; **no VENDOR resume
path exists** — nothing takes a run id or `session_handle` and reaches the original blocked
tool call. `needs_input` rests until `question_timeout_s`, then becomes `failed` (docs/11 §3
invariant 3), unless the operator continues it first.

**A run id (the `session_handle` returned here) is exactly what an operator uses to trigger
a continuation** — `bridge answer <run_id> "<text>"` (docs/04 §6) or the TUI's question panel
— which is not a resume: it starts a new run seeded with reconstructed context (docs/02
§2.3a) rather than reaching back into the original blocked call. This is operator-only and
unreachable from MCP: `registerTools` never exposes it as a tool, so the calling agent can
only observe the outcome, via this same `await_agent` call, once someone else has acted. If
the operator continues the run while a caller is polling `await_agent`, a later call returns
state `superseded` with the successor's id (`out.SupersededBy`) instead of falling into the
ordinary completion path — a caller that keeps polling learns where its work went. The handle
**is the bridge-issued run id, not a vendor session id**: handing the caller a vendor
identifier would be a session-takeover primitive (docs/11 §2), which is why every vendor
session id — Codex thread id, Claude session id — stays private to the bridge. When
`ask_<agent>` grows a real bridge-issued session handle distinct from the run id, that
handle is what appears here instead.

This is the only place progress notifications and elicitation are legal. A `still_running`
result is not an error and does not mean anything is wrong; the model is expected to call
again. **The tool description says so explicitly**, because nothing in the protocol forces
a model to await a run it started.

### Unawaited runs

A run nobody awaits is a real failure mode: the calling model fires and forgets, and the
bridge burns tokens for a result no one reads. Three mitigations, all in v1:

1. `ask_<agent>`'s description states that a run must be awaited or cancelled.
2. Any subsequent tool call on this server returns a one-line reminder naming runs that
   have finished and not been collected.
3. A run whose `timeout_s` expires with no `await_agent` call ever made is cancelled, its
   process group killed, and the abandonment recorded in the audit log.

### `steer_agent`, `cancel_agent`, `list_runs`, `get_changes`, `accept_changes`

`steer_agent` is present only when `features.agent_steering` is enabled (off by default)
and the adapter declares `steer`. It returns `question_pending` and refuses while a run has
an unanswered question — answering B's question is the operator's channel, not A's, and
allowing it here would be automatic answering by the back door.

`get_changes` / `accept_changes` exist only for write-mode adapters, and return
`no_diff_unconfined` / `not_applicable_unconfined` for a `worktree: off` run. `get_changes`
on a `superseded` run returns a typed error naming the successor instead of a stale or empty
diff — the predecessor's change-store entry was retired when the continuation started, and
exactly one acceptable diff exists per chain, now on the successor (docs/02 §2.3a).

## 3. Three distinct interactions, never conflated

Question and permission approval are still two different things — B is asking a different
kind of thing, and the bridge answers them differently — but Spike C7 (`docs/12`)
falsified the assumption that they arrive on separate surfaces. For `claude -p`, both
arrive on **one channel**: an MCP `tools/call` on the gate server the bridge supplies to
the child (`--permission-prompt-tool mcp__<srv>__<tool> --permission-prompts host`),
distinguished only by `tool_name`. `tool_name == "AskUserQuestion"` is a human question;
every other value is a permission approval bound to a specific tool/file/command decision.

The bridge answers a question (TUI, only when a watcher is attached to this specific run →
elicitation → fail-closed, per §4) and **denies**
every permission approval with a constant message, via `behavior: "deny"` — the only
channel C7 found that the child treats as an answer rather than silence — and audits the
denial. A denied approval is never surfaced to a human: v1 does not offer a UI for
approving B's tool calls, only for answering B's questions.

"Attached" is a per-run signal, not a server-wide one: `bridge watch <run_id>` resolves
exactly one run before it ever attaches, then sends one `VerbAttach` naming that run's id
for the life of the watch process (`cmd/bridge/watch.go`'s `attachToRun`), detaching on
every exit path. A watcher on a different run does not count, and neither does a bare
attach naming no run — nothing in this codebase sends one. So the TUI channel exists for a
given run only while an operator is actively running `bridge watch` against that exact run
id; otherwise the question falls straight through to elicitation, then to `needs_input`.

| | What it is | Vendor surface needed | v1 support |
|---|---|---|---|
| **Question** | B asks the human something and blocks | a `tools/call` on the bridge's gate server, `tool_name == "AskUserQuestion"` | Claude Code only |
| **Permission approval** | B blocks on a specific tool/file/command decision bound to a thread and turn | the same `tools/call` channel as Question, any other `tool_name` | Claude Code only — always denied with a constant, audited message; never surfaced to a human |
| **Steering** | unsolicited guidance injected into a running conversation | an injection channel | Codex (`queue`), Claude (queued at turn boundary) |

An adapter declaring `steer: true` makes **no** claim about questions or approvals. The
capability flags are independent and are checked independently.

## 4. What the host is expected to do — and what it cannot be relied on for

**Relied on:** delivering tool results to the model as content; honouring
`elicitation/create` during an in-flight request if it advertises the capability;
supplying a `progressToken` if it wants progress.

**Not relied on:** rendering the untrusted-data envelope specially, preserving it
verbatim, or preventing the model from acting on enclosed text. The envelope is a
statement of provenance, not an enforcement mechanism — if the host strips it, the
protection is gone, and the threat model already treats prompt injection as residual risk.

**Degradation, never failure:** a host that sends no `progressToken` gets no progress and
loses nothing else (Claude Code is such a host today). A host without elicitation gets the
TUI path, and if no watcher is attached to this run the question falls through to
elicitation and then, if that is unavailable or unanswered, to `needs_input`. Neither the
TUI wait nor the elicitation wait is open-ended: both are bounded by `question_timeout_s`,
so a host that accepts `elicitation/create` and never answers cannot block the run past
that timeout.

**The elicitation message carries attribution, not just the raw question.** `q.Text` is
untrusted vendor output — already run through the sanitizer's `clean` function, which
strips control characters but not meaning, so a hostile question could still contain text
engineered to look like a heading of its own. `ElicitParams.Message` is built by
`cmd/bridge/tools.go`'s `elicitMessage(agentID, runID, text)`, which puts a fixed,
bridge-authored preamble naming the source agent and run id first, then a marker, then the
quoted question — so the message never renders inside host A's UI as though the bridge
itself were asking: the prefix always occupies that position, and a question crafted to
mimic it can only ever appear after the true one.

**Elicitation is unreachable above protocol version `2026-07-28`.** The bridge calls the
SDK's `Elicit` directly, which is a server-initiated JSON-RPC request. Verified in the
`go-sdk` v1.8.0 source (`mcp/server.go:1613-1628`,
`assertServerInitiatedRequestAllowed`): on any session negotiated at protocol version
`>= 2026-07-28`, the SDK refuses that call outright. SEP-2322 / SEP-2575 require
elicitation (and sampling, and roots) to be embedded as `InputRequests` in an
`InputRequiredResult` returned from a multi round-trip handler (`tools/call`,
`prompts/get`, `resources/read`) instead of sent as a standalone request. Against a host
negotiated on that version or later, `Elicit` returns an error before it reaches the
client, and the TUI/elicitation fallback in the diagram above collapses straight to
`needs_input` — this is the same fail-closed path a host with no TUI takes, never a wrong
answer.

This works today, and is now verified rather than deduced: the C7 spike observed Claude
Code 2.1.272 negotiating protocol version `2025-11-25`, below the cutoff, and the C9 spike
(`docs/12`) confirmed directly that this host cannot reach `2026-07-28` from either side of
the handshake. Against a real go-sdk v1.8.0 server, its `initialize` request asked for
`2025-11-25` and negotiated exactly that. Against a hand-rolled server (bypassing the
go-sdk) that answered `initialize` claiming `protocolVersion: "2026-07-28"` regardless of
what the client requested, Claude Code 2.1.272 **rejected the connection outright** —
`Server's protocol version is not supported: 2026-07-28` — and sent no further JSON-RPC
message at all, so no `InputRequests`/`InputRequiredResult` result could ever be shown to
it even by a server willing to send one. The go-sdk's own client-facing handshake caps
below `2026-07-28` by design for the same reason, independent of what any particular
vendor CLI does. The limitation still arms exactly as described above whenever a host
*does* negotiate the newer protocol — C9 verifies only that today's pinned Claude Code
build is not such a host, not that no host ever will be. A host that negotiates
`2026-07-28` or later will not offer the elicitation fallback at all until `await_agent`
is redesigned around `InputRequests` (blocked on host support per C9) — that redesign
does not exist yet.

## 5. Protocol version

Target the current MCP specification. Elicitation is negotiated per client and used only
when the client advertises it, and only below protocol version `2026-07-28` — see §4.
Sampling is not used: it is deprecated as of the `2026-07-28` specification, and a bridge
that asked the host's model to answer B's questions would be automatic answering —
explicitly out of scope.
