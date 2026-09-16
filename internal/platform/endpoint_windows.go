//go:build windows

package platform

import (
	"fmt"
	"net"

	"golang.org/x/sys/windows"
)

type windowsEndpoint struct{}

// NewControlEndpoint returns the Windows control endpoint: a named pipe under
// \\.\pipe\agents-bridge\, created with FILE_FLAG_FIRST_PIPE_INSTANCE, carrying a
// DACL that grants this account alone, with the peer's token user SID checked on
// every connection.
//
// This authenticates the Windows ACCOUNT, not the operator. Agent B runs as the
// same user, so it passes this check exactly as the operator's TUI does; see
// docs/03-threat-model.md T10a. The SID check is what keeps a *different* local
// account out (T10), and the per-run gate token is what narrows the gate further
// (T23). Neither makes same-user a privilege boundary.
func NewControlEndpoint() ControlEndpoint { return windowsEndpoint{} }

// Listen creates the pipe for a path-shaped endpoint name.
//
// FILE_FLAG_FIRST_PIPE_INSTANCE is the anti-squatting control from T21: if a
// hostile local process created the name first, creation fails rather than
// quietly handing us a second instance of somebody else's pipe. There is no
// "remove the stale one and retry" path here, and deliberately so — the POSIX
// build unlinks a stale socket file because a crashed process leaves one behind,
// but a pipe has no on-disk remnant. A name that already exists means a live
// process owns it, so failing is correct.
func (windowsEndpoint) Listen(name string) (net.Listener, error) {
	pipe, err := pipeName(name)
	if err != nil {
		return nil, err
	}
	sa, sd, err := ownerOnlySecurityAttributes()
	if err != nil {
		return nil, err
	}
	cancelEv, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("create listener event: %w", err)
	}
	// cancelEv lives as long as the listener: Close signals it to release a
	// blocked Accept, and an Accept may still be waiting on it afterwards, so it
	// is not closed. One handle per control endpoint, released with the process.
	return &pipeListener{
		name:     pipe,
		sa:       sa,
		sd:       sd,
		first:    true,
		cancelEv: cancelEv,
	}, nil
}

// VerifyPeer establishes that the connected client runs under the same Windows
// account as this process. It is the named-pipe counterpart of SO_PEERCRED:
//
//	GetNamedPipeClientProcessId -> the client's pid
//	OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION) -> a handle to it
//	OpenProcessToken(TOKEN_QUERY) -> its access token
//	GetTokenInformation(TokenUser) -> its user SID
//	EqualSid against this process's own token user SID
//
// Every failure denies. A peer whose token cannot be read is refused, not
// assumed friendly.
//
// The pid is read from the kernel's own record of the connection, never from
// anything the peer sends, so the peer cannot nominate another process. The pid
// could in principle be recycled between the query and the token read; the
// window is the duration of two local calls and closing it would require the
// kernel to hand out a process handle directly, which no named-pipe API offers.
func (windowsEndpoint) VerifyPeer(conn net.Conn) error {
	pc, ok := conn.(*pipeConn)
	if !ok {
		return fmt.Errorf("control connection is %T, not a named pipe", conn)
	}
	pid, err := pc.clientPID()
	if err != nil {
		return fmt.Errorf("peer credentials unavailable: %w", err)
	}
	if err := sameUserAsSelf(pid); err != nil {
		return fmt.Errorf("peer credentials: %w", err)
	}
	return nil
}
