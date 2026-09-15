# Security policy

## Reporting a vulnerability

Do not open a public issue for a security problem. Report privately through GitHub's
private vulnerability reporting on this repository. You will get an acknowledgement within
3 working days and a status update at least every 7 days until resolution.

Please include: version, host agent and target agent, config (with paths redacted), and a
minimal reproduction. If a fix requires a coordinated release, we will agree a disclosure
date with you.

## What this tool is

`agents-mcp-bridge` lets one CLI coding agent run another as a subprocess. That makes it a
trust boundary, not plumbing. The full analysis is in
[`docs/03-threat-model.md`](docs/03-threat-model.md).

## Security properties we claim

1. **The bridge does not parse, store or forward credentials.** The target agent
   authenticates with your own existing login, which it reaches through `HOME` like any
   other process you run. A config naming a known credential variable in `env_allowlist`
   is rejected at load, and the child environment is deny-by-default over a per-platform
   baseline. **This is not credential isolation:** agent B runs as you, so it can read
   every file you can read, including `~/.codex/auth.json` and `~/.ssh`. API-key auth is
   unsupported in v1 precisely because supporting it would put the bridge in the
   credential path.
2. **Read-only by default; write runs are confined to a worktree unless you opt out.** Write access is
   opt-in per agent in the config file, never a call parameter. A write-mode run executes
   in a disposable git worktree and returns a diff that must be explicitly accepted before
   anything reaches your tree. An adapter may set `worktree: off` to edit your tree
   directly; that requires two separate config ceilings, is labelled **UNCONFINED** in the
   tool description and `confinement: none` in the audit log, and has no diff and no
   rollback — see the non-claims below. The bridge never emits a `--dangerously-*`,
   `--yolo` or `--allow-all` bypass flag under any configuration.
3. **No shell.** Command lines are built as argv lists. No `sh -c`, no interpolation. A
   `command` containing shell metacharacters fails to load.
4. **Working-directory validation.** A caller-supplied working directory is
   symlink-resolved and must lie inside a configured allowed root, and it is the child's
   actual spawn directory. **This bounds where B works, not what B can read** — no vendor
   read-only sandbox restricts reads to the working directory.
5. **Untrusted output.** The delegated agent's output is sanitized and returned inside an
   explicit untrusted-data envelope. It is never auto-executed.
6. **Bounded recursion and cost.** *Enforced:* vendor config isolation prevents the
   delegated agent loading a nested bridge at all (`--ignore-user-config` for Codex,
   `--setting-sources ""` for Claude), plus wall-clock timeout, concurrency and rate
   limits, output cap, and a cap on agent-originated steering. *Best-effort only:* a depth
   marker in the child's environment — Codex is VERIFIED not to propagate its environment
   to MCP servers it spawns, so the marker may never arrive and is not relied upon.
7. **Self-call exclusion.** An agent cannot delegate to itself; that tool is not exposed.
8. **No network listener.** MCP is stdio only, on every platform. Operator control uses a
   local IPC endpoint restricted to the current user, with the peer's identity verified on
   every connection: a Unix socket (`0600`, `SO_PEERCRED` UID check) on Linux and macOS, a
   named pipe (user-SID DACL, client token SID check, first-instance flag) on Windows.
9. **Repo-local config is never read.** Cloning a hostile repository cannot change policy.
10. **Reproducible, signed releases.** Static binary, no package resolution at launch.
11. **Interactive mode is double-gated and fails closed.** *Enforced* (asserted by
    `internal/gate` and `cmd/bridge` unit tests — `server_test.go`, `gate_test.go`,
    `gateconfig_test.go`): a run gets a gate socket and mcp-config only when the adapter
    declares `capabilities.interactive` *and* the operator has turned on
    `features.interactive` (default off); the gate socket and its per-run mcp-config carry
    the child's only address for asking a question, authenticated by a per-run token plus
    `SO_PEERCRED`; the mcp-config is written `0600` outside any worktree and removed when
    the run ends, when gate startup is followed by a failed `buildSpec`, or on server
    shutdown; a gate that fails to start aborts the run rather than proceeding
    non-interactively. **Declared, not yet enforced:** that a real delegated agent, asking a
    real question through this path end to end, actually receives the operator's answer and
    nothing else — no conformance test yet drives a real vendor CLI through a question, so
    this end-to-end property rests on the unit-level guarantees above plus manual testing,
    not on an automated assertion.

## Security properties we do NOT claim

- **The bridge is not a sandbox.** It configures the *target CLI's* sandbox and confines
  paths. If a vendor's sandbox is bypassed, our remaining controls are containment and
  audit, not prevention.
- **Output sanitization is not a guarantee.** The untrusted-data envelope is the primary
  control; pattern stripping is defence in depth. No filter reliably detects prompt
  injection.
- **`worktree: off` is an unconfined write.** With that flag the agent edits your working
  tree in place: no worktree, no diff, no acceptance step, no rollback. Working-directory
  validation still applies; nothing else does. It exists because an operator who wants
  direct edits will otherwise use a tool that gives them silently — here it is at least
  double-gated and loudly labelled.
- **No defence against a malicious operator.** The config file is the root of authority.
- **OS-level isolation is Linux-only and post-1.0.** The optional bubblewrap/landlock
  wrapper has no macOS or Windows equivalent. It is defence in depth where available, never
  a property the security model depends on.

## Known vendor traps handled

Verified against real CLI releases and encoded in the shipped adapters:

- **Codex:** `--sandbox read-only` is not binding when config sets
  `approvals_reviewer = "auto_review"` — `-c approvals_reviewer="user"` is emitted
  alongside every sandbox level. `codex exec resume` inherits `workspace-write` from
  project trust if the sandbox flag is omitted, so sandbox flags are re-emitted on every
  turn and never persisted in session state.
- **Claude Code:** `--allowed-tools` does not remove `Bash`, which is auto-approved under
  `--print`; only `--tools` bounds the surface. A prompt not preceded by `--` can be
  reparsed as flags.

If you find another such trap, it is a valid security report.

## Platform notes

**v1.0 supports Linux and macOS. Windows is v1.1.** All six targets compile and are built
in CI from the first commit, and Windows binaries are published as unsupported previews
until the port lands — the blocker is that npm-installed `claude` and `codex` are `.cmd`
shims, which the adapter rules refuse. The security-relevant mechanisms differ by platform
and are implemented behind four interfaces, each with conformance tests:

- **Windows has no argv.** The OS passes one command-line string that each program parses
  itself, so the bridge sets the command line explicitly with MSVCRT-correct quoting and
  **refuses `.bat`, `.cmd` and `.ps1` as an adapter command** — `cmd.exe` re-parses their
  arguments and would reintroduce metacharacter injection.
- **Windows path aliasing is a containment problem.** 8.3 short names, junctions, case
  variance, UNC paths, device namespaces and alternate data streams can all defeat a naive
  allowed-roots check. All are canonicalised or rejected before the containment test; a
  path that cannot be canonicalised is refused.
- **Windows has no process groups.** Children run in a job object with
  `KILL_ON_JOB_CLOSE`, so a hard kill of the bridge cannot leave orphaned agents.
- **Named pipes can be squatted.** The endpoint is created with
  `FILE_FLAG_FIRST_PIPE_INSTANCE` and the client verifies the server's SID before sending.

## Supported versions

The latest minor release receives security fixes. Adapters are smoke-tested in CI against
pinned target-CLI versions; a vendor flag change is intended to break CI rather than a
user's sandbox.
