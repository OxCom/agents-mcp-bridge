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
// The Windows entries are present even though Windows support lands in v1.1:
// leaving them out would break Node-based CLIs the moment the port arrives, and
// the list is a fact about the platform, not about the schedule.
func baselineEnv() []string {
	if runtime.GOOS == "windows" {
		return []string{
			"SystemRoot", "SystemDrive", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
			"PATH", "PATHEXT", "TEMP", "TMP", "COMSPEC", "NUMBER_OF_PROCESSORS", "OS",
		}
	}
	return []string{"HOME", "PATH", "LANG", "LC_ALL", "TERM", "USER", "LOGNAME", "SHELL", "TMPDIR"}
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
		keep[k] = true
	}
	for _, k := range allowlist {
		keep[k] = true
	}

	out := make([]string, 0, len(keep)+1)
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		name := kv[:eq]
		if strings.HasPrefix(name, "AGENTS_BRIDGE_") {
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
