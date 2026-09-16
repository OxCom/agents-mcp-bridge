// Package policy is the single decision point: may this delegation happen, in
// this directory, at this depth, under this mode?
//
// It imports no other internal package on purpose. Every decision is a pure
// function of (config, toggles, request), which is what makes every refusal
// testable without spawning a process. See docs/11-domain-model.md §7.
package policy

import (
	"fmt"
	"strings"

	"github.com/oxcom/agents-mcp-bridge/internal/config"
	"github.com/oxcom/agents-mcp-bridge/internal/platform"
)

// Reason names why a request was refused. It is a closed set so the audit log
// and the MCP error surface agree on vocabulary.
type Reason string

// The closed set of refusal reasons. Adding one obliges the audit log and the MCP
// error surface to carry the same string.
const (
	ReasonUnknownAgent      Reason = "unknown_agent"
	ReasonSelfCall          Reason = "self_call"
	ReasonRootViolation     Reason = "root_violation"
	ReasonDepthExceeded     Reason = "depth_exceeded"
	ReasonFeatureDisabled   Reason = "capability_disabled"
	ReasonPromptTooLarge    Reason = "prompt_too_large"
	ReasonSandboxNotAllowed Reason = "sandbox_not_allowed"
	ReasonModelNotAllowed   Reason = "model_not_allowed"
)

// Refusal is a policy denial. It carries a reason for machines and a message
// for humans, and never leaks internal detail such as the list of allowed roots.
type Refusal struct {
	Reason  Reason
	Message string
}

func (r *Refusal) Error() string { return fmt.Sprintf("%s: %s", r.Reason, r.Message) }

func refuse(reason Reason, format string, args ...any) *Refusal {
	return &Refusal{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// Request is what a caller asked for. Every field is attacker-influenceable:
// these are tool arguments from an untrusted model.
type Request struct {
	AgentID string
	Prompt  string
	CWD     string // may be empty; then the first allowed root is used
	Model   string
	// Sandbox may narrow a write-mode adapter to read-only. It can never widen.
	Sandbox string
}

// Decision is an approved request, resolved to the concrete values the run
// manager needs. Nothing downstream re-derives policy from the Request.
type Decision struct {
	Adapter         *config.Adapter
	CWD             string // canonical, inside an allowed root
	Mode            config.Mode
	Confined        bool // false only for worktree: off
	SandboxEnforced bool
	Model           string
}

// Engine answers policy questions for one server instance.
type Engine struct {
	cfg   *config.Config
	guard platform.PathGuard
	host  string
	depth int

	roots   []string // canonical, resolved once at construction
	toggles *Toggles
	rate    *rateLimiter
}

// New resolves the allowed roots up front so a request never pays for it, and
// so a root that does not exist is a startup failure rather than a per-call
// surprise.
func New(cfg *config.Config, guard platform.PathGuard, host string, depth int) (*Engine, error) {
	e := &Engine{
		cfg: cfg, guard: guard, host: host, depth: depth,
		toggles: NewToggles(),
		rate:    newRateLimiter(cfg.Defaults.RatePerMinute),
	}
	for _, r := range cfg.AllowedRoots {
		canonical, err := guard.Canonicalise(r)
		if err != nil {
			return nil, fmt.Errorf("allowed_roots %q: %w", r, err)
		}
		e.roots = append(e.roots, canonical)
	}
	if len(e.roots) == 0 {
		return nil, fmt.Errorf("no usable allowed_roots")
	}
	return e, nil
}

// Depth reports the delegation depth this server is running at.
func (e *Engine) Depth() int { return e.depth }

// Exhausted reports whether this server is at or beyond the configured depth,
// in which case it exposes no delegation tools at all.
func (e *Engine) Exhausted() bool { return e.depth >= e.cfg.Defaults.MaxDepthOrDefault() }

// Authorise decides a delegation request. The order matters: cheap structural
// checks first, filesystem work last.
func (e *Engine) Authorise(req Request) (*Decision, error) {
	if e.Exhausted() {
		// Neither the current depth nor the ceiling is disclosed: both describe
		// the operator's configuration, not the caller's request.
		return nil, refuse(ReasonDepthExceeded, "delegation is not available at this depth")
	}
	if req.AgentID == e.host {
		// Belt and braces: the tool should not exist at all.
		return nil, refuse(ReasonSelfCall, "an agent may not delegate to itself")
	}
	adapter, ok := e.cfg.Agents[req.AgentID]
	if !ok {
		return nil, refuse(ReasonUnknownAgent, "no such agent %q", req.AgentID)
	}
	if err := e.checkRate(); err != nil {
		return nil, err
	}
	if len(req.Prompt) > e.cfg.Defaults.MaxPromptBytes {
		return nil, refuse(ReasonPromptTooLarge, "prompt exceeds the configured limit")
	}

	mode, err := e.resolveMode(adapter, req.Sandbox)
	if err != nil {
		return nil, err
	}
	model, err := e.resolveModel(adapter, req.Model)
	if err != nil {
		return nil, err
	}
	cwd, err := e.resolveCWD(req.CWD)
	if err != nil {
		return nil, err
	}

	return &Decision{
		Adapter:         adapter,
		CWD:             cwd,
		Mode:            mode,
		Confined:        adapter.Worktree != config.WorktreeOff,
		SandboxEnforced: adapter.SandboxEnforcedOrDefault(),
		Model:           model,
	}, nil
}

// resolveMode applies the one per-call parameter that touches policy. It may
// only narrow: a read-only adapter can never be asked to write.
func (e *Engine) resolveMode(a *config.Adapter, requested string) (config.Mode, error) {
	if requested == "" {
		return a.Mode, nil
	}
	switch config.Mode(requested) {
	case config.ModeReadOnly:
		return config.ModeReadOnly, nil // always a narrowing
	case config.ModeWrite:
		if a.Mode != config.ModeWrite {
			return "", refuse(ReasonSandboxNotAllowed,
				"agent %q is configured read-only; a call cannot widen it", a.ID)
		}
		return config.ModeWrite, nil
	default:
		return "", refuse(ReasonSandboxNotAllowed, "unknown sandbox %q", requested)
	}
}

func (e *Engine) resolveModel(a *config.Adapter, requested string) (string, error) {
	if requested == "" {
		return "", nil
	}
	if len(a.AllowedModels) == 0 {
		return "", refuse(ReasonModelNotAllowed,
			"agent %q does not accept a model override", a.ID)
	}
	for _, m := range a.AllowedModels {
		if m == requested {
			return requested, nil
		}
	}
	return "", refuse(ReasonModelNotAllowed, "model %q is not permitted for agent %q", requested, a.ID)
}

// resolveCWD canonicalises the requested directory and contains it. An omitted
// cwd becomes the FIRST ALLOWED ROOT, never the bridge's inherited working
// directory, which is the host's cwd and may lie outside every root.
func (e *Engine) resolveCWD(requested string) (string, error) {
	if strings.TrimSpace(requested) == "" {
		return e.roots[0], nil
	}
	canonical, err := e.guard.Canonicalise(requested)
	if err != nil {
		// The error is deliberately vague to the caller: whether a path exists
		// is information an untrusted model does not need.
		return "", refuse(ReasonRootViolation, "working directory is not usable")
	}
	for _, root := range e.roots {
		if e.guard.Contains(root, canonical) {
			return canonical, nil
		}
	}
	return "", refuse(ReasonRootViolation, "working directory is outside the allowed roots")
}

// FeatureEnabled reports whether a named global feature is on. Adapters may
// narrow these; nothing may widen them.
// FeatureEnabled applies the precedence rule once, in one place:
//
//	config ceiling  ≥  runtime toggle
//
// A toggle can only narrow. If the config forbids a feature, no runtime action
// turns it on.
func (e *Engine) FeatureEnabled(name string) bool {
	if !e.cfg.Features.Enabled(name) {
		return false
	}
	return e.toggles == nil || !e.toggles.DisabledAtRuntime(name)
}

// Toggles exposes the operator's switch set.
func (e *Engine) Toggles() *Toggles { return e.toggles }
