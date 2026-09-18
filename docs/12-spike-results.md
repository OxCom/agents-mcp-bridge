# Vendor behaviour spikes

Run 2026-09-14 against the pinned local binaries: **codex-cli 0.154.0** and
**claude 2.1.270**, in throwaway directories. Every line below is observed behaviour, not
documentation. Where a spike contradicted an assumption this specification had made, the
specification was changed — the list of those is §3.

A second run on 2026-09-15 against **claude 2.1.272** added C7 and C8. C7 is the
interactive-mode spike; it is the one that changed an architecture decision rather than a
flag.

A fourth round on 2026-09-16, same **claude 2.1.272**, added C10 and C11. Both were found by the
gated conformance suite rather than by a standalone spike: each was a bridge defect the vendor
reported only to the model, never to the bridge, so the run looked like a child that chose not
to ask. Their fixes are what made `conformance` pass end to end for the first time.

A third spike, also on 2026-09-15 against the same **claude 2.1.272**, added C9. It used two
throwaway stdio MCP servers outside this repo (one a real go-sdk v1.8.0 server, one a
hand-rolled JSON-RPC responder with no SDK dependency) to test SEP-2322 multi round-trip
reachability directly, verified twice via `claude -p` with the wire traffic teed to a log.
Total cost: $0.0589, two `claude -p` calls on `claude-haiku-4-5-20251001`.

Raw output was kept per run. Re-run these before bumping either pinned version; that is
what Phase 6.4's CI smoke tests automate.

## 1. Codex — codex-cli 0.154.0

| # | Question | Result |
|---|---|---|
| S1 | Event stream shape | Types in order: `thread.started`, `turn.started`, `item.completed`×N, `turn.completed`. Thread id at **`.thread_id` on `thread.started`**, top level, not nested. **The final assistant message is not a distinct event type** — it is `item.completed` with `.item.type == "agent_message"` and text at `.item.text`. Non-fatal warnings arrive the same way with `item.type == "error"`. Usage on `turn.completed.usage`. |
| S2 | Does `exec resume` inherit the sandbox? | **No.** Resumed with no sandbox setting, the run **created a file**. With `-c sandbox_mode="read-only"` the write was refused. Separately: **`exec resume` accepts neither `-C/--cd` nor `--sandbox`** — `-C` is a parse error and `--sandbox` is absent from its help. |
| S3 | Can `codex queue` steer a turn in flight? | **No.** The command *succeeds* against a CLI-owned thread (exit 0, no daemon complaint), but the word never appeared in the running session's output. Resuming the thread afterwards with "ok" produced `banana`. It is a durable next-turn mailbox. A one-shot `codex exec` exits without ever consuming it. |
| S4 | Is the `approvals_reviewer` trap real? | **Yes.** With `-c approvals_reviewer="auto_review"` and `--sandbox read-only`, the file **was created**. Control (read-only alone): refused. Adding `-c approval_policy="never"`: refused. A project-level `./.codex/config.toml` with the same setting had no effect at the path tested — project-config discovery remains **NOT VERIFIED**. |
| S5 | Does `--ignore-user-config` break auth? | **No.** Authenticated and answered. Only `config.toml` is skipped; agent-role and skill files still loaded. Help confirms auth still uses `CODEX_HOME`. |
| S6 | Does the environment reach child MCP servers? | **No.** A spawned MCP server received exactly 12 variables (`HOME LANG LC_ALL LOGNAME NODE_EXTRA_CA_CERTS PATH PWD SHELL SHLVL TERM USER _`); the marker was absent. `shell_environment_policy.inherit=all` changed nothing — it governs shell commands, not MCP children. The only channel is the per-server `mcp_servers.<name>.env` map. |
| S7 | Prompt on stdin | `codex exec … -` works on both `exec` and `exec resume`. `--` is not needed. The prompt never appears in argv. |
| S8 | How does codex report a refused turn? | **Two events the S1 shape does not cover**, observed live on 2026-09-17 under a usage limit: a **top-level** `{"type":"error","message":"..."}` — not an `item.completed` error item — followed by `{"type":"turn.failed","error":{"message":"..."}}`, then exit 1 with **empty stderr**. A parser that maps both to the unknown kind leaves the caller `the agent exited with status 1: ` and no reason. Both are parsed since; `turn.failed` becomes `run.failed` and supplies the failure line when stderr is empty. |

## 2. Claude Code — claude 2.1.270

| # | Question | Result |
|---|---|---|
| C1 | Input record shape, and does it exit? | `{"type":"user","message":{"role":"user","content":"<string>"}}` is accepted as-is. **The process does not exit when the turn completes** — it emitted the result at ~7s and exited only when stdin closed at 121s. Turn completion is the `result` record; process exit is not a signal. |
| C2 | Injection: interrupt or queue? | **Queued at the turn boundary.** Injected at T+1s into a 6.35s turn, the first answer completed with `stop_reason: "end_turn"`, *then* the injected message was answered, same `session_id`. No interruption at any timing tried. |
| C3 | What stops a hostile repo's hooks? | A `.claude/settings.json` `SessionStart` hook fired **with no isolation** and fired **with `--strict-mcp-config`**. It did **not** fire with `--setting-sources ""`. That flag is the control — and it is all-or-nothing: the operator's own user-level settings went with it. |
| C4 | Is plan mode usable read-only? | **Yes.** `--permission-mode plan` returned a real answer and read the file; it did not return a plan or refuse. Gotcha: `--tools` is variadic and swallows a trailing positional prompt. |
| C5 | `--tools` vs `--allowed-tools` | `--tools Read,Glob,Grep` → init lists exactly those three, no file created. `--allowed-tools Read` → init lists **29 tools including `Bash`, `Write`, `Edit`**, and the file **was created**. `--allowed-tools` is a permission hint, not a tool roster. |
| C7 | Where does a blocking question surface in `-p` mode? | **Not in the event stream at all.** With the child held for 4 s, stdout emitted nothing; the `tool_result` record appeared 15 ms after the bridge answered. The question arrives instead as an **MCP `tools/call` on a server the parent supplies to the child** (`--permission-prompt-tool mcp__<srv>__<tool> --permission-prompts host`), payload `{tool_name, input, tool_use_id}` with `_meta{"claudecode/toolUseId", progressToken}`. One channel carries both kinds: a human question is `tool_name == "AskUserQuestion"`, anything else is a permission approval. **`behavior: "allow"` does not answer a question** — the child received `"The user did not answer the questions."`. **`behavior: "deny"` with `message` is the answer channel**: the operator's text arrives as the tool result, marked `is_error: true`, and the model acts on it (it chose `blue.txt` from `"The user answered: use blue.txt"`). Claude Code as an MCP client advertises `elicitation: {}` and `roots` on initialize. |
| C6 | Session id semantics | `session_id` appears top level on **every** record. `--session-id` names a new session and exits 1 with "already in use" when reused; `--resume` is the only way back, and context was retained. |
| C8 | `--permission-prompts` passed twice with different values — which wins? | **The last occurrence wins.** Invocation: `claude -p '<prompt that writes a file>' --output-format stream-json --verbose --mcp-config <cfg> --permission-prompt-tool mcp__gate__approve --permission-prompts none --permission-prompts host --setting-sources ""`. The permission-prompt tool **received** the `tools/call` and the file was created — `host` (the later flag) governed, not `none` (the earlier one). An adapter's fail-closed sandbox list can therefore keep `--permission-prompts none`, and an interactive flag list emitted after it can override to `host`: the two lists compose by order, with no ambiguity. |
| C9 | Does Claude Code's MCP client implement SEP-2322 multi round-trip (protocol `>= 2026-07-28`) at all? | **No — it cannot reach that protocol version, from either direction.** Against a real go-sdk v1.8.0 server, the client's `initialize` requested `protocolVersion: "2025-11-25"` and negotiated exactly that; the go-sdk's own handshake caps below `2026-07-28` by design (`shared.go`'s `negotiatedVersion`), so an MRTR-shaped `InputRequests` result never reaches the wire — the SDK downgrades it to a legacy `elicitation/create` request first. Against a hand-rolled server bypassing the go-sdk entirely, answering `initialize` with `protocolVersion: "2026-07-28"` regardless of what the client requested, Claude Code **rejected the connection outright**: no further JSON-RPC message was sent — no `notifications/initialized`, no `tools/list`, no `tools/call` — and the assistant turn reported verbatim: `Server's protocol version is not supported: 2026-07-28`. **Consequence: SEP-2322 multi round-trip requests are unreachable against this host in either role**, so the `InputRequests`/`InputRequiredResult` flow cannot be exercised by it at all. Side observation from the same run: the go-sdk's legacy elicitation shim **did** successfully round-trip a plain `elicitation/create` request to Claude Code's client, which validated it against a real schema requiring either `form` mode with an object `requestedSchema`, or `url` mode with `elicitationId` + `url` — a `url` elicitation mode not documented anywhere else in this repo. Verified twice (both runs independently reproduced). Cost: $0.0589, two `claude -p` calls on `claude-haiku-4-5-20251001`. |
| C10 | What shape must a `--permission-prompt-tool` result take? | **Exactly one text block, and nothing else.** A result carrying a text block *plus* `structuredContent` — which the go-sdk's generic `mcp.AddTool` attaches whenever the handler declares a typed output value, along with an `outputSchema` — is refused verbatim: `Error calling tool (AskUserQuestion): Permission prompt tool returned an invalid result. Expected a single text block param with type="text" and a string text value.` The model then retried the same question, and the retry blocked on a gate that had already answered the first one until the client's own MCP timeout aborted it. Registering the tool through the untyped `Server.AddTool` (no output schema, no structured content) is what works. |
| C11 | Does the client validate a `tools/call` against the server's advertised input schema before sending it? | **Yes, and it never sends a call that fails.** The gate declared `input` as `{"items":{"maximum":255,"minimum":0,"type":"integer"},"type":["null","array"]}` — schema inference from a Go `json.RawMessage`, i.e. `[]byte` — while the permission payload's `input` is the delegated tool's arguments *object*. The child reported "the tool is having trouble with the input validation", retried with different argument formatting, and gave up asking in prose. Nothing reached the gate, so the bridge saw a run that simply completed. An input schema that misdescribes the payload is therefore indistinguishable, from the bridge's side, from a model that chose not to ask. |

## 3. Assumptions this specification got wrong

| Was | Is | Changed in |
|---|---|---|
| Codex supports mid-run steering via `codex queue` | `queue` is next-turn store-and-forward; it cannot reach a turn in flight, and a one-shot `exec` never consumes it. **Codex is `steer: false`** | `04`, `07`, `11` |
| One sandbox flag list serves `exec` and `exec resume` | `exec resume` rejects `-C` and `--sandbox`; a second `resume_sandbox` list using `-c sandbox_mode` is required | `04`, schema rule 15 |
| `-c approvals_reviewer="user"` is sufficient for read-only | It only routes escalation. `-c approval_policy="never"` is the governing control | `04`, `07`, SECURITY.md |
| A depth marker in B's environment bounds recursion | Codex does not propagate its environment to MCP children. Demoted to best-effort; vendor config isolation is the enforced control | `01` SR-6, `03` T6 |
| `--strict-mcp-config` isolates Claude from hostile repo config | It scopes MCP servers only. Hooks still fire. `--setting-sources ""` is the control | `04`, `07`, `03` T5 |
| `--permission-mode plan` forces a plan-shaped reply, so it is unusable read-only | It answers normally. The Wave-1 note claiming otherwise was wrong | `04`, `07` |
| A finished `claude -p` process exits | It blocks on stdin EOF. The bridge must close stdin, and must not treat exit as turn completion | `04`, `02` |
| Claude steering is `steer: true` | Queued at the turn boundary: `steer: queued` | `04`, `07` |
| B's questions arrive as events in the vendor stream, so a `stream.Parser` can detect them and emit `question.asked` | Nothing reaches stdout while the child blocks. Detection requires the bridge to run a second MCP server facing the child and receive a `tools/call`. Interactive mode is therefore a *transport* the adapter declares, not a parser feature | `01` FR-4, `02`, `08` 3.1, `10`, `11` |
| A question and a permission approval are separate surfaces, so v1 can take questions and leave approvals out | They arrive on one channel and cannot be received separately. Approvals must at minimum be answered with an explicit policy (deny, or surface) the moment interactive mode is on | `10` §3 |
| Answering is a normal success response | A question is answered with `behavior: "deny"` plus the answer text; `allow` discards it. The answer reaches the model as an error-flagged tool result | `02`, `10` |
| A sandbox flag list and an interactive flag list both setting `--permission-prompts` is ambiguous and must be rejected as a load error | The vendor resolves it deterministically: the last occurrence wins ([C8](#2-claude-code--claude-21270)). Composition by order is well-defined, so that check was dropped — it also encoded a vendor flag string in Go, which this codebase forbids | `12` |

## 3a. Consequences found while building on these results

- **A one-shot run is barely steerable.** Because injection lands at the next turn
  boundary and `close_stdin_after: result` ends input when the first turn completes, the
  window to steer a short task can close before an operator reacts. Steering is a feature
  of multi-turn sessions, and the config reference now says so.
- **A vendor can fail a run without writing to stderr.** The failure line was built from
  the last stderr line alone, so codex's usage limit reached the caller as a bare exit
  status. The run now falls back to the reason the vendor stated on its own event stream
  (S8).
- **Claude's `result` record repeats the final assistant message.** Appending both hands
  the caller the same answer twice; the result text is used only when the agent produced
  no message of its own.

## 4. Still unverified

- Whether `--permission-prompts none` denies without ever calling the gate (documented as
  such, not yet observed).
- Whether a gate that never returns holds the child indefinitely, or whether Claude applies
  its own timeout to the permission call.
- Whether `--brief`'s `SendUserMessage` tool is a second, non-blocking agent-to-user
  channel, and whether it also passes through the permission gate.
- Codex project-level config discovery — whether *any* project file can set
  `approvals_reviewer`. The `./.codex/config.toml` path tested was not read.
- Claude's error text for a malformed stream-json input record (the tested shape was
  accepted, so no error was produced).
- Whether Claude can be interrupted mid-turn through some channel other than a plain user
  record (no control/interrupt record type was tried).
- Every capability row in [`07`](07-agent-capability-matrix.md) §2–3 for the seven
  non-reference agents. Those remain documentation-derived and must not be shipped as
  adapters until spiked the same way.
