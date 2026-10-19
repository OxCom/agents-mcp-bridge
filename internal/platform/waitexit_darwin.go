//go:build darwin

package platform

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// WaitExit satisfies ExitWaiter. An EVFILT_PROC filter with NOTE_EXIT reports
// the exit without reaping, so the leader stays an unreaped zombie and keeps
// its pid and pgid reserved until the caller runs cmd.Wait.
func (g *posixGroup) WaitExit() error {
	if g.attached == nil || g.attached.Process == nil {
		return errors.New("WaitExit called before Start")
	}
	pid := g.attached.Process.Pid

	kq, err := unix.Kqueue()
	if err != nil {
		return fmt.Errorf("kqueue: %w", err)
	}
	defer func() { _ = unix.Close(kq) }()

	change := unix.Kevent_t{
		Ident:  uint64(pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}
	var events [1]unix.Kevent_t
	for {
		n, err := unix.Kevent(kq, []unix.Kevent_t{change}, events[:], nil)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue // re-registering the same filter is idempotent
			}
			if errors.Is(err, unix.ESRCH) {
				return nil
			}
			return fmt.Errorf("kevent %d: %w", pid, err)
		}
		if n == 0 {
			continue
		}
		ev := events[0]
		if ev.Flags&unix.EV_ERROR != 0 {
			// A registration failure arrives as an event whose Data is the
			// errno. The leader is this process's own child, so ESRCH can only
			// mean it has already exited (an unreaped zombie, or gone): that is
			// the state WaitExit exists to observe.
			errno := unix.Errno(ev.Data)
			if errno == unix.ESRCH {
				return nil
			}
			return fmt.Errorf("kevent %d: %w", pid, errno)
		}
		if ev.Fflags&unix.NOTE_EXIT != 0 {
			return nil
		}
	}
}
