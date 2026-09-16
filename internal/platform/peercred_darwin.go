//go:build darwin

package platform

import "golang.org/x/sys/unix"

// macOS spells it LOCAL_PEERCRED, and the option value is a struct xucred, not
// an int: reading it with GetsockoptInt returns the leading cr_version field,
// which is 0 on every connection, so every peer looked like uid 0 and the check
// rejected the operator's own socket. GetsockoptXucred reads the whole struct.
func peerUIDOf(fd uintptr) (uint32, error) {
	cred, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, err
	}
	return cred.Uid, nil
}
