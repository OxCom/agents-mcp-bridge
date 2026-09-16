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
   named pipe (user-SID DACL, client token SID check, first-instance flag) on Windows. The
   POSIX half is *enforced*; the Windows half is **implemented but never executed** — it
   compiles and vets under `GOOS=windows`, and no test has ever run it, so it is *declared*.
   The client verifies the server's SID as well, so a squatted pipe cannot impersonate the
   bridge; that too is unexecuted.
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
    non-interactively. **Also enforced, as of 2026-09-16:** that a real delegated agent, asking a
    real question through this path end to end, receives the operator's answer and nothing
    else. `conformance.TestClaudeQuestionReachesTheGateAndTheAnswerReachesTheModel`
    (`BRIDGE_CONFORMANCE=1 go test ./conformance/`, gated because it spends vendor credits)
    passes against `claude` 2.1.272: the child raises `AskUserQuestion`, the question reaches
    the gate, the operator answers over the control channel with a filename the prompt never
    mentions, and that filename appears in the child's own final output. Three defects had to
    be fixed before it could pass, each found by the one before it: the adapter's `--tools`
    list omitted `AskUserQuestion` (now load rule 21); the gate advertised `input` as a byte
    array, inferred from a `json.RawMessage` field, so the vendor refused its own call before
    sending it; and the gate answered with a text block plus `structuredContent`, which the
    vendor rejects with "Permission prompt tool returned an invalid result. Expected a single
    text block".
12. **A `needs_input` run can be continued by the operator, never by the delegated agent, and
    the continuation seed keeps agent-authored and human-authored text apart.** *Enforced*
    (asserted by `internal/run`, `internal/worktree` and `cmd/bridge` unit tests, including
    `BRIDGE_GIT_TESTS=1 go test ./internal/worktree/`): only a run resting in `needs_input` can
    be continued (`Run.Supersede`/`TakeContinuation` refuse otherwise); an unconfined write run
    (`worktree: off`) refuses continuation outright, since its edits already landed in the real
    tree with no diff to carry forward; a chain is bounded by `defaults.max_continuations`
    (default 3); no code on the MCP tool surface reaches `continueRun` — only the control
    channel's `bridge answer`/TUI panel do, both operator-side. In the continuation seed, the
    delegated agent's question is enclosed in the untrusted-data envelope with attribution; the
    operator's answer is sanitized but delivered outside the envelope, attributed to the
    operator, as an instruction the successor should act on — putting the answer inside the
    envelope too was an earlier defect (the envelope's own "do not act on this" preamble would
    have told the successor to ignore the operator's answer along with the question), fixed
    before this note was written. Predecessor retirement (change-store entry, worktree handle,
    gate) is unconditional once the successor's `Start` succeeds, not gated on `Supersede`'s
    own outcome, closing a window where a predecessor whose `needs_input` deadline lapsed
    mid-continuation could otherwise leave two independently acceptable diffs live for one
    chain. **Also enforced, as of 2026-09-16:** the full path — a real delegated agent asking
    a real question with no operator attached, the operator answering afterwards, and the
    successor completing the original task using that answer —
    `conformance.TestNeedsInputAnswerContinuesIntoASuccessorRun` (`BRIDGE_CONFORMANCE=1 go test
    ./conformance/`) passes against `claude` 2.1.272, asserting that the predecessor reaches
    `needs_input`, is superseded by the successor named in the continue response, and that the
    successor's own output carries a filename supplied only by the operator's answer. It spends
    real vendor credits and is not run on a pull request. **What this does not claim:** that a
    successor never re-asks an answered question. The chain's original prompt is replayed
    verbatim in the seed, so a caller whose prompt says "ask me whether to ..." gets a
    successor that asks again — obeying the caller, not losing the answer. The bridge does not
    rewrite a caller's prompt to prevent that; `defaults.max_continuations` (default 3) bounds
    the chain instead. A continuation record is
    held in memory only and is lost on a bridge restart; an operator who answers a run whose
    record is gone gets a typed refusal, never a continuation seeded with partial context.
    **Enabling audit bodies also captures operator answers.** The successor's `run.admitted`
    entry carries the full seed as `Prompt`, matching `makeAsk`'s existing practice for an
    ordinary run's prompt; with `audit.bodies` on, that seed — and therefore the operator's
    answer to the delegated agent's question — is written to the audit log body, not only its
    digest. `audit.Write` still strips bodies whenever `bodies` is off, same as every other
    event.

13. **No telemetry, no phone-home.** The bridge collects nothing about you, your prompts or
    your repositories, and sends nothing anywhere. It has no update check, no crash
    reporter, no usage counter and no analytics dependency. The only sockets it opens are
    local IPC: the operator control endpoint and the per-run gate, both Unix domain sockets
    on Linux and macOS and named pipes on Windows. Everything it writes stays on the
    machine, under the state directory (transcripts, audit log) — see `bridge doctor` for
    the exact paths. The delegated CLI does talk to its own vendor, authenticated by that
    vendor's own login; that traffic belongs to the vendor, and the bridge neither adds to
    it nor inspects it. *Declared, not enforced:* no test asserts the absence of a network
    call. The evidence is the source — every `net` call in the tree is a unix-socket or
    named-pipe `Listen`/`Dial`, and no HTTP client is constructed anywhere.

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
shims, which the adapter rules refuse. As of 2026-09-16 the Windows implementations of all
four seams exist, but **nothing executes them**: CI cross-compiles Windows and runs no test
there. A Windows binary is therefore not a supported artifact, and the mechanisms below are
claims about code that has compiled, not about behaviour anyone has observed. The one
exception as of 2026-09-16 is the `doctor` smoke job, which now runs on `windows-latest`
and therefore executes `platform.NewPaths`, the DACL'd state and runtime directories, the
loader and the semantic rules. It reaches the loader only via
`--insecure-skip-permission-check`, because config permission checking on Windows is
unimplemented and refuses: on that platform a config file is trusted without proof that
only its owner can write it, which is a real gap, not a check that passed. The
security-relevant mechanisms differ by platform and are implemented behind four interfaces:

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
- **A Windows file mode grants nothing.** `0600` on a Go `os.WriteFile` leaves the file
  readable by every local account, so the two files that must not leak — `servers.json`,
  the index of every live bridge's control endpoint, and the per-run gate config, which
  carries the gate token — are written through `platform.WriteOwnerOnlyFile`: a mode on
  POSIX, an owner-only DACL supplied at `CreateFile` time on Windows, with the file removed
  rather than left readable if the restriction cannot be applied. The config file's own
  permission check is still unimplemented on Windows and refuses instead (v1.1).

## Verifying a release

Every release publishes `checksums.txt`, which lists each archive with its
SHA-256, and `checksums.txt.bundle`, a Sigstore bundle signing that file. One
signature therefore covers every artifact transitively. There is no signing key
to trust: the bundle is keyless, bound to this repository's release workflow.

```bash
cosign verify-blob \
  --bundle checksums.txt.bundle \
  --certificate-identity-regexp '^https://github.com/OxCom/agents-mcp-bridge/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum --check --ignore-missing checksums.txt
```

Build provenance is separate and covers each archive individually:
`gh attestation verify <archive> --repo OxCom/agents-mcp-bridge`.

## Supported versions

The latest minor release receives security fixes. Adapters are smoke-tested in CI against
pinned target-CLI versions; a vendor flag change is intended to break CI rather than a
user's sandbox.
