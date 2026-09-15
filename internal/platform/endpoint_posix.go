//go:build !windows

package platform

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// sunPathMax is the portable limit for a unix socket path. Linux allows 108,
// macOS 104; the smaller is used so one path works on both.
const sunPathMax = 104

type posixEndpoint struct{}

// NewControlEndpoint returns the POSIX control endpoint: a unix domain socket,
// mode 0600, inside an owner-only directory, with the peer's UID checked on
// every connection.
//
// This authenticates the Unix ACCOUNT, not the operator. Agent B runs as the
// same user; see docs/03-threat-model.md T10a.
func NewControlEndpoint() ControlEndpoint { return posixEndpoint{} }

func (posixEndpoint) Listen(name string) (net.Listener, error) {
	if len(name) > sunPathMax {
		return nil, fmt.Errorf("socket path %d bytes, exceeds the %d-byte portable limit: %s", len(name), sunPathMax, name)
	}
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	// A stale socket from a crashed server would otherwise make Listen fail.
	if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("clear stale socket: %w", err)
	}
	// Create with a restrictive umask so there is no window in which the socket
	// is world-accessible between bind and chmod.
	old := unix.Umask(0o177)
	ln, err := net.Listen("unix", name)
	unix.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", name, err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("restrict socket: %w", err)
	}
	return ln, nil
}

func (posixEndpoint) VerifyPeer(conn net.Conn) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("control connection is %T, not a unix socket", conn)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return fmt.Errorf("peer credentials unavailable: %w", err)
	}
	var (
		peerUID uint32
		credErr error
	)
	if err := raw.Control(func(fd uintptr) {
		peerUID, credErr = peerUIDOf(fd)
	}); err != nil {
		return fmt.Errorf("inspect peer: %w", err)
	}
	if credErr != nil {
		return fmt.Errorf("peer credentials: %w", credErr)
	}
	if self := uint32(os.Getuid()); peerUID != self {
		return fmt.Errorf("peer uid %d does not match server uid %d", peerUID, self)
	}
	return nil
}
