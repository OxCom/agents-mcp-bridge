---
title: Documentation
---

<section class="docs-hero">
  <p class="docs-eyebrow">Delegation as a trust boundary</p>
  <h1>Safely, visibly, steerably.</h1>
  <p class="docs-lede">
    One MCP server that lets any CLI coding agent delegate work to any other
    CLI coding agent.
  </p>
  <div class="docs-actions">
    <a class="docs-action docs-action-primary" href="13-adapter-authoring.html">Write an adapter</a>
    <a class="docs-action" href="02-architecture.html">Explore the architecture</a>
  </div>
</section>

## Documentation

<nav class="docs-grid" aria-label="Documentation sections">
  <a class="docs-card" href="13-adapter-authoring.html">
    <span class="docs-card-label">Start here</span>
    <strong>Adapter authoring guide</strong>
    <span>How to add a new delegated agent: a task-oriented companion to the normative configuration reference.</span>
  </a>
  <a class="docs-card" href="04-config-schema.html">
    <span class="docs-card-label">Reference</span>
    <strong>Configuration</strong>
    <span>Where the file lives, how to register the bridge in a host agent, worked config files, placeholder binding and the operator commands.</span>
  </a>
  <a class="docs-card" href="14-settings-reference.html">
    <span class="docs-card-label">Reference</span>
    <strong>Settings reference</strong>
    <span>Every key the bridge accepts, with its type, default, effect, and the rule that rejects a bad value.</span>
  </a>
  <a class="docs-card" href="10-mcp-contract.html">
    <span class="docs-card-label">Reference</span>
    <strong>MCP contract</strong>
    <span>What each tool returns, when progress and elicitation are legal, and what the host is and is not expected to do.</span>
  </a>
  <a class="docs-card" href="07-agent-capability-matrix.html">
    <span class="docs-card-label">Reference</span>
    <strong>Agent capability matrix</strong>
    <span>What each CLI agent offers a bridge that must drive it headlessly, stream its activity, resume it, steer it mid-run, and sandbox it.</span>
  </a>
  <a class="docs-card" href="02-architecture.html">
    <span class="docs-card-label">Concepts</span>
    <strong>Architecture</strong>
    <span>One Go binary, with its roles selected by argv: MCP server, operator TUI and control CLI.</span>
  </a>
  <a class="docs-card" href="11-domain-model.html">
    <span class="docs-card-label">Concepts</span>
    <strong>Domain model</strong>
    <span>The vocabulary of this project, defined once. Every other document uses these terms in exactly this sense.</span>
  </a>
  <a class="docs-card" href="03-threat-model.html">
    <span class="docs-card-label">Concepts</span>
    <strong>Threat model</strong>
    <span>A delegation bridge sits on a boundary where an untrusted text channel becomes an agent with filesystem and shell authority.</span>
  </a>
  <a class="docs-card" href="12-spike-results.html">
    <span class="docs-card-label">Evidence</span>
    <strong>Vendor behaviour spikes</strong>
    <span>Behaviour observed against pinned vendor CLIs, not documentation, and the assumptions it falsified.</span>
  </a>
  <a class="docs-card" href="01-requirements.html">
    <span class="docs-card-label">Evidence</span>
    <strong>Requirements</strong>
    <span>What the bridge must do, and how far it is built: implemented through Phase 5, plus Phase 3 interactive mode.</span>
  </a>
  <a class="docs-card" href="security.html">
    <span class="docs-card-label">Security</span>
    <strong>Security policy</strong>
    <span>How to report a vulnerability, and which properties are enforced by a test versus declared by design.</span>
  </a>
</nav>

## What makes it different

Every existing bridge is a subprocess wrapper. This one treats delegation as a trust
boundary and as a supervised activity.

- **Config-declared agents, two tiers.** A basic agent is YAML and no rebuild; a full
  agent adds a Go adapter for streaming, sessions, steering and interactivity.
- **Live watch.** `bridge watch` renders the delegated agent's tool calls, file edits and
  shell commands as they happen, in a second terminal.
- **Interactive.** When the delegated agent asks a question, you answer it. Never
  guessed, never silently defaulted. Shipped for Claude Code only.
- **Mid-run steering.** Type extra guidance into a running agent through its vendor's own
  channel.
- **Self-call exclusion.** Claude cannot delegate to Claude; Codex cannot delegate to Codex.
- **Write runs are confined by default.** A write-mode run happens in a disposable git
  worktree and returns a diff; nothing reaches your tree without an explicit acceptance step.
- **Secure by construction.** Read-only default, working-directory allowlist, no shell,
  no credential parsing, untrusted-output envelope, audit log, no network listener.

[View the source on GitHub](https://github.com/OxCom/agents-mcp-bridge)
