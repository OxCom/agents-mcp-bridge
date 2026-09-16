# Threat model

## 1. Why this needs one

Most "call another agent" wrappers are treated as plumbing. They are not. A delegation
bridge sits on a boundary where an *untrusted text channel* becomes an *agent with
filesystem and shell authority*. Two distinct crossings happen per call:

- **Inbound:** a prompt composed by model A (itself influenced by web pages, repo
  contents, issue text) becomes the instructions of a second autonomous agent.
- **Outbound:** text produced by agent B (influenced by whatever B read) re-enters model
  A's context, where A may act on it.

Both crossings are injection surfaces. The bridge is the only place they can be controlled.

## 2. Assets

| Asset | Why it matters |
|---|---|
| Operator's source tree | B can edit it if granted write |
| Operator's credentials | `~/.codex/auth.json`, Claude keychain, SSH keys, `.npmrc` |
| Operator's shell authority | B runs commands as the operator |
| API spend | Unbounded delegation costs real money |
| Audit trail | Must be truthful to reconstruct what happened |
| The bridge's own config | Defines all authority in the system |

## 3. Trust boundaries

```
 operator ── trusted
    │
    │ owns config, runtime toggles, TUI answers
    ▼
 bridge ──── trusted core (the policy decision point)
    ▲   │
    │   │ argv (never a shell), restricted env, sandbox flags, cwd
    │   ▼
    │  agent B ── UNTRUSTED. autonomous, reads hostile data, emits hostile text
    │   │
    │   └── output ──► sanitizer ──► envelope ──► model A ── UNTRUSTED input channel
    │
 model A ─ UNTRUSTED. its tool arguments are attacker-influenceable
 repo/CWD ─ UNTRUSTED. cloned content is attacker-controlled
```

The critical inversion: **model A is not a trusted principal.** Tool arguments arriving
over MCP are treated exactly like untrusted user input from a network client.

## 4. Threats and controls

| # | Threat | Control |
|---|---|---|
| T1 | Injected prompt makes A delegate a destructive task to B | Read-only default; write is config-only, never a call parameter; no bypass flag is ever emitted; a write run reaches only a disposable worktree and produces a diff that must be accepted (FR-10) |
| T2 | B's output contains instructions that A obeys ("now run …") | SR-5 envelope + control-char strip + markup neutralisation + byte cap; never auto-executed |
| T3 | Command injection through the prompt | SR-4: argv list, no shell, placeholders as discrete argv elements |
| T4 | Path traversal via `cwd` to reach `~/.ssh`, `~/.codex/auth.json` | SR-3: symlink-resolved containment in `allowed_roots`; refusal is logged |
| T5 | Hostile repo ships `.agents-bridge.yml` granting write and a new adapter | SR-7: repo-local bridge config is never read. Cloning grants the bridge nothing |
| T5b | **Hostile repo ships `.claude/settings.json` with a `SessionStart` hook**, which runs arbitrary shell as the operator regardless of `--tools` | `--setting-sources ""` on every Claude invocation — VERIFIED as the only flag that stops it; `--strict-mcp-config` does **not** ([`12`](12-spike-results.md) C3). Codex's equivalent is `--ignore-user-config --ignore-rules`. Conformance-tested per adapter |
| T6 | A→B→A→… infinite recursion burning tokens | **Primary:** vendor config isolation stops B loading a nested bridge (`--ignore-user-config` for Codex, `--strict-mcp-config` for Claude), plus wall-clock, breadth and concurrency limits. **Secondary, best-effort:** a depth marker in B's environment — VERIFIED not to propagate to Codex's own MCP children, so it is not relied upon |
| T7 | Same-agent recursion (`claude` spawning `claude`) | FR-6: tool absent from `tools/list`; startup ancestry cross-check; no override |
| T8 | Credential theft through the bridge | SR-1: bridge never touches credentials; config carrying a key is rejected at load; env is deny-by-default |
| T9 | Secrets leak into the audit log or transcripts | SR-8/FR-8.2: hashes not bodies by default; env values never logged |
| T10a | **Agent B drives the control channel.** B is a same-UID process; if it can reach the endpoint it can answer its own questions, enable agent steering, or stop sibling runs | Do not pass the runtime directory or the run id into B's environment; reject peers whose PID is inside any run's process group or job. **Neither is a boundary** — stated as residual risk, not as a control |
| T10 | Another local user reads transcripts or drives the control channel | POSIX: socket `0600` + `SO_PEERCRED` UID check, state dir `0700`, config refused if group/world-writable. Windows: named pipe DACL limited to the user SID + per-connection token SID check, `FILE_FLAG_FIRST_PIPE_INSTANCE` against name squatting, `servers.json` and the per-run gate config written with an owner-only DACL (`platform.WriteOwnerOnlyFile`) since a Go file mode grants nothing there. **The Windows config check is not implemented** (v1.1): `internal/config/perm_windows.go` refuses every config file rather than assume it is protected, so the DACL inspection named here is a target, not a shipped control. `bridge doctor --insecure-skip-permission-check` is the only way past that refusal, and it trusts the file without proof |
| T11 | Network attacker reaches the bridge | No listener. stdio MCP only; **local IPC only** — Unix socket on POSIX, named pipe on Windows |
| T12 | Runaway spend from agent-driven steering loops | Steer cap for agent-originated steers, wall-clock limit, max-turns, roadmap: budget from usage events (`claude --max-budget-usd` already exists) |
| T13 | Orphaned agent processes survive the host | Process groups + reaping on shutdown, cancel and timeout |
| T14 | Supply-chain compromise of the bridge itself | Static Go binary, no runtime package resolution, signed and checksummed releases, provenance in CI |
| T15 | B re-enters the bridge through its own MCP config | Adapters pass the vendor's MCP-isolation flag (`claude --strict-mcp-config`, `codex --ignore-user-config`) so B does not inherit a bridge registration |
| T16 | Model widens its own authority through a tool argument | No tool accepts a policy field. Toggles live on the operator-only control socket |
| T17 | Operator is socially engineered by B's question text | Questions pass the same sanitizer; the TUI renders them clearly marked as agent-authored, never as bridge UI text |
| T18 | Elicitation prompt in host A used to phish the operator | Elicitation payload is sanitized and always labelled with the source agent and run id |
| T19 | **Windows command-line re-parsing.** The OS passes one string, not argv; a `.bat`/`.cmd` adapter makes `cmd.exe` re-parse arguments and reintroduces metacharacter injection | Explicit `SysProcAttr.CmdLine` with MSVCRT-correct quoting; `.bat`/`.cmd`/`.ps1` refused as an adapter `command`; unquotable arguments rejected at build time |
| T20 | **Windows path aliasing** defeats the allowed-roots check: 8.3 short names (`PROGRA~1`), junctions, case variance, UNC paths, device namespaces, alternate data streams | Canonicalise through all of them before the containment test; compare case-insensitively; reject UNC, `\\?\`, `\\.\`, reserved device names and ADS outright; a path that cannot be canonicalised is refused |
| T21 | **Named-pipe squatting**: a hostile local process creates the control endpoint first and impersonates the bridge to the TUI | `FILE_FLAG_FIRST_PIPE_INSTANCE` on create; the client verifies the server process SID before sending anything |
| T22 | Orphaned agent processes survive a hard kill of the bridge on Windows, where there is no process group | Job object with `KILL_ON_JOB_CLOSE` — kernel-enforced, survives SIGKILL-equivalent termination of the bridge |
| T23 | **The gate socket is an address deliberately given to B** — a process other than B's own child reaches it, or B itself floods or stalls it | A per-run token (carried only in that run's mcp-config) required alongside `SO_PEERCRED` UID check, the same control as T10; deny-by-default decode with a 64 KiB request cap and a 10s request-read deadline; a resilient accept loop capped at 8 connections in flight so a hostile peer cannot exhaust goroutines or take the gate down; the gate refuses every request once its run is terminal. The gate socket also lives in its own subdirectory of the runtime directory, never as a sibling of `servers.json` (the index of every live bridge's operator control socket) — B is handed one address and nothing that points at the other. Defence in depth, not a boundary: same-UID is still not a privilege boundary (T10a), and a same-UID process can still reach `servers.json` by other means |
| T24 | **A continuation's seed re-injects the delegated agent's own question.** The question is authored by the untrusted child; placing it in the successor's prompt as bare instruction would let a misbehaving agent write part of its own next prompt under the guise of a clarifying question, defeating the human-in-the-loop reset `needs_input` exists to enforce | The seed splits by authorship, not by content: every question in the chain, together with the prior attempt's changed file names (also B's own text — a confined-write git status leaves a space-free path unquoted, so a crafted name is bare attacker prose like any other agent output), travels inside the untrusted-data envelope with attribution. The operator's answer to each turn is deliberately **not** enclosed — it is sanitized (control-strip, cap) but delivered outside the envelope, attributed to the operator, as a genuine instruction. Enclosing the answer too was an earlier defect: the envelope's own preamble tells the reader not to act on anything inside it, which would have told the successor to ignore the operator's answer along with the question. Only the bridge-authored preamble, the operator's answer and the caller's original prompt sit outside the envelope (`02` §2.3a, `cmd/bridge/continuation.go`'s `buildContinuationSeed`) |
| T23a | **The per-run gate mcp-config file** (`gate-<run_id>.json` in the state directory) is a new object placed inside B's reach — it is what actually carries the gate's address and token to the child | Written mode `0600`, outside any worktree (B's writable workspace) so it sits alongside other bridge state, not among files B is expected to edit or that a write-mode diff could surface; its token is minted fresh per run and is the same per-run, single-use secret T23 already treats as useless outside this run; `closeGate` removes the file when the run ends, when the run settles into `needs_input` (the child is already gone at that point, so the gate socket and this file would otherwise sit reachable for as long as the run rests in `needs_input`), when a gate-armed `buildSpec` fails before the run starts, and on server shutdown, so no stale mcp-config survives its run to be read by a later process |

## 5. Explicit non-goals

- **Every claim below about write confinement is scoped to the default
  `worktree: required`.** Under `worktree: off` there is no worktree, no diff, no
  acceptance step and no undo: B edits the caller's tree in place, and path validation is
  the only filesystem control left.
- **Write mode is confined, not contained** *(under `worktree: required`)*. A write run touches only a disposable
  worktree and its result must be accepted before it reaches your tree. That bounds the
  *filesystem* blast radius. It does not stop B from running commands, using the network,
  reading anything you can read, or writing outside the worktree if the vendor sandbox is
  bypassed. Confinement is a control; containment would need OS-level isolation.
- **The bridge is not a sandbox.** It configures the target CLI's sandbox and confines
  paths; it does not implement isolation itself. If B's vendor sandbox is bypassed, the
  bridge's remaining controls are containment and audit, not prevention. OS-level
  isolation (bubblewrap/landlock, containers) is a roadmap item, not a v1 claim.
- **The bridge cannot make an untrusted model trustworthy.** It bounds what B may do and
  makes B's output non-authoritative. It does not verify B's reasoning.
- **No protection against a malicious operator.** Config is the root of authority.

## 6. Residual risks

| Risk | Why accepted | Mitigation path |
| Config directory swapped between the parent-directory check and the open | The file itself is validated on the open descriptor, so its bytes cannot be swapped; only the directory check is path-based | Refusing a non-owner-writable directory removes the attacker who could do it. A fully race-free form needs `openat2` with `RESOLVE_NO_SYMLINKS` (Linux 5.6+) and is not portable |
| PID recycling between `Getpgid` and `kill(-pgid)` | The window is microseconds and the group is one the bridge created; a recycled pgid would have to be created inside it | Kernel process handles (`pidfd` on Linux, `EVFILT_PROC` on macOS) |
|---|---|---|
| Vendor sandbox flag changes meaning across a release | Out of our control | Pin-tested adapters; CI smoke test per supported CLI version; `--strict-config` where available |
| A vendor's steer command does not accept the isolation flags its run command does — `codex queue` has no `--ignore-user-config` or `--ignore-rules` | The steer path is a separate short-lived process the bridge cannot bring under the run's isolation policy | Validate the queue target's thread ownership before injecting; never claim invocation-style config isolation for the steer path ([`04`](04-config-schema.md) §3) |
| An adapter runs with `worktree: off` | The operator asked for direct edits; a bridge that refuses would be replaced by one that does not | Double config ceiling (`allow_write_mode` **and** `allow_unconfined_write`), UNCONFINED in the tool description, `confinement: none` in every audit line, path validation still enforced. There is no rollback |
| An adapter runs with `sandbox_enforced: false` | Some CLIs have no read-only mode at all; refusing them entirely would exclude most of the field | The word UNSANDBOXED is surfaced in the tool description, `bridge status` and every audit record; path confinement is the only remaining control |
| Agent-to-agent steering enabled (operator choice) creates an autonomous loop | Explicitly requested capability | Caps, audit, kill switch, disabled per-agent by toggle |
| Sanitizer is pattern-based and cannot be complete | No complete solution exists | Envelope is the primary control; stripping is defence in depth, not the guarantee |
| Transcripts may contain sensitive repo content | Needed for the watch feature | `0700` state dir (POSIX) / owner-only DACL (Windows), retention policy, `--no-transcript` toggle |
| Windows has no OS-isolation story equivalent to bubblewrap | Post-1.0 OS isolation is Linux-only; Windows relies on the vendor CLI's own sandbox plus path confinement | Documented as a platform difference in SECURITY.md, not hidden |
| The gate's per-run token is readable by the child it belongs to | The child needs it to authenticate its own requests; a token only it can read is the design, not a leak | Scoped to one run, invalid once that run reaches a terminal state or settles into `needs_input` — useless against any other run or after this one ends |
| A continuation record lives in memory only and dies with a bridge restart | Persisting prompt bodies (the original prompt, the question/answer chain) to disk would be a new data-retention commitment in a project that audits digests, not bodies | An operator whose bridge restarted gets `ErrContinuationUnavailable` on the next `bridge answer` — a typed refusal, never a silent no-op or a continuation seeded with partial context |
| A predecessor's `needs_input` deadline can expire in the gap between the successor's `Start` succeeding and `Run.Supersede` committing | The window is one function call wide, not the whole seed-construction path it replaced; closing it fully would require reversible-state machinery in `internal/run` that conflicts with "terminal means no live child, ever" | The predecessor settles `failed` (expired) instead of `superseded`, with no `superseded_by` — but its chain-owned state (change-store entry, worktree handle, gate) is retired unconditionally the moment the successor's `Start` succeeds, not gated on `Supersede`'s own outcome, so `get_changes`/`accept_changes` on it still refuse rather than minting a second acceptable diff for the chain; the successor is already live and is not torn down to chase the label mismatch, and its own `run.admitted` entry still carries `resumed_from`, so the chain stays traceable end to end |
