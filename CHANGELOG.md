# Changelog

All notable changes to this project are documented in this file. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); this project has not yet made a
tagged release, so there are no version headings or dates below — only the accumulated
`Unreleased` state of the tree.

## Unreleased

### Added

- MCP server over stdio (`bridge serve --host <claude|codex>`) exposing `ask_<agent>`,
  `await_agent`, `steer_agent`, `cancel_agent` and `list_runs`; none of these tools can
  alter policy.
- Config loader with JSON Schema validation (`schema/config.schema.json`) plus 21
  semantic rules not expressible in JSON Schema (`internal/config/semantics.go`), enforced
  as load errors, never warnings.
- Platform abstraction (`internal/platform`) behind four interfaces — `Paths`,
  `PathGuard`, `ProcessGroup`, `ControlEndpoint` — with POSIX implementations. The
  Windows implementations came later and have never been executed; see below.
- Adapter registry and argv builder (`internal/adapter`): `PATH` resolution, identity
  checks, self-call exclusion by `agent_id == --host`, placeholder expansion as whole
  argv elements only, rejection of shell metacharacters and `--dangerously*`/`--yolo`/
  `--allow-all` flags.
- Policy engine (`internal/policy`): `allowed_roots` confinement with symlink resolution,
  feature-toggle precedence (`config ceiling ≥ adapter declaration ≥ runtime toggle ≥
  per-call parameter`, narrow-only), depth guard.
- Process runner (`internal/run`): own process group per child, deny-by-default
  environment with an allowlist extension, timeout with group kill, no orphaned processes
  on cancel or bridge shutdown.
- Output sanitizer (`internal/sanitize`): control-character stripping, markup
  neutralisation, byte cap, untrusted-data envelope applied on every return path
  (success, failure, timeout, cancellation).
- Audit log (`internal/audit`): append-only JSONL, content digests (not bodies) by
  default, every policy refusal logged with its reason.
- Adapter validation (`bridge validate --config <path> [--agent <id>] [--stream-fixture <file>]
  [--json]`): loads a config through the real loader, then exercises each adapter against a
  hidden `stub-agent` verb that impersonates a vendor CLI with no credentials and no network.
  Reports argv building, prompt delivery by the declared mode, envelope and sanitisation,
  timeout kill, non-zero exit handling, sandbox flag presence (`sandbox_enforced: false` is
  reported as UNSANDBOXED), `stream.Parser` registration and fixture parsing, and the
  interactive rules 13/18/19/20/21 — one PASS/FAIL/SKIP line each, exit 1 on any FAIL.
- Basic-tier runner: a `tier: basic` adapter is pure YAML (invoke, sandbox flags, cwd
  confinement, buffered output) with no Go code and no rebuild.
- Two reference `tier: full` adapters, each with a registered `stream.Parser`: `claude`
  (Claude Code) and `codex` (OpenAI Codex CLI), encoding vendor traps found in
  `docs/12-spike-results.md` (Codex's `approvals_reviewer`/`approval_policy` interaction,
  `exec resume`'s rejection of `-C`/`--sandbox`, Claude's `--tools` vs `--allowed-tools`,
  `--setting-sources ""` as the only stop for a hostile repo's hook).
- Run manager, event normalisation from `codex exec --json` and `claude --output-format
  stream-json`, per-run JSONL transcripts, and non-blocking `ask_<agent>`/`await_agent`.
- Control socket (`internal/control`): Unix domain socket, `0600`, `SO_PEERCRED` UID
  check, connections from another UID refused and logged.
- `bridge watch` TUI (Bubble Tea): run picker, live event feed, tool/file/shell
  rendering, token counters.
- Write isolation: disposable git worktree per write-mode run, diff extraction
  with per-file digests, `bridge accept`/`accept_changes` via `git apply --3way` (refusing
  on conflict), worktree removal on accept/reject/expiry, `worktree: off` behind a double
  config ceiling (`allow_write_mode` + `allow_unconfined_write`) labelled **UNCONFINED**
  in the tool description and `confinement: none` in the audit log.
- Interactive mode: a per-run gate transport (`bridge gate`) receiving the
  child's question as an MCP `tools/call` over a Unix socket, never by stream-event
  detection; a TUI answer panel; an `elicitation/create` fallback; a fail-closed
  `needs_input` result carrying a run-id `session_handle` when no operator channel is
  attached. Shipped for the `claude` adapter only — `codex exec` has no correlated
  approval request. `question.*` audit entries carry the asking agent in `TargetAgent`,
  looked up from the run at write time; that lookup fails open, so a `question.asked`
  raised before the run registers is audited with an empty `TargetAgent` rather than
  blocking the write.
- Mid-run steering: `bridge steer` (operator, uncapped) and `steer_agent` MCP
  tool (agent, capped by `max_agent_steers`), steer origin recorded in the audit log,
  `steer: false` adapters returning `unsupported_capability`. Codex has no steer channel
  (`codex queue` is a next-run mailbox, not a mid-turn channel) and does not declare one.
- Feature toggles, limits and operator control: global default → adapter
  override → runtime toggle precedence (narrow-only, tested against widening attempts);
  `bridge enable/disable/status/runs/stop` over the control socket; concurrency,
  wall-clock, max-turns, steer-cap and output-cap limits; `bridge doctor` full check set;
  a schema-review test asserting no MCP tool carries a policy field.
- CI building all six release targets (`linux/darwin/windows` × `amd64/arm64`) from the
  first commit, so the platform seams cannot rot while the Windows port is deferred.
  These checks now live in the staged pipeline described below.
- Gated conformance suite (`BRIDGE_CONFORMANCE=1 go test ./conformance/`) invoking real
  `claude`/`codex` CLIs — nightly and manual-dispatch only, never on a pull request from a
  fork. Not run by default; SECURITY.md's enforced/declared distinction applies to
  everything this suite would exercise.
- Gated git-worktree tests (`BRIDGE_GIT_TESTS=1 go test ./internal/worktree/`), also not
  run by default.
- Staged CI/release pipeline (`.github/workflows/`): one ordered chain — security → tests →
  compile → smoke → release — with the jobs inside each stage running in parallel and the
  stages gated by `needs:`. The four shared stages are reusable `workflow_call` workflows
  (`stage-security.yml`, `stage-tests.yml`, `stage-compile.yml`, `stage-smoke.yml`) called by
  both `ci.yml` and `release.yml`, so a tag cannot take a shortcut past a check a pull
  request has to pass. Security runs TruffleHog and Gitleaks (secret scanning), govulncheck
  (reachable-dependency audit) and gosec; tests run golangci-lint, a gofmt gate and the race
  suite on Linux and macOS; compile builds all six release targets; smoke boots the binary
  and runs `bridge doctor` on both operating systems, loads every documented config example,
  and — on a schedule or a release — installs the pinned vendor CLIs (`claude` 2.1.272,
  `codex-cli` 0.154.0) and asserts the binaries report those versions.
- Versioning and compatibility policy (`README.md`): semantic versioning from `v1.0.0`,
  naming what the promise covers (MCP tool surface, config keys and defaults, adapter
  declaration keys, operator verbs, control-channel records, audit field names) and what it
  does not (`internal/` packages, transcript event shapes, TUI layout, Windows previews).
  A security fix may tighten a default in a minor release; everything else that narrows
  behaviour is a major.
- Explicit no-telemetry claim (`SECURITY.md` item 13): no update check, no crash reporter,
  no analytics, no outbound connection of any kind; the only sockets are the local control
  endpoint and the per-run gate.
- MIT licence (`LICENSE`), copyright Andrii Afanasiev. `.goreleaser.yaml` archives it with
  every release artifact; until now that `LICENSE*` glob matched nothing and the archives
  would have shipped unlicensed.
- Windows implementations of all four platform seams, partial and unexecuted: `Paths` with
  owner-only DACLs, `PathGuard` over `GetFinalPathNameByHandle` rejecting UNC paths, device
  namespaces, reserved device names and alternate data streams, `ProcessGroup` as a
  `KILL_ON_JOB_CLOSE` job object, and `ControlEndpoint` as a named pipe with an owner-SID DACL,
  `FILE_FLAG_FIRST_PIPE_INSTANCE` and a token-SID check in both directions. A new
  `platform.DialControl` seam carries the client half, and the three `net.Dial("unix")` call
  sites now go through it. **Compile-verified only — no Windows test has ever run**, and rows
  5a.1 (config DACL refusal), 5a.4, 5a.6-5a.9 remain open. Windows binaries stay unsupported
  previews.
- `platform.PostStarter`, an optional interface a `ProcessGroup` implements where the OS needs
  two-phase binding. Windows creates the child suspended so it cannot spawn a descendant before
  the job object captures it; `AfterStart` assigns and resumes it. POSIX implements it as a
  no-op, so `internal/run` needs no OS branch. Without the call the child would stay suspended
  forever.
- Release workflow (`.github/workflows/release.yml`): tag-gated (`v*`), running the
  same four stages before `goreleaser release --clean` for the six targets, with `syft` SBOMs
  per archive, keyless `cosign` signing of `checksums.txt` over GitHub OIDC, and SLSA build
  provenance via `actions/attest-build-provenance`. Write permissions are granted per job,
  never globally; `GITHUB_TOKEN` is the only credential.

### Fixed

- `await_agent` returned the run's result only as a text content block. `mcp.AddTool` infers
  an output schema from the tool's typed return value, and a host that sees an output schema
  may render `structuredContent` alone — Claude Code does — so the caller saw a run id, a
  state and no output at all, for the result, a `needs_input` question, the progress digest
  and the `superseded` pointer alike. Each now travels in both halves of the result: vendor
  words as the same enveloped string the text block carries, bridge-authored text as
  `notice` outside the envelope (FR-13).
- A vendor that reported an error in its event stream and still exited 0 — Codex on a usage
  limit — was indistinguishable from a success. Stream error events are now recorded on the
  run, returned inside the envelope, and counted as `vendor_errors` (FR-14).
- `vendor_error` was a boolean verdict on a signal that carries no verdict. Codex emits
  `item.completed / error` for warnings about the operator's own config — a malformed agent
  role file, a shortened skill description — so a run that answered correctly and exited 0
  came back flagged as a soft failure; three such events on one PONG run (codex 0.154.0). The
  field is now `vendor_errors`, a count of what the stream reported, and the run's state
  remains the only field that says whether the run failed (FR-14.2).
- The still-running `notice` and the progress notification carried the agent's last tool name
  and last changed path, both derived from the vendor's stream, in text that is deliberately
  outside the untrusted-data envelope. Both now use a counts-only digest
  (`stream.DigestCounts`); the operator's watch TUI keeps the full one (FR-13.3).
- A run whose output filled `max_output_bytes` returned a vendor-error count with the
  messages truncated off the tail. The messages are now budgeted before the answer, so the
  answer is what gets shortened (FR-14.3).
- An error event carrying no message was dropped, leaving the count at 0 for a run whose
  stream had reported an error. The occurrence is now recorded independently of the text,
  under the same per-run cap (FR-14.4).
- Every run was created with `Spec.SandboxEnforced` unset, so the run carried `false`
  regardless of the policy decision. `bridge runs` and the TUI header labelled sandboxed runs
  UNSANDBOXED, and `await_agent` reported `confinement: ""`, `sandboxed: false` for a run
  `ask_*` had correctly reported as worktree-confined and sandboxed. The decision's value now
  reaches the run, and `await_agent` reads both fields off it (FR-13.4).
- The interactive gate advertised its `input` parameter as an array of bytes, inferred from a
  `json.RawMessage` field. A vendor MCP client validates a call against the advertised schema
  before sending it, so a real `claude` child's `AskUserQuestion` never left the child and the
  bridge saw a run that simply completed (`docs/12-spike-results.md` C11).
- The gate answered a permission prompt with a text block plus `structuredContent`, which the
  vendor refuses outright: "Permission prompt tool returned an invalid result. Expected a single
  text block". The tool is now registered through the untyped `Server.AddTool`, with no output
  schema (`docs/12-spike-results.md` C10).
- A `needs_input` result carried only the question's text, dropping the options the agent
  offered — so a caller received "What should I name the file?" with no way to know the choices
  were `red.txt` and `blue.txt`. Both halves now reach the caller, inside the untrusted-data
  envelope.
- A continuation's successor was admitted against its predecessor's slot while the predecessor
  still counted as live, so the live set sat at `max_concurrent_runs`+1 until `Supersede` landed
  a moment later, and at +N with N continuations in flight. The predecessor now stops occupying
  its slot inside the same critical section that admits the successor, which makes the cap an
  invariant rather than an approximation.
- With those three fixed, the gated conformance suite passes end to end for the first time; the
  interactive and continuation properties in `SECURITY.md` move from *declared* to *enforced*.

### Known limitations

- Windows is not supported. The `Paths`, `PathGuard`, `ProcessGroup` and `ControlEndpoint`
  Windows implementations exist and cross-compile, but no test has ever executed them and
  no Windows machine has run the binary; config permission checking on Windows still
  refuses rather than guessing. The port targets v1.1.
- Interactive mode is Claude Code only. `codex exec` has no correlated approval request,
  so a Codex run gets the fail-closed `needs_input` path instead.
- No tagged release yet. The release pipeline is configured end to end but has never been
  executed: no tag exists, and `goreleaser`, `cosign` and `syft` were not available in the
  environment where it was written, so the signing, SBOM and provenance steps are
  *declared*, not *enforced*.
