package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureTranscripts copies a failed run's transcripts out of the test's temp
// directory, which Go removes before the failure can be inspected. A vendor
// transcript is the only record of what the child actually did — the raw event
// stream, including tool calls the bridge does not surface — so a conformance
// failure against a real CLI is otherwise unfalsifiable guesswork.
//
// Off unless BRIDGE_CONFORMANCE_ARTIFACTS names a directory. The transcripts
// are raw vendor output: they belong in a scratch directory the operator
// chooses, never in the repository.
func captureTranscripts(t *testing.T, stateHome string) {
	t.Helper()
	dest := os.Getenv("BRIDGE_CONFORMANCE_ARTIFACTS")
	if dest == "" || !t.Failed() {
		return
	}
	src := filepath.Join(stateHome, "agents-bridge", "transcripts")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Logf("capture: %v", err)
		return
	}
	outDir := filepath.Join(dest, strings.ReplaceAll(t.Name(), "/", "_"))
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Logf("capture: %v", err)
		return
	}
	for _, e := range entries {
		body, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Logf("capture %s: %v", e.Name(), err)
			continue
		}
		if err := os.WriteFile(filepath.Join(outDir, e.Name()), body, 0o600); err != nil {
			t.Logf("capture %s: %v", e.Name(), err)
			continue
		}
		t.Logf("captured transcript %s", filepath.Join(outDir, e.Name()))
	}
}
