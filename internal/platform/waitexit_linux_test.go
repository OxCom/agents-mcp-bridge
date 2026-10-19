//go:build linux

package platform

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The leader exits and stays unreaped. KillAll must still reach a surviving
// grandchild through the reserved pgid, and WaitExit must not reap.
func TestKillAllAfterWaitExitKillsGrandchildBeforeReap(t *testing.T) {
	g := NewProcessGroup()
	ew, ok := g.(ExitWaiter)
	if !ok {
		t.Fatal("linux process group must implement ExitWaiter")
	}
	cmd := exec.Command("sh", "-c", "sleep 300 >/dev/null 2>&1 & echo $!")
	if err := g.Attach(cmd); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	buf := make([]byte, 32)
	n, _ := out.Read(buf)
	var gpid int
	if _, err := fmtSscan(string(buf[:n]), &gpid); err != nil || gpid <= 0 {
		t.Fatalf("unparsable grandchild pid %q", string(buf[:n]))
	}

	if err := ew.WaitExit(); err != nil {
		t.Fatalf("WaitExit: %v", err)
	}
	if err := syscall.Kill(gpid, 0); err != nil {
		t.Fatalf("grandchild should outlive the leader: %v", err)
	}
	if err := g.KillAll(); err != nil {
		t.Fatalf("KillAll after WaitExit: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("leader must still be reapable after WaitExit: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !pidRunning(gpid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(gpid, syscall.SIGKILL)
	t.Fatalf("grandchild %d survived KillAll", gpid)
}

// pidRunning reports whether pid is a running process. A zombie counts as dead:
// under a pid 1 that does not reap, a killed orphan stays a zombie for good.
func pidRunning(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(raw)
	if i := strings.LastIndexByte(s, ')'); i >= 0 {
		if f := strings.Fields(s[i+1:]); len(f) > 0 && f[0] == "Z" {
			return false
		}
	}
	return true
}
