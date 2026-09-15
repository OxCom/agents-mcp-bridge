//go:build !windows

package platform

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestKillAllReapsGrandchild(t *testing.T) {
	g := NewProcessGroup()
	// A shell that spawns a long-lived grandchild and then waits. Killing only
	// the leader would leave the grandchild running.
	cmd := exec.Command("sh", "-c", "sleep 300 & echo $!; wait")
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
	if n == 0 {
		t.Fatal("no grandchild pid reported")
	}
	var gpid int
	if _, err := fmtSscan(string(buf[:n]), &gpid); err != nil || gpid <= 0 {
		t.Fatalf("unparsable grandchild pid %q", string(buf[:n]))
	}

	if err := g.KillAll(); err != nil {
		t.Fatalf("KillAll: %v", err)
	}
	_ = cmd.Wait()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(gpid, 0); err != nil {
			return // grandchild is gone: the group was killed, not just the leader
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(gpid, syscall.SIGKILL)
	t.Fatalf("grandchild %d survived KillAll", gpid)
}

func TestAttachAfterStartIsRefused(t *testing.T) {
	g := NewProcessGroup()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	if err := g.Attach(cmd); err == nil {
		t.Fatal("Attach after Start must be refused: the child may already have descendants")
	}
}

func TestKillAllOnUnstartedGroupIsNoError(t *testing.T) {
	if err := NewProcessGroup().KillAll(); err != nil {
		t.Fatalf("KillAll on an unstarted group: %v", err)
	}
}

func fmtSscan(s string, v *int) (int, error) { return sscan(s, v) }
