//go:build darwin

package platform

import "golang.org/x/sys/unix"

// macOS spells it LOCAL_PEERCRED and returns the uid directly.
func peerUIDOf(fd uintptr) (uint32, error) {
	uid, err := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, err
	}
	return uint32(uid), nil
}
