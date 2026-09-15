// Package adapter turns a validated configuration into the concrete argv and
// environment a target agent is spawned with.
//
// The whole point of this package is that a prompt can never become an
// argument. Placeholders bind as whole argv elements; there is no shell, no
// string concatenation into a command line, and no format string that a caller
// influences. See docs/01-requirements.md SR-4.
package adapter

import (
	"fmt"
	"strings"

	"github.com/oxcom/agents-mcp-bridge/internal/config"
)

// Values are the substitutions for one invocation.
type Values struct {
	Prompt            string
	CWD               string
	VendorSessionID   string
	Message           string
	Model             string
	MaxTurns          string
	SandboxFlags      []string
	ResumeSandboxFlgs []string
	GateFlags         []string
	GateConfig        string
}

// BuildArgs expands a template into argv.
//
// Two forms are supported and no others:
//
//	"{{placeholder}}"        the element IS the value
//	"--flag={{placeholder}}" one element, flag and value joined
//
// The second form exists because some CLIs misparse a value that begins with a
// dash (`codex queue --message -foo` fails). Anything else containing a
// placeholder is rejected: partial interpolation is how a value becomes two
// arguments.
func BuildArgs(template []string, v Values) ([]string, error) {
	out := make([]string, 0, len(template)+8)
	for i, tmpl := range template {
		switch {
		case !strings.Contains(tmpl, "{{"):
			out = append(out, tmpl)

		case isWholePlaceholder(tmpl):
			expanded, list, isList, err := expand(tmpl, v)
			if err != nil {
				return nil, fmt.Errorf("args[%d]: %w", i, err)
			}
			if isList {
				out = append(out, list...) // sandbox flag lists only
				continue
			}
			if expanded == "" && omitWhenEmpty(tmpl) {
				continue // an absent optional value contributes no argument
			}
			out = append(out, expanded)

		case isFlagBinding(tmpl):
			flag, ph := splitFlagBinding(tmpl)
			expanded, _, isList, err := expand(ph, v)
			if err != nil {
				return nil, fmt.Errorf("args[%d]: %w", i, err)
			}
			if isList {
				return nil, fmt.Errorf("args[%d]: %s expands to a list and cannot be bound to a flag", i, ph)
			}
			out = append(out, flag+"="+expanded)

		default:
			return nil, fmt.Errorf("args[%d]: %q interpolates a placeholder into a larger string; "+
				"use the whole element or the --flag={{placeholder}} form", i, tmpl)
		}
	}
	return out, nil
}

func isWholePlaceholder(s string) bool {
	return strings.HasPrefix(s, "{{") && strings.HasSuffix(s, "}}") &&
		strings.Count(s, "{{") == 1 && strings.Count(s, "}}") == 1
}

func isFlagBinding(s string) bool {
	eq := strings.Index(s, "=")
	if eq <= 0 || !strings.HasPrefix(s, "-") {
		return false
	}
	return isWholePlaceholder(s[eq+1:]) && !strings.Contains(s[:eq], "{{")
}

func splitFlagBinding(s string) (flag, placeholder string) {
	eq := strings.Index(s, "=")
	return s[:eq], s[eq+1:]
}

func omitWhenEmpty(tmpl string) bool {
	switch tmpl {
	case "{{model}}", "{{max_turns}}", "{{vendor_session_id}}", "{{message}}", "{{gate_flags}}":
		return true
	default:
		return false
	}
}

func expand(tmpl string, v Values) (single string, list []string, isList bool, err error) {
	switch tmpl {
	case "{{prompt}}":
		return v.Prompt, nil, false, nil
	case "{{cwd}}":
		return v.CWD, nil, false, nil
	case "{{gate_config}}":
		return v.GateConfig, nil, false, nil
	case "{{vendor_session_id}}":
		return v.VendorSessionID, nil, false, nil
	case "{{message}}":
		return v.Message, nil, false, nil
	case "{{model}}":
		return v.Model, nil, false, nil
	case "{{max_turns}}":
		return v.MaxTurns, nil, false, nil
	case "{{sandbox_flags}}":
		return "", v.SandboxFlags, true, nil
	case "{{resume_sandbox_flags}}":
		return "", v.ResumeSandboxFlgs, true, nil
	case "{{gate_flags}}":
		return "", v.GateFlags, true, nil
	default:
		return "", nil, false, fmt.Errorf("unknown placeholder %s", tmpl)
	}
}

// SandboxFlags returns the flag list for a mode, or an error when the adapter
// claims enforcement it has not declared.
func SandboxFlags(a *config.Adapter, mode config.Mode) ([]string, error) {
	flags := a.Sandbox[string(mode)]
	if len(flags) == 0 && a.SandboxEnforcedOrDefault() {
		return nil, fmt.Errorf("adapter %s declares no sandbox flags for mode %s", a.ID, mode)
	}
	return flags, nil
}

// InteractiveFlags returns the EXPANDED gate flags for this mode: the
// adapter's interactive list with {{gate_config}} resolved to gateConfig,
// run through the same whole-element placeholder rules as invoke.args (a
// partial interpolation such as "--mcp-config={{gate_config}}x" is refused,
// exactly as it would be anywhere else). Empty/absent is legal and means the
// adapter is not interactive for this mode.
func InteractiveFlags(a *config.Adapter, mode config.Mode, gateConfig string) ([]string, error) {
	if a.Interactive == nil {
		return nil, nil
	}
	raw, ok := a.Interactive[string(mode)]
	if !ok || len(raw) == 0 {
		return nil, nil
	}
	expanded, err := BuildArgs(raw, Values{GateConfig: gateConfig})
	if err != nil {
		return nil, fmt.Errorf("adapter %s interactive.%s: %w", a.ID, mode, err)
	}
	return expanded, nil
}

// ResumeSandboxFlags returns the flags for the vendor's resume surface, which
// may differ from its run surface. Codex is the reason this exists: `codex exec
// resume` rejects --sandbox outright and silently runs WRITABLE without an
// equivalent setting. See docs/12-spike-results.md S2.
func ResumeSandboxFlags(a *config.Adapter, mode config.Mode) ([]string, error) {
	if len(a.ResumeSandbox) > 0 {
		flags := a.ResumeSandbox[string(mode)]
		if len(flags) == 0 && a.SandboxEnforcedOrDefault() {
			return nil, fmt.Errorf("adapter %s declares no resume_sandbox flags for mode %s", a.ID, mode)
		}
		return flags, nil
	}
	return SandboxFlags(a, mode)
}
