# Agents MCP bridge

One MCP server that lets any CLI coding agent delegate work to any *other* CLI coding
agent — safely, visibly, and steerably.

> **Status: pre-release, no tagged version yet.** Two reference adapters (`claude`,
> `codex`) ship. Build, `go vet`, `-race` tests and all six cross-build targets are green.
> See [`SECURITY.md`](SECURITY.md) for which properties are *enforced* (asserted by a test)
> versus *declared* (true by design, not yet exercised end to end by an automated test).
> Windows is not supported: its binaries are unsupported previews and target v1.1.

```
Claude Code ──MCP──► bridge ──argv──► Codex        you ──► bridge watch
                       │                             (live TUI, second pane)
                       ├─ read-only by default       ├─ see every tool call B makes
                       ├─ cwd confined to allowed roots
                       ├─ B's output returned as untrusted data
                       └─ audit log, depth guard, timeouts
```

## What makes it different

Every existing bridge is a subprocess wrapper. This one treats delegation as a trust
boundary and as a supervised activity:

- **Config-declared agents, two tiers.** A *basic* agent is ~15 lines of YAML and no
  rebuild: invoke, sandbox flags, cwd confinement, buffered output, audit. A *full* agent
  adds an in-tree Go adapter for streaming, sessions, steering and interactivity. See
  [`docs/13-adapter-authoring.md`](docs/13-adapter-authoring.md) for how to write one.
- **Live watch.** `bridge watch` renders B's tool calls, file edits and shell commands as
  they happen, in a second terminal.
- **Interactive.** When B asks a question, *you* answer it — in the TUI, or via MCP
  elicitation inside your host agent. Never guessed, never silently defaulted. Shipped for
  Claude Code only — `codex exec` has no correlated approval request to surface, so a
  Codex run fails closed into `needs_input` instead. This path is double-gated (adapter
  capability plus an operator-enabled feature flag), and a gated conformance test drives a
  real `claude` child through the whole path — question, operator answer, the answer
  reaching the model — so the end-to-end property is *enforced*, not merely declared. That
  suite spends vendor credits and runs on demand, never on a pull request.
- **Mid-run steering.** Type extra guidance into a running agent through its vendor's own
  channel (`codex queue`, Claude's streaming stdin). Agent A can steer too, under a cap.
- **Self-call exclusion.** Claude cannot delegate to Claude; Codex cannot delegate to
  Codex. The tool is not exposed at all.
- **Write runs are confined by default.** A write-mode run happens in a disposable git
  worktree and returns a diff; nothing reaches your tree without an explicit acceptance
  step. An operator who wants direct edits can set `worktree: off` — double-gated in
  config and labelled UNCONFINED wherever the run appears.
- **Secure by construction.** Read-only default, working-directory allowlist, no shell,
  no credential parsing, untrusted-output envelope, audit log, no network listener.

## Quick shape

```bash
bridge serve --host claude    # registered in Claude Code's MCP config
bridge serve --host codex     # same binary, registered in Codex
bridge watch                  # operator TUI, second terminal
bridge status | runs | stop | steer | answer | enable | disable | doctor
bridge validate --config <path>   # exercise every adapter against a stub agent
```

Tools exposed to the host agent: `ask_<agent>`, `await_agent`, `steer_agent`,
`cancel_agent`, `list_runs`. None of them can change policy.

## Building and testing

```bash
make all      # go vet + go test -race ./... + go build
make cross    # verify all six release targets compile (linux/darwin/windows × amd64/arm64)
```

Two suites are gated and do not run in `make all` or the default CI job:

```bash
BRIDGE_CONFORMANCE=1 go test ./conformance/ -v   # invokes real claude/codex CLIs, spends credits
BRIDGE_GIT_TESTS=1 go test ./internal/worktree/  # creates throwaway git repos
```

CI (`.github/workflows/ci.yml`) runs one ordered pipeline on every push — **security →
tests → compile → smoke** — where the jobs inside a stage run in parallel and each stage
gates the next. Security is secret scanning (TruffleHog, Gitleaks) plus `govulncheck` and
`gosec`; tests are `golangci-lint`, a `gofmt` gate and `go vet` + `go test -race` on Linux
and macOS; compile builds all six cross-compile targets; smoke boots the binary, runs
`bridge doctor` on Linux, macOS and Windows and loads every documented config example. The
four stages are reusable workflows (`stage-*.yml`) that `.github/workflows/release.yml`
calls in the same order before publishing, so a tag runs exactly the checks a pull request
does. The conformance suite and the pinned-vendor-CLI smoke job run nightly and on manual
dispatch only — never on a pull request from a fork.

## Documentation

| Doc | Contents |
|---|---|
| [01 Requirements](docs/01-requirements.md) | Functional, security and non-functional requirements; scope |
| [02 Architecture](docs/02-architecture.md) | Components, event model, tool surface, reference adapters |
| [03 Threat model](docs/03-threat-model.md) | Assets, trust boundaries, threats and controls, residual risk |
| [04 Configuration](docs/04-config-schema.md) | Where config lives, host registration, worked config files, placeholder rules, operator commands |
| [07 Agent capability matrix](docs/07-agent-capability-matrix.md) | What each CLI agent supports: streaming, resume, injection, sandbox |
| [10 MCP contract](docs/10-mcp-contract.md) | Tool contracts, the async progress/elicitation problem, three interaction types |
| [11 Domain model](docs/11-domain-model.md) | Vocabulary, entities, run state machine, capability semantics, package layout |
| [12 Spike results](docs/12-spike-results.md) | Observed vendor behaviour, and the eight assumptions it falsified |
| [13 Adapter authoring guide](docs/13-adapter-authoring.md) | How to write a `tier: basic` adapter in YAML, and what `tier: full` additionally requires |
| [14 Settings reference](docs/14-settings-reference.md) | Every configuration key: type, default, effect, and the rule that rejects a bad value |
| [`schema/config.schema.json`](schema/config.schema.json) | Normative config schema, plus the semantic rules JSON Schema cannot express |
| [SECURITY.md](SECURITY.md) | Security policy, claims, non-claims, disclosure |
| [CHANGELOG.md](CHANGELOG.md) | Notable changes, Keep a Changelog format |

## Requirements

Go 1.26+ to build (see `go.mod`). **v1.0 supports Linux and macOS**; Windows targets v1.1
(all six targets compile and are CI-built from the start, but the Windows binaries are
unsupported previews until that port has actually been executed). The target agents' own
CLIs, already logged in.

## Versioning and compatibility

Semantic versioning from `v1.0.0`. What the compatibility promise covers:

- the MCP tool surface: tool names, parameters, and the shape of what they return;
- the configuration file: keys, defaults, enum values, and the semantic rules that reject a
  config;
- adapter declaration keys and placeholder names;
- the operator verbs and their flags;
- the control-channel verbs and their wire records;
- audit record field names.

What it does not cover: every Go package under `internal/`, the normalised transcript event
shapes (they follow the vendors' own streams and change when those change), the TUI layout,
and the Windows binaries, which stay unsupported previews until the port has been executed
on Windows.

Removing or renaming anything in the covered list, or narrowing a default so an existing
config behaves differently, is a **major** release. A new optional setting whose default
preserves current behaviour is a **minor** one. The single exception is a security fix: a
default may be tightened in a minor release when leaving it alone would keep users exposed,
and the CHANGELOG says so explicitly under `Changed`.

## Licence

MIT, copyright Andrii Afanasiev — see [LICENSE](LICENSE). Every release archive carries it
(`.goreleaser.yaml`).
