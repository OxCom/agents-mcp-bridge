// Package platform holds the four OS seams. Nothing else in the codebase is
// platform-aware: every difference between POSIX and Windows lives behind one of
// these interfaces, so the Windows port (v1.1) is an implementation job rather
// than a rewrite. See docs/02-architecture.md §4a.
package platform

import (
	"net"
	"os/exec"
)

// Paths resolves the per-user locations the bridge owns. Implementations must
// create the state and runtime directories owner-only.
type Paths interface {
	Config() string  // the single config file that is read
	State() string   // transcripts, audit log
	Runtime() string // control endpoints, server index
}

// PathGuard canonicalises and contains filesystem paths. Canonicalise must
// resolve symlinks; on Windows it must additionally collapse 8.3 short names and
// junctions and reject UNC, device namespaces and reserved device names.
//
// Contains compares component-wise. A string-prefix test is wrong: it lets
// /var/wwwx pass for root /var/www.
type PathGuard interface {
	Canonicalise(path string) (string, error)
	Contains(root, path string) bool
}

// ProcessGroup binds a child and all its descendants to one killable unit.
// Attach must be called BEFORE Start: on Windows a job object assigned after the
// process is running races the child spawning grandchildren.
type ProcessGroup interface {
	Attach(cmd *exec.Cmd) error
	KillAll() error
}

// ControlEndpoint is the operator's local IPC channel. There is never a TCP
// listener. VerifyPeer must reject a connection whose peer is not the same user.
//
// This is not an operator boundary: agent B runs as the same user. See
// docs/03-threat-model.md T10a.
type ControlEndpoint interface {
	Listen(name string) (net.Listener, error)
	VerifyPeer(conn net.Conn) error
}
