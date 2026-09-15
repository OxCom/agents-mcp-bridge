//go:build !windows

package platform

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

type posixGroup struct{ attached *exec.Cmd }

// NewProcessGroup returns a POSIX process-group binding.
//
// Honest limitation, recorded in docs/01-requirements.md NFR-6: this kills the
// whole group when the bridge asks it to, but it gives NO guarantee once the
// bridge itself is SIGKILLed, because nothing then remains to signal the group,
// and a child may setsid() out of it. Linux PR_SET_PDEATHSIG and cgroups, and
// the Windows job object, are the mechanisms that survive that; macOS is
// best-effort by design.
func NewProcessGroup() ProcessGroup { return &posixGroup{} }

func (g *posixGroup) Attach(cmd *exec.Cmd) error {
	if cmd.Process != nil {
		return errors.New("Attach called after Start: the child may already have spawned descendants")
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0 // new group, leader is the child
	g.attached = cmd
	return nil
}

func (g *posixGroup) KillAll() error {
	if g.attached == nil || g.attached.Process == nil {
		return nil // never started; nothing to reap
	}
	pgid, err := syscall.Getpgid(g.attached.Process.Pid)
	if err != nil {
		// The child is already gone; its group died with it.
		return nil
	}
	// Signal the GROUP (negative pid), not the leader. Signalling the leader
	// alone leaves its siblings and children running.
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill process group %d: %w", pgid, err)
	}
	return nil
}
