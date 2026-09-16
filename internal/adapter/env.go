package adapter

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// baselineEnv is what any CLI needs to run at all. It is built in rather than
// configured, because an operator who had to list it would eventually paste a
// broader list and defeat the point.
//
// Both lists are allowlists: a name absent here and absent from the operator's
// env_allowlist does not reach the child (SR-1).
func baselineEnv() []string {
	if runtime.GOOS == "windows" {
		return []string{
			"SystemRoot", "SystemDrive", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
			"PATH", "PATHEXT", "TEMP", "TMP", "COMSPEC", "NUMBER_OF_PROCESSORS", "OS",
		}
	}
	return []string{"HOME", "PATH", "LANG", "LC_ALL", "TERM", "USER", "LOGNAME", "SHELL", "TMPDIR"}
}

// bridgeEnvPrefix marks our own state. It is compared through envKey, so a
// parent spelling it in another case cannot smuggle it past on Windows.
const bridgeEnvPrefix = "AGENTS_BRIDGE_"

// envKey normalizes an environment variable name for comparison.
//
// Windows treats these names case-insensitively, and a real Windows process
// block spells them "Path", "ComSpec", "Temp", "windir" — never the upper-case
// forms a POSIX-shaped allowlist is written in. Comparing case-sensitively
// there drops PATH from the baseline and the child cannot resolve its own
// executables. POSIX names are case-sensitive and stay exact: HOME and home are
// two variables, and collapsing them would widen the allowlist.
func envKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

// DepthEnvVar marks how deep a delegation chain has gone.
//
// This is BEST-EFFORT, not a control. Codex is verified not to propagate its
// environment to MCP servers it spawns, so a nested bridge may never see it
// (docs/12-spike-results.md S6). The enforced recursion control is vendor config
// isolation, which stops the child loading a nested bridge at all.
const DepthEnvVar = "AGENTS_BRIDGE_DEPTH"

// RunIDEnvVar is deliberately NOT passed to the child. Handing the agent its own
// run id, together with the runtime directory, is what would let it answer its
// own questions over the control channel (docs/03-threat-model.md T10a).
const RunIDEnvVar = "AGENTS_BRIDGE_RUN_ID"

// BuildEnv produces the child's environment: deny by default, baseline plus an
// operator-chosen allowlist, plus the depth marker.
//
// Neither the runtime directory nor the run id is forwarded, so the child has no
// address for the operator's control socket.
func BuildEnv(allowlist []string, depth int) []string {
	keep := make(map[string]bool, len(allowlist)+16)
	for _, k := range baselineEnv() {
		keep[envKey(k)] = true
	}
	for _, k := range allowlist {
		keep[envKey(k)] = true
	}

	out := make([]string, 0, len(keep)+1)
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		name := envKey(kv[:eq])
		if strings.HasPrefix(name, bridgeEnvPrefix) {
			continue // never leak our own state into the child
		}
		if keep[name] {
			out = append(out, kv)
		}
	}
	out = append(out, fmt.Sprintf("%s=%d", DepthEnvVar, depth+1))
	return out
}

// DepthFromEnv reads the marker a parent bridge set. A missing or unparsable
// value means depth zero: the marker is best-effort, so its absence must mean
// "no evidence of nesting", not "refuse to run".
func DepthFromEnv() int {
	v := os.Getenv(DepthEnvVar)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
