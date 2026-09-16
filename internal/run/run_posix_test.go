//go:build !windows

// The process-group kill test is POSIX-only: it shells out to `sh -c` and
// probes the grandchild with signal 0, neither of which exists on Windows.
// The Windows job object is a different mechanism with its own coverage in
// internal/platform, so this file is tagged rather than the whole suite.
package run

import (
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/platform"
)

func TestTimeoutKillsTheWholeGroup(t *testing.T) {
	reg := NewRegistry(4, time.Hour)
	sp := spec(t, "sh", "-c", "sleep 300 & echo $!; wait")
	sp.Timeout = 300 * time.Millisecond
	r, err := reg.Start(sp, platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	s := r.Await(10 * time.Second)
	if s.State != StateFailed || !strings.Contains(s.Failure, "timed out") {
		t.Fatalf("state = %q failure = %q", s.State, s.Failure)
	}

	var gpid int
	if _, err := sscanInt(strings.TrimSpace(s.Output), &gpid); err != nil || gpid <= 0 {
		t.Skipf("grandchild pid not reported: %q", s.Output)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(gpid, 0); err != nil {
			return // the grandchild died with the group
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(gpid, syscall.SIGKILL)
	t.Fatal("a timeout left a grandchild running")
}
