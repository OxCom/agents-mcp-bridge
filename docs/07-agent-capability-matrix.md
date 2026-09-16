# Agent capability matrix

What each CLI agent offers a bridge that must drive it headlessly, stream its activity,
resume it, steer it mid-run, and sandbox it.

**VERIFIED (local)** = confirmed against the binary's own `--help` on this machine at the
stated version. **VERIFIED (docs)** = confirmed in official documentation or official
source. **UNVERIFIED / UNKNOWN** = not confirmed; no adapter ships on an unverified cell.
Only `claude` and `codex` are installed locally; every other row is documentation-derived.

Surveyed 2026-09-14.

---

## 1. Reference adapters (verified locally)

### Claude Code — `2.1.270` — VERIFIED (local)

| Need | Flag / mechanism |
|---|---|
| Headless | `-p`, `--print` |
| Event stream | `--output-format stream-json --verbose`; `--include-partial-messages`; `--include-hook-events`; `--forward-subagent-text` |
| Structured result | `--output-format json`; `--json-schema <schema>` |
| **Mid-run injection** | `--input-format stream-json` — user messages written to the child's stdin while it runs. **Queued at the turn boundary, not an interruption:** injected at T+1s into a 6.35s turn, the answer completed with `stop_reason: end_turn` before the injected message was handled. VERIFIED 2026-09-14. `--replay-user-messages` echoes for acknowledgement |
| Session | `--session-id <uuid>`, `-r/--resume`, `-c/--continue`, `--fork-session`, `--no-session-persistence` |
| Sandbox / permissions | `--permission-mode plan\|manual\|dontAsk\|acceptEdits\|auto\|bypassPermissions`; `--restricted`; `--tools`; `--allowed-tools`; `--disallowed-tools`; `--permission-prompts host\|none` |
| Workspace | `--add-dir <dirs...>` |
| Cost cap | `--max-budget-usd <amount>` |
| Config isolation | `--strict-mcp-config`, `--mcp-config`, `--settings`, `--setting-sources`, `--bare`, `--safe-mode` |
| Vendor background mode | `--bg`, `claude agents`, `attach`, `logs`, `stop`, `rm` |
| MCP client | yes — `claude mcp`, config `~/.claude.json` |
| Auth | own OAuth/keychain login, or `ANTHROPIC_API_KEY`; a subprocess inherits the login |

**Traps — VERIFIED empirically, 2026-09-14:**
1. `--allowed-tools Read` leaves **29 tools** in the init record including `Bash`, `Write`
   and `Edit`, and a file was actually created. `--tools Read,Glob,Grep` produced exactly
   those three and no file. Only `--tools` bounds the surface.
2. **`--strict-mcp-config` does not stop a hostile repo's hooks.** With a
   `.claude/settings.json` `SessionStart` hook present, the marker file was created both
   without isolation and with `--strict-mcp-config`. Only `--setting-sources ""` stopped
   it — and it is all-or-nothing, dropping the operator's own user settings too.
3. **The process does not exit when the turn ends**; it blocks until stdin EOF. Turn
   completion is the `result` record.
4. `--permission-mode plan` **does** return a real answer and read files — it is usable as
   a read-only mode. (This corrects an earlier assumption that it forces a plan-shaped
   reply.)
5. `--tools` is variadic and swallows a trailing positional prompt; deliver the prompt on
   stdin.

### Codex — `codex-cli 0.154.0` — VERIFIED (local)

| Need | Flag / mechanism |
|---|---|
| Headless | `codex exec` (alias `e`) |
| Event stream | `--json` (JSONL events on stdout) |
| Structured result | `--output-schema <file>`, `-o/--output-last-message <file>` |
| **Mid-run injection** | **None.** `codex queue --thread <id> --message=<text>` succeeds against a CLI thread (exit 0) but is store-and-forward: delivered on the thread's *next* turn, never mid-turn, and stranded entirely if the run was a one-shot `codex exec`. VERIFIED 2026-09-14 |
| Session | `codex exec resume <id>` / `--last`, `codex exec fork`, `--ephemeral`. **`exec resume` accepts neither `-C/--cd` nor `--sandbox`** — cwd comes from the process, sandbox from `-c sandbox_mode`. VERIFIED |
| Sandbox | `-s/--sandbox read-only\|workspace-write\|danger-full-access`; `--approve-for-me` |
| Workspace | `-C/--cd <dir>`, `--add-dir`, `--worktree`, `--skip-git-repo-check` |
| Config isolation | `--ignore-user-config`, `--ignore-rules`, `--strict-config`, `-c key=value`, `-p/--profile` |
| Vendor background mode | `codex agents`, `app-server`, `remote-control`, `exec-server` |
| MCP client | yes — `codex mcp`, config `~/.codex/config.toml` |
| Auth | `codex login` → `~/.codex/auth.json`, or `OPENAI_API_KEY`; inherited by a subprocess |

**Traps — all three now VERIFIED empirically, 2026-09-14:**
1. `-c approvals_reviewer="auto_review"` **does** defeat `--sandbox read-only`: the write
   succeeded. Adding `-c approval_policy="never"` closes it — the write was refused. Pin
   both; `approval_policy` is the governing control, `approvals_reviewer` only routes.
2. `codex exec resume` does **not** inherit the thread's read-only sandbox. Omit the
   setting and the resumed turn is **writable** — a file was created. Re-assert
   `-c sandbox_mode` on every turn.
3. `exec resume` rejects `-C` and `--sandbox` outright (parse error), so a single flag set
   cannot be reused across `exec` and `exec resume`.

Not reproduced: a *project-level* `.codex/config.toml` setting `approvals_reviewer` had no
effect at the path tested, so the project-config discovery path remains UNVERIFIED.

---

## 2. The wider field — invocation, output, sessions, steering

All VERIFIED (docs) unless the cell says otherwise.

| Agent | Headless | Structured output | Session resume | **Mid-run injection** |
|---|---|---|---|---|
| **Gemini CLI** | `-p`/`--prompt` (also auto when not a TTY) | `-o/--output-format text\|json\|stream-json` (JSONL: `init`, `message`, `tool_use`, `tool_result`, `error`, `result`) | `-r/--resume [latest\|N\|uuid]`, `--session-id`, `--session-file`, `--list-sessions`; stored under `~/.gemini/tmp/<project_hash>/chats/` | **NONE** — no `--input-format`, verified by absence in `packages/cli/src/config/config.ts` |
| **Antigravity CLI (`agy`)** | `-p`/`--print`/`--prompt` | `--output-format text\|json\|stream-json`; `--json-schema` → `structured_output` | `-c/--continue`, `--conversation <id>` | `--input-format stream-json` + `--output-format stream-json`; write `{"event":"user",…}` lines to stdin. **Sequential turns only** — next prompt read after the turn ends; `control_request` rejected with exit 2 |
| **Cursor CLI** | `-p`/`--print` | `--output-format text\|json\|stream-json`, `--stream-partial-output`; events `system`/`assistant`/`tool_call`/`result` | `--resume [chatId]`, `--continue`, `agent ls`, `agent create-chat` | **NONE** |
| **OpenCode** | `opencode run [message..]` | `--format default\|json` (raw JSON events) | `-c/--continue`, `-s/--session <id>`, `--fork` | **Not via CLI.** Only `opencode serve` HTTP: `POST /session/:id/prompt_async`, `/abort`, SSE `/event`. Behaviour against a *busy* session is UNVERIFIED |
| **Aider** | `-m/--message`, `-f/--message-file` | **plain text only** (no JSON flag in `aider/args.py`) | `--chat-history-file`, `--restore-chat-history` (file transcript, not a session id) | **NONE** |
| **Amp** | `-x`/`--execute` (also auto when stdout redirected) | `--stream-json`, `--stream-json-thinking` | `amp threads continue T-…`, `threads list/search/fork/export` | **YES** — `--stream-json-input` reads message lines from stdin; `steer: true` handles queued messages at interruption points *while processing continues* |
| **Qwen Code** | `-p`/`--prompt` | `-o/--output-format text\|json\|stream-json`, `--include-partial-messages`, `--json-schema`, `--json-fd`, `--json-file` | `-c/--continue`, `-r/--resume`, `--session-id`, `--fork-session` | `--input-format stream-json` exists (flag verified in source); **interrupt/queue semantics UNVERIFIED** |
| **Crush** | `crush run [prompt...]` (alias `r`) | **plain text only** for `run`; `session … --json` is metadata only | `run -s/--session <id>`, `run -C/--continue` | **NONE** |
| **Copilot CLI** | `-p`/`--prompt` | **plain text only**; `-s/--silent` trims decoration | `--resume`, `--continue` documented for interactive only; combination with `-p` **UNKNOWN** | **NONE** documented |

## 3. The wider field — permissions, auth, cwd, limits, MCP

| Agent | Sandbox / permissions | Auth, subprocess inheritance | cwd flag | Turn/step limits | MCP client |
|---|---|---|---|---|---|
| **Gemini CLI** | `--approval-mode default\|auto_edit\|yolo\|plan`, `-y/--yolo` (deprecated), `-s/--sandbox`, `--allowed-tools` (deprecated), `--allowed-mcp-server-names`, `--skip-trust` | OAuth or API key; credential path UNVERIFIED, inheritance via `$HOME` presumed UNVERIFIED | **NONE** (`--include-directories` widens, does not move) | no flag; `model.maxSessionTurns` in settings; exit code **53** = turn limit | yes — `gemini mcp add/remove/list` |
| **Antigravity (`agy`)** | `--dangerously-skip-permissions`, `--sandbox`; headless default **soft-denies** blocked tools; policy in `~/.gemini/antigravity-cli/settings.json` | **OS keyring** (Keychain / Secret Service / Windows Cred Manager) — subprocess inherits silently. API-key path needs `modelProvider: "gemini"` *plus* `GEMINI_API_KEY` | **UNKNOWN** (`/add-dir` is interactive) | none; `--print-timeout` (default 5m) is the only bound | yes — `~/.gemini/config/mcp_config.json`, workspace `.agents/mcp_config.json` |
| **Cursor CLI** | `-f/--force` (= yolo), `--sandbox enabled\|disabled`, `agent sandbox … --allow-paths/--readonly-paths/--blocked-patterns/--network`, `--approve-mcps`, `--trust` | `agent login` (store path UNKNOWN) or `CURSOR_API_KEY`/`--api-key`; subprocess inheritance UNVERIFIED | `--workspace <path>` | **NONE** | yes — `.cursor/mcp.json`, `~/.cursor/mcp.json` |
| **OpenCode** | `--auto`, `OPENCODE_PERMISSION` (inline JSON), agent-level `--permissions` per tool | `opencode auth login` → `~/.local/share/opencode/auth.json`; inherited via `$HOME` | `--dir` | **NONE** | yes — `mcp` key; config path via `OPENCODE_CONFIG`/`OPENCODE_CONFIG_DIR` |
| **Aider** | `--yes-always`, `--dry-run`, `--no-auto-commits`, `--subtree-only`. **No sandbox** | `--api-key PROVIDER=KEY`, env, `.aider.conf.yml`. No login session | **NONE** | **NONE** | **NO** — zero MCP support in `aider/args.py` |
| **Amp** | `amp.mcpPermissions` allow/reject rules; `--dangerously-allow-all` UNVERIFIED | `amp login` session token **or** `AMP_API_KEY`. Docs: non-interactive runs need an `sgamp_` key, **not** the interactive session token | **UNKNOWN** | **UNKNOWN** | yes — `amp.mcpServers`, `--mcp-config`, `amp mcp add` |
| **Qwen Code** | `--approval-mode`, `-y/--yolo`, `-s/--sandbox`, `--sandbox-image`, `--allowed-tools`, `--core-tools`, `--exclude-tools`, `--allowed-mcp-server-names`, `--safe-mode` | OAuth qwen.ai or `--openai-api-key`/`OPENAI_API_KEY`; credential path UNVERIFIED | **NONE** (`--include-directories` only) | `--max-session-turns`, `--max-tool-calls`, `--max-wall-time`, `--max-subagent-depth` — **richest of the set** | yes — `--mcp-config`, `qwen mcp` |
| **Crush** | `-y/--yolo`, `permissions allow/deny` in crushrc | `crush login [platform]` or provider env vars | `-c/--cwd` | **NONE** | yes — `.crushrc` / `~/.config/crush/crushrc` |
| **Copilot CLI** | `--allow-all`/`--yolo`, `--allow-all-tools`, `--allow-all-paths`, `--allow-all-urls`, `--allow-tool='shell(git:*)'`, `--deny-tool` (deny wins), `--no-ask-user`, `--secret-env-vars`, `--cloud` | `copilot login` OAuth stored under `~/.copilot` (or `COPILOT_HOME`); or `COPILOT_GITHUB_TOKEN` > `GH_TOKEN` > `GITHUB_TOKEN`; subprocess inherits via `$HOME` | `--add-dir=DIR` (allowed paths, not a cwd) | **NONE** | yes — `~/.copilot/mcp-config.json` |

## 4. Gemini CLI retirement — VERIFIED (docs)

Per Google's developer blog post *"Transitioning Gemini CLI to Antigravity CLI"*: on
**2026-06-18** Gemini CLI and the Gemini Code Assist IDE extensions stop serving requests
for Google AI Pro, Google AI Ultra and free-tier users. Paid Gemini Code Assist
Standard/Enterprise licences and paid Gemini API keys are unaffected. The replacement is
the Go-based **Antigravity CLI**, binary `agy`.

The post does **not** say the repository is archived, and `docs/cli/` is still current.
Practical consequence: ship both adapters. They are **not** flag-compatible — `agy` uses
`--print`/`--continue`/`--conversation`/`--dangerously-skip-permissions`; Gemini CLI uses
`-p`/`--resume`/`--approval-mode`.

## 5. Adapter capability flags forced by these findings

**Steer-capable** — Claude Code (`steer: queued`), **Amp** (`steer: true`). Codex is
**not** steer-capable on the `exec` surface: `queue` is a durable next-turn mailbox, not
an injection channel. The adapter schema distinguishes `true` (true mid-turn interruption)
from `queued` (delivered at the next turn boundary); both are steerable, they differ in
latency and in whether the current turn is cut short. Amp is the only agent outside our two
reference adapters with documented true mid-flight injection (`--stream-json-input` with
`steer: true`, handled at interruption points while processing continues).

**Interactive-capable** — Claude Code only, `interactive: true`, VERIFIED (docs/12 C7): a
question never appears on stdout; it arrives as an MCP `tools/call` on a gate server the
bridge points the child at via `--strict-mcp-config --mcp-config <gate config>
--permission-prompt-tool mcp__bridge_gate__ask --permission-prompts host`. The answer
returns as `behavior: "deny"` with the answer text as `message` — `allow` discards it. One
channel carries both a human question (`tool_name == "AskUserQuestion"`) and a permission
approval; the two are told apart by `tool_name`, never by a separate transport. **Codex is
`interactive: false`**: `codex exec` runs with `approval_policy="never"`, so there is no
correlated question for a gate to receive. No other adapter in this matrix is verified
interactive-capable; none is claimed as one. `capabilities.interactive: true` also requires `mode: read-only`: `mode: write` combined with it is a config load error, because v1 denies every approval wholesale and a write-mode interactive run would have every tool call refused by the gate despite looking correctly configured.

**Observed gap, Claude Code — now a load error:** declaring `capabilities.interactive: true`
is not enough — `AskUserQuestion` also has to survive whatever flag bounds the child's tool
surface. Since Trap 1 above forces that surface to be `--tools`, not `--allowed-tools`, a
`--tools` list that omits `AskUserQuestion` leaves the child with no way to raise a question
at all, even though the gate transport itself works. This was observed against a real CLI:
the model reported the tool was unavailable and asked in plain text instead. Rule 21 refuses
such a config at load time, and the shipped `claude` example (`examples/config.yaml`) grants
the tool in its `interactive` list, which lands after the sandbox flags — see
`docs/13-adapter-authoring.md` §5.

**`steer: false`** — Gemini CLI (no stdin input format), Cursor CLI (print mode one-shot),
Aider (one message per process), Crush (`run` one-shot), Copilot CLI (one-shot),
Antigravity (multi-turn in one process but strictly sequential: model as `steer: false`,
`multi_turn: true`), OpenCode (CLI one-shot; only the undocumented `serve` HTTP path could
change this — do not build on it without an empirical test), Qwen Code
(`--input-format stream-json` exists but interrupt semantics are undocumented — ship
`steer: false` until tested).

**`stream: false`** — Aider, Crush, Copilot CLI. Copilot is the worst case: no tool-call
visibility at all, so the watch TUI can only show start, final text and exit.

## 6. Per-adapter traps to encode

| Agent | Trap | Adapter rule |
|---|---|---|
| Codex | `approvals_reviewer = "auto_review"` defeats `--sandbox read-only`; `exec resume` inherits `workspace-write` | emit `-c approvals_reviewer="user"`; re-emit sandbox flags every turn |
| Claude Code | `--allowed-tools` does not remove `Bash`, auto-approved under `--print` | bound with `--tools`; always `--` before the prompt |
| Antigravity | headless **soft-denies** blocked tools and still **exits 0** | never key success off the exit code; parse the stderr notice |
| Antigravity | stream mode stays open by design until stdin closes | never wait on exit before reading stdout |
| Cursor CLI | without `--force` it only *proposes* edits | a "successful" run with an unchanged worktree is expected, not a bug |
| Crush | `--yolo`/`--debug` are **first-wins across clients sharing a `--cwd`** | a second bridge-spawned client can inherit the first's permission posture — never co-locate two Crush runs in one directory |
| Amp | interactive `amp login` token is unsuitable for `-x`; needs an `sgamp_` API key | **violates SR-1 (zero credential handling)** — Amp cannot ship as a verified adapter under the current auth model |
| Gemini, Qwen, Amp, Aider, Antigravity | **no cwd flag at all** | the bridge sets the child's working directory itself; `--include-directories`/`--add-dir` widen the workspace, they do not move it |
| All except Qwen (and Claude's `--max-budget-usd`) | no built-in turn or cost limits | the bridge's own wall-clock, turn and steer caps are the only bound |

## 7. What the matrix tells the design

1. **Two injection mechanisms, not one.** Codex uses a side-channel command, Claude and
   Amp use the child's stdin. The adapter schema needs both `mode: command` and
   `mode: stdin`. Confirmed by three independent vendors.
2. **Mid-run steering is rare in the ecosystem.** Three of eleven agents support it. That
   is precisely why no existing bridge implements it — and why an adapter schema must make
   `steer: false` a first-class, visible state rather than a failure.
3. **Zero-credential-handling holds everywhere except Amp.** Antigravity (keyring),
   OpenCode (`auth.json`), Copilot (`~/.copilot`), Codex (`auth.json`), Claude (keychain)
   all let a subprocess ride the existing login through `$HOME`. Amp explicitly does not,
   which under SR-1 disqualifies it from shipping as verified.
4. **Sandbox vocabularies do not align.** Three named levels (Codex), mode + tool
   allowlist (Claude), approval modes (Gemini/Qwen), path/network allowlists (Cursor),
   per-tool grants (OpenCode/Copilot), nothing at all (Aider). The adapter must carry an
   opaque per-mode flag list, never a shared enum.
5. **A flag can lie, and an exit code can lie.** Codex and Claude each have a documented
   case where a sandbox flag is overridden; Antigravity exits 0 on a run that did nothing.
   Conformance tests must assert *behaviour*, not flag acceptance.
6. **The bridge owns cwd.** Five agents have no working-directory flag, so setting the
   child's cwd in the spawn call — inside a validated allowed root — is the only portable
   mechanism. This makes SR-3 an implementation property rather than a per-adapter flag.

## 8. Adapter conformance checklist

Before an adapter ships as verified, all of these must be demonstrated against the pinned
CLI version in CI:

- [ ] headless invocation returns a final result and a meaningful exit code
- [ ] event stream parses; session id is extractable from it
- [ ] read-only mode genuinely refuses a write (attempt one, assert failure)
- [ ] read-only mode survives a hostile user/project config
- [ ] resume re-applies the sandbox (attempt a write on turn 2, assert failure)
- [ ] a prompt beginning with `-` cannot be reparsed as flags
- [ ] the declared steer channel visibly changes behaviour mid-run, or `steer: false`
- [ ] the child does not inherit a bridge registration in its own MCP config
- [ ] the child's cwd is the one the bridge set, not the parent's
- [ ] process-group kill leaves no orphan

Sources: [Cursor CLI parameters](https://cursor.com/docs/cli/reference/parameters) ·
[Cursor headless](https://cursor.com/docs/cli/headless) ·
[OpenCode CLI](https://opencode.ai/docs/cli/) ·
[OpenCode server](https://opencode.ai/docs/server/) ·
[Amp execute mode](https://ampcode.com/docs/cli/execute-mode) ·
[Amp streaming JSON](https://ampcode.com/docs/cli/streaming-json) ·
[Gemini CLI reference](https://github.com/google-gemini/gemini-cli/blob/main/docs/cli/cli-reference.md) ·
[Gemini headless](https://github.com/google-gemini/gemini-cli/blob/main/docs/cli/headless.md) ·
[Antigravity headless](https://antigravity.google/docs/cli/headless/) ·
[Antigravity install & auth](https://antigravity.google/docs/cli/install) ·
[Google blog: Transitioning Gemini CLI to Antigravity CLI](https://developers.googleblog.com/an-important-update-transitioning-gemini-cli-to-antigravity-cli/) ·
[Qwen Code options (source)](https://github.com/QwenLM/qwen-code/blob/main/packages/cli/src/config/top-level-options.ts) ·
[Crush README](https://github.com/charmbracelet/crush/blob/main/README.md) ·
[Aider options](https://aider.chat/docs/config/options.html) ·
[Copilot CLI programmatic reference](https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-programmatic-reference)
