# Domain model

The vocabulary of this project, defined once. Every other document uses these terms in
exactly this sense; where an earlier draft used two words for one thing, this document is
the authority. A term that cannot be defined precisely here is a sign the design is still
confused, not a sign the glossary is incomplete.

## 1. Entities

```
Operator ──owns──► Config ──declares──► Adapter ──serves──► Tool  (one per adapter)
                                           │
                                           ▼
Host agent (A) ──calls Tool──► Run ──spawns──► Target agent (B)
                                │
                                ├──produces──► Event*  ──►  Transcript
                                ├──produces──► Result  ──►  Envelope ──► A
                                ├──may hold──► Question (awaits an Answer)
                                ├──may accept─► Steer
                                ├──belongs to─► Session  (identified by a Handle)
                                └──records───► Audit entry*
```

| Entity | Definition | Identity | Lifetime |
|---|---|---|---|
| **Operator** | The human. The only trusted principal. Owns the config and the runtime toggles. | OS user | — |
| **Host agent (A)** | The CLI agent whose MCP config contains this bridge. Untrusted: its tool arguments are attacker-influenceable. | `--host <agent_id>` | one server process |
| **Target agent (B)** | The CLI agent the bridge spawns. Untrusted and autonomous. | `agent_id` | one run |
| **Adapter** | The declaration of how to invoke, stream, resume and steer one target agent. | `agent_id` | config load |
| **Tool** | The MCP surface of exactly one adapter: `ask_<agent_id>`. | tool name | server lifetime |
| **Run** | One delegated execution. The central entity; everything else hangs off it. | `run_id` (bridge-issued) | spawn → terminal state → `run_retention_s` |
| **Session** | Conversation state on B's side, continued across runs. | **Handle** (bridge-issued, opaque) ↔ vendor session id (private) | until B discards it |
| **Event** | One normalised item from B's stream. | `(run_id, seq)` | transcript retention |
| **Question** | B blocking on something only a human should answer. | `(run_id, question_id)` | until answered or `question_timeout_s` |
| **Steer** | Guidance injected into a live run. Has an **origin**: operator or agent. | `(run_id, seq)` | instantaneous |
| **Result** | B's final output, sanitized and wrapped in an envelope. | `run_id` | with the run |
| **Transcript** | The full event stream on disk. Sensitive by construction. | `run_id` | age/size pruned |
| **Audit entry** | The security record of a run or a refusal. Never bodies by default. | append order | rotated |

## 2. The one naming rule

**A `Handle` is issued by the bridge. A vendor session id is private to the bridge.**

Every identifier crossing the MCP boundary to A is bridge-issued and opaque: `run_id`,
`session_handle`. Every vendor identifier — Codex thread id, Claude session id — lives
only in the bridge's state and in the argv it builds. There is no configuration that
relaxes this, because a vendor id from A is a session-takeover primitive
([`03`](03-threat-model.md) T16).

This is why the placeholder is `{{vendor_session_id}}` and why no `{{session_id}}` exists.

## 3. Run state machine

```
                    ┌──────────────► rejected  (policy refused before spawn)
                    │
  requested ──► admitted ──► running ──┬──► completed
                                       ├──► needs_input ──► superseded  (continued; carries resumed_by)
                                       ├──► failed        (vendor error, limit breach)
                                       └──► cancelled     (operator, agent, or timeout)
                                                │
  running ──► awaiting_acceptance ──► accepted ─┤   (write mode, worktree: required)
                    │                           │
                    └──► rejected_changes ──────┘
                                                ▼
                                            retained ──► reaped
```

Invariants, each of which is a test:

1. A run in any terminal state has no live child process.
2. A run in `awaiting_acceptance` has an intact worktree; a run past it has none.
3. `needs_input` is the only non-terminal state a run can rest in indefinitely — bounded
   by `question_timeout_s`, after which it becomes `failed`, freeing its concurrency slot.
   The rest period is measured from the moment the run *enters* `needs_input`, not from the
   original `Question.Deadline`: by the time a run reaches `needs_input`, that deadline has
   almost always already elapsed (every path into `needs_input` gets there by first waiting
   out that same deadline on the TUI or elicitation channel), so reusing it as-is would fail
   the run the instant `needs_input` began, with no rest period at all. Instead the question's
   own `Asked → Deadline` span is taken as a duration and re-anchored to `time.Now()` when
   `needs_input` begins. It is not terminal, but there
   is no VENDOR resume path: the child is killed and the pending question is cleared as
   part of the same transition, so no operator answer can ever reach the original blocked
   tool call. What CAN happen from here is a **continuation**: an ordinary new run,
   `Run.ResumedFrom` pointing back at this one, seeded with reconstructed context (original
   prompt, prior question/answer pairs) rather than a resumed vendor session — see
   [`02`](02-architecture.md) §2.3a. `Run.Supersede` transitions
   a `needs_input` run to `superseded` once its successor exists; only `needs_input` may
   become `superseded` (`ErrNotAwaitingInput` otherwise), and only `needs_input`'s own
   `ContinuationRecord` — attached at admission via `Spec.Continuation`, consumed exactly
   once by `TakeContinuation` — makes a continuation possible at all: it is never persisted,
   so it does not survive a bridge restart, and a run whose record is gone (already
   consumed, or never attached — an unconfined write run refuses continuation entirely)
   returns `ErrContinuationUnavailable` rather than a silent no-op. `defaults.max_continuations`
   (default 3) bounds how many links a chain may have; `ContinuationRecord.CheckDepth`
   refuses with `ErrChainTooDeep` past that ceiling. `Ask` itself refuses a second question
   while a run rests here (`ErrRunNotLive`,
   `internal/run/question.go`): a `needs_input` run has no live child, so admitting a new
   question would let the deadline expiry below flip the run to `failed` while that
   question's verdict channel sat unresolved, stranding whatever goroutine called `Ask`.
   Expiry past the deadline resolves any pending question defensively rather than stranding
   it — belt and suspenders, since the `Ask` refusal above means production code can no
   longer leave one pending here. It is settled against a later `finish()`: the first
   settled state wins, so a killed child's belated exit cannot flip `needs_input` back to
   `cancelled`. The question that caused it survives for the caller after the child is gone
   — the run's snapshot still reports it once nothing is pending.
4. A run never leaves `retained` before `run_retention_s`, so a late `await_agent` gets an
   answer rather than "unknown run".
5. Every transition is one audit entry. A run with no terminal audit entry is a fault. The
   transition into `needs_input` is audited as `question.timeout` on all three trigger
   paths: no operator channel was available, the operator's answer window elapsed, and the
   request was cancelled (`internal/gate/resolver.go`'s `ctx.Done()` path, whose detail
   string is suffixed `" cancelled"` to distinguish it from the other two). The other half
   of this transition — `needs_input` expiring into `failed` once `question_timeout_s`
   passes — has no request in flight to audit from, so it is reported separately: `Run`
   carries a `transition` callback wired once, at startup, via `Registry.OnTransition`
   (`cmd/bridge/serve.go` wires it to the audit writer before any run can start), and a
   deadline check invokes it as `run.expired` with the expiry reason. `OnTransition` must
   be called before the registry's first `Start`: calling it after panics, rather than
   silently leaving that already-started run's expiry unaudited. The callback itself
   always runs with no registry or run lock held — `Run.expireIfDue()` mutates state under
   `r.mu` alone, then dispatches after releasing it, and `Registry.sweepExpired()` (called
   at the top of every `Start`, `Get` and `List`) copies run pointers under `reg.mu`,
   releases it, then calls `expireIfDue` on each — so the audit disk write on this path
   never blocks a registry-wide operation. A run that only ever sits in `needs_input` and
   expires still gets this one entry, so invariant 5 holds for that path too.
6. `worktree: off` skips `awaiting_acceptance` entirely: `running → completed`.
7. `superseded` IS terminal (`State.IsTerminal`), unlike `needs_input`: a continued run
   frees its `max_concurrent_runs` slot immediately and becomes prunable on the normal
   retention clock rather than resting until `question_timeout_s`. `Run.Supersede` emits
   exactly one audit entry, `run.superseded`, carrying the successor's run id as its
   detail — the same single-entry-per-transition discipline as invariant 5.

## 4. Capability semantics

Capabilities are **independent**, and one never implies another. This matters because the
natural-language shorthand "supports interaction" hides three different vendor features:

| Capability | Means precisely | Does not mean |
|---|---|---|
| `stream` | vendor emits parseable structured events | that any of them are questions |
| `resume` | a prior session can be continued by id | that state is shared live |
| `steer` | unsolicited text can be delivered into a **running process** — `true` mid-turn, `queued` at the turn boundary of a live child | that B will obey it, that it can answer a pending question, or that a *next-run* mailbox counts. Codex's `queue` writes to a durable mailbox consumed by a later run, so Codex is `steer: false` ([`12`](12-spike-results.md) S3) |
| `interactive` | B emits a correlated question the bridge can detect and route to a human | permission approval, which is a separate protocol the bridge does not implement in v1 |
| `structured_output` | a schema can be imposed on the final result | anything about the event stream |

Dependency: `interactive` requires `stream` — you cannot detect a question in a stream you
cannot parse. That is the only dependency, and it is enforced at load.

## 5. Policy vocabulary

| Term | Meaning | Not to be confused with |
|---|---|---|
| **mode** | `read-only` or `write` — which sandbox flag list is emitted | confinement |
| **confinement** | `worktree` or `none` — whether a write run's changes are staged or land directly | mode |
| **sandbox_enforced** | whether the vendor offers *any* enforcement for that mode | either of the above |
| **tier** | `basic` (YAML only) or `full` (YAML + a Go adapter) | capabilities, which tier constrains |
| **ceiling** | a `defaults` key that caps what any adapter may declare | a per-adapter setting |

Precedence, stated once and never restated differently elsewhere:

```
config ceiling  ≥  adapter declaration  ≥  runtime toggle  ≥  per-call parameter
```

Each step may only narrow. No tool parameter reaches policy at all; the only per-call
field that touches it is `sandbox`, which can narrow `write` to `read-only` and never the
reverse.

## 6. Trust vocabulary

Used consistently in the threat model and nowhere loosely:

- **Trusted** — the operator, and the bridge's own code and config. Nothing else.
- **Untrusted** — A, B, the repository, B's output, A's tool arguments, comment and issue
  text anything reads.
- **Confined** — bounded blast radius (a worktree, an allowed root). Achievable here.
- **Contained** — cannot escape at all. Requires OS isolation; **not claimed anywhere**.
- **Enforced** — a control the bridge itself applies and can test.
- **Declared** — a statement the bridge passes to a vendor and cannot verify.

A claim in SECURITY.md may use *enforced* only where a conformance test asserts the
behaviour. Everything else is *declared*.

## 7. Package layout

The Go packages mirror the entities above, so a reader who knows the domain knows where
code lives. No package depends on one to its right.

```
cmd/bridge            serve | watch | control verbs
internal/config       load, JSON-Schema validation, semantic rules, ceilings
internal/adapter      registry, tiers, VendorStream interface, per-vendor implementations
internal/policy       the single decision point: roots, mode, confinement, depth, limits
internal/run          run lifecycle, state machine, process group / job, worktree
internal/stream       vendor event normalisation, caps, UTF-8 validation
internal/sanitize     control-char stripping, envelope, redaction
internal/audit        JSONL, keyed digests, rotation
internal/gate         the delegated agent's only address; carries one blocking question and its verdict, never the operator's control socket
internal/control      unix socket / named pipe, peer check, verbs
internal/tui          bubbletea watch UI
internal/platform     Paths, PathGuard, ProcessGroup, ControlEndpoint (the four OS seams)
```

`internal/policy` imports none of `run`, `stream`, `adapter`. It is a pure function of
(config, toggles, request), which is what makes every refusal testable without spawning
anything.
