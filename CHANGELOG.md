# Changelog

All notable changes to this project are documented in this file. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); this project has not yet made a
tagged release, so there are no version headings or dates below — only the accumulated
`Unreleased` state of the tree, grouped by the roadmap phase that produced it (see
[`docs/08-roadmap.md`](docs/08-roadmap.md) for the full phase descriptions and their
verification steps).

## Unreleased

### Added

- MCP server over stdio (`bridge serve --host <claude|codex>`) exposing `ask_<agent>`,
  `await_agent`, `steer_agent`, `cancel_agent` and `list_runs`; none of these tools can
  alter policy (Phase 0–1).
- Config loader with JSON Schema validation (`schema/config.schema.json`) plus 20
  semantic rules not expressible in JSON Schema (`internal/config/semantics.go`), enforced
  as load errors, never warnings.
- Platform abstraction (`internal/platform`) behind four interfaces — `Paths`,
  `PathGuard`, `ProcessGroup`, `ControlEndpoint` — with POSIX implementations; Windows
  constructors return errors and fail closed until Phase 5a.
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
- Basic-tier runner: a `tier: basic` adapter is pure YAML (invoke, sandbox flags, cwd
  confinement, buffered output) with no Go code and no rebuild.
- Two reference `tier: full` adapters, each with a registered `stream.Parser`: `claude`
  (Claude Code) and `codex` (OpenAI Codex CLI), encoding vendor traps found in
  `docs/12-spike-results.md` (Codex's `approvals_reviewer`/`approval_policy` interaction,
  `exec resume`'s rejection of `-C`/`--sandbox`, Claude's `--tools` vs `--allowed-tools`,
  `--setting-sources ""` as the only stop for a hostile repo's hook).
- Run manager, event normalisation from `codex exec --json` and `claude --output-format
  stream-json`, per-run JSONL transcripts, and non-blocking `ask_<agent>`/`await_agent`
  (Phase 2).
- Control socket (`internal/control`): Unix domain socket, `0600`, `SO_PEERCRED` UID
  check, connections from another UID refused and logged.
- `bridge watch` TUI (Bubble Tea): run picker, live event feed, tool/file/shell
  rendering, token counters.
- Write isolation (Phase 2a): disposable git worktree per write-mode run, diff extraction
  with per-file digests, `bridge accept`/`accept_changes` via `git apply --3way` (refusing
  on conflict), worktree removal on accept/reject/expiry, `worktree: off` behind a double
  config ceiling (`allow_write_mode` + `allow_unconfined_write`) labelled **UNCONFINED**
  in the tool description and `confinement: none` in the audit log.
- Interactive mode (Phase 3): a per-run gate transport (`bridge gate`) receiving the
  child's question as an MCP `tools/call` over a Unix socket, never by stream-event
  detection; a TUI answer panel; an `elicitation/create` fallback; a fail-closed
  `needs_input` result carrying a run-id `session_handle` when no operator channel is
  attached. Shipped for the `claude` adapter only — `codex exec` has no correlated
  approval request. `docs/08-roadmap.md`'s Phase 3 status note records that question
  attribution (`TargetAgent`) is not yet carried directly on `question.*` audit entries,
  only reconstructible via a `RunID` join, and that the two gated conformance tests
  exercising a live question/approval through the gate have not yet been run.
- Mid-run steering (Phase 4): `bridge steer` (operator, uncapped) and `steer_agent` MCP
  tool (agent, capped by `max_agent_steers`), steer origin recorded in the audit log,
  `steer: false` adapters returning `unsupported_capability`. Codex has no steer channel
  (`codex queue` is a next-run mailbox, not a mid-turn channel) and does not declare one.
- Feature toggles, limits and operator control (Phase 5): global default → adapter
  override → runtime toggle precedence (narrow-only, tested against widening attempts);
  `bridge enable/disable/status/runs/stop` over the control socket; concurrency,
  wall-clock, max-turns, steer-cap and output-cap limits; `bridge doctor` full check set;
  a schema-review test asserting no MCP tool carries a policy field.
- CI matrix (`.github/workflows/ci.yml`) building all six release targets
  (`linux/darwin/windows` × `amd64/arm64`) from the first commit, so the platform seams
  cannot rot while the Windows port (Phase 5a) is deferred; `gosec` and `govulncheck`
  scans; a `spec-consistency` job that loads every complete YAML example in the docs
  through the real config loader.
- Gated conformance suite (`BRIDGE_CONFORMANCE=1 go test ./conformance/`) invoking real
  `claude`/`codex` CLIs — nightly and manual-dispatch only, never on a pull request from a
  fork. Not run by default; SECURITY.md's enforced/declared distinction applies to
  everything this suite would exercise.
- Gated git-worktree tests (`BRIDGE_GIT_TESTS=1 go test ./internal/worktree/`), also not
  run by default.

### Known limitations

- Windows is not supported. `Paths`, `PathGuard`, `ProcessGroup` and `ControlEndpoint`
  Windows constructors compile but return errors; the port is scoped as Phase 5a,
  targeting v1.1.
- Interactive mode is Claude Code only, and its end-to-end behaviour against a real
  vendor CLI is *declared*, not *enforced* — see `SECURITY.md` and the Phase 3 status
  note above.
- No tagged release, no signed binaries, no SBOM or provenance attestation yet (Phase 6,
  in progress alongside this changelog).
