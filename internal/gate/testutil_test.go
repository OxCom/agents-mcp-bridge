package gate

import (
	"os"
	"runtime"
	"testing"
)

// shortTempDir returns a fresh temp directory shorter than t.TempDir().
// t.TempDir() nests under TMPDIR, which on macOS is a long per-process
// /var/folders/<random>/T path (~45-50 bytes); once a gate socket name is
// appended that leaves too little headroom under the 104-byte unix socket
// sun_path limit, so tests that actually bind a socket need a shorter base.
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
