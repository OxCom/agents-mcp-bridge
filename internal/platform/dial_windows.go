//go:build windows

package platform

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// pipeBusyRetry is how long to wait before retrying a pipe whose instances are
// all in use. The server creates the next instance as soon as it accepts, so the
// busy window is short.
const pipeBusyRetry = 10 * time.Millisecond

// DialControl connects to a control or gate endpoint addressed by a path-shaped
// name, and refuses to return a connection until the process serving it has been
// shown to run under this same account.
//
// That check is the client half of T21. FILE_FLAG_FIRST_PIPE_INSTANCE stops a
// squatter from pre-creating the bridge's pipe name, but it cannot help a client
// that starts talking to a name the bridge never created — a hostile process
// that got there first, or one that created the name after the bridge exited.
// Verifying the server before sending anything means the client never hands a
// request, or a per-run gate token, to an impostor:
//
//	GetNamedPipeServerProcessId -> the server's pid
//	OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION) -> a handle to it
//	OpenProcessToken(TOKEN_QUERY) -> its access token
//	GetTokenInformation(TokenUser) -> its user SID
//	EqualSid against this process's own token user SID
//
// Like the server-side check this authenticates the ACCOUNT. It rules out a
// different local user impersonating the bridge; it does not distinguish the
// bridge from another process of your own (T10a).
//
// SECURITY_SQOS_PRESENT|SECURITY_ANONYMOUS is set on the open so the server
// cannot impersonate this client even if it is not the bridge. Without it a
// pipe server may assume the caller's identity, which would turn a squatted pipe
// from an eavesdropper into a way to act as the operator.
func DialControl(name string, timeout time.Duration) (net.Conn, error) {
	pipe, err := pipeName(name)
	if err != nil {
		return nil, err
	}
	namePtr, err := windows.UTF16PtrFromString(pipe)
	if err != nil {
		return nil, fmt.Errorf("encode pipe name: %w", err)
	}

	deadline := time.Now().Add(timeout)
	var handle windows.Handle
	for {
		handle, err = windows.CreateFile(
			namePtr,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_ANONYMOUS,
			0,
		)
		if err == nil {
			break
		}
		// Every instance is busy: the server has not yet created the next one.
		// Anything else, including "no such pipe", fails immediately, matching
		// the POSIX dial against a socket that is not there.
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, fmt.Errorf("connect to %s: %w", pipe, err)
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("connect to %s: %w", pipe, os.ErrDeadlineExceeded)
		}
		time.Sleep(pipeBusyRetry)
	}

	conn, err := newPipeConn(handle, pipe, false)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}

	pid, err := conn.serverPID()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("identify the server behind %s: %w", pipe, err)
	}
	if err := sameUserAsSelf(pid); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("refusing %s: %w", pipe, err)
	}
	return conn, nil
}
