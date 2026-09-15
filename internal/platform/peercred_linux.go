//go:build linux

package platform

import "golang.org/x/sys/unix"

func peerUIDOf(fd uintptr) (uint32, error) {
	cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return cred.Uid, nil
}
