package config

import (
	"fmt"
	"os"
)

// HostEvidence is what the environment says about which agent launched us.
type HostEvidence struct {
	Detected string // agent id inferred from the environment, "" when unknown
	Reason   string // which signal produced it
}

// hostSignals maps an environment variable to the agent it identifies. Process
// ancestry is deliberately not consulted: wrappers, shims, tmux and npx all
// break it, and a wrong guess is worse than no guess.
var hostSignals = []struct {
	env, agent string
}{
	{"CLAUDECODE", "claude"},
	{"CLAUDE_CODE_ENTRYPOINT", "claude"},
	{"CODEX_HOME", "codex"},
	{"CODEX_SANDBOX", "codex"},
}

// DetectHost inspects the environment for a known host agent.
func DetectHost() HostEvidence {
	for _, s := range hostSignals {
		if os.Getenv(s.env) != "" {
			return HostEvidence{Detected: s.agent, Reason: s.env + " is set"}
		}
	}
	return HostEvidence{}
}

// VerifyHost implements FR-6.2. Positive contradictory evidence is fatal: a
// config block copied into the wrong agent would otherwise let it delegate to
// itself. Absence of evidence is not fatal, because MCP Inspector, `bridge
// doctor`, tests and unusual launchers legitimately provide none.
//
// It returns a warning string when the host could not be confirmed.
func VerifyHost(declared string, ev HostEvidence) (warning string, err error) {
	if declared == "" {
		return "", fmt.Errorf("--host is required: without it the bridge cannot exclude the host's own adapter")
	}
	switch {
	case ev.Detected == "":
		return fmt.Sprintf("host %q could not be confirmed from the environment; continuing, but a wrong --host would let this agent delegate to itself", declared), nil
	case ev.Detected != declared:
		return "", fmt.Errorf("--host %s contradicts the environment, which says %s (%s); refusing to start",
			declared, ev.Detected, ev.Reason)
	default:
		return "", nil
	}
}
