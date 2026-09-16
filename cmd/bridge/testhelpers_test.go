package main

import (
	"os"
	"runtime"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpTextContent = mcp.TextContent

// shortTempDir returns a fresh temp directory shorter than t.TempDir().
// t.TempDir() nests under TMPDIR, which on macOS is a long per-process
// /var/folders/<random>/T path (~45-50 bytes); once a control or gate socket
// name is appended that leaves too little headroom under the 104-byte unix
// socket sun_path limit, so tests that actually bind a socket need a
// shorter base.
func shortTempDir(t *testing.T) string {
	t.Helper()
	base := os.TempDir()
	if runtime.GOOS == "darwin" {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "b")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// stubArgs prefixes the stub-agent verb, so a test spawn of the test binary
// (self) lands in runStubAgent rather than in a vendor CLI. It replaces
// /bin/cat, /bin/echo and /bin/sleep, none of which exist on Windows:
// stubagent.go echoes stdin and argv as one JSON line and honours --sleep and
// --exit, which is everything these tests asked those binaries for.
func stubArgs(args ...string) []string {
	return append([]string{stubAgentVerb}, args...)
}
