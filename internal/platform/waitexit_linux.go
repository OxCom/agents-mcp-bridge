//go:build linux

package platform

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// WaitExit satisfies ExitWaiter. WNOWAIT leaves the leader an unreaped zombie,
// which keeps its pid and pgid reserved so KillAll cannot hit a recycled group.
func (g *posixGroup) WaitExit() error {
	if g.attached == nil || g.attached.Process == nil {
		return errors.New("WaitExit called before Start")
	}
	pid := g.attached.Process.Pid
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err == nil {
			return nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return fmt.Errorf("waitid %d: %w", pid, err)
	}
}
