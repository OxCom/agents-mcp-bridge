//go:build !windows

package platform

import (
	"net"
	"time"
)

// DialControl connects to a control or gate endpoint addressed by name.
//
// On POSIX the name is the unix socket path, so this is a plain dial. The seam
// exists for Windows, where the same name has to be translated to a pipe and the
// server's identity checked before anything is sent.
func DialControl(name string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", name, timeout)
}
