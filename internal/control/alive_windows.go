//go:build windows

package control

import (
	"golang.org/x/sys/windows"
)

// stillActive is the exit code GetExitCodeProcess reports for a process that has
// not exited. It is not exported by golang.org/x/sys/windows.
const stillActive = 259

// processAlive reports whether a pid is still running.
//
// The POSIX build sends signal 0, which performs the existence and permission
// checks without delivering anything. The Windows equivalent opens the process
// with the narrowest right that allows the exit code to be read and asks for it.
//
// Fail closed, matching the POSIX contract: every error reads as "not alive", so
// a stale index entry is dropped rather than the operator being pointed at a
// control endpoint that may not be theirs.
//
// Known imprecision, inherent to the API: a process that genuinely exited with
// code 259 is reported as alive until its handle is released. The bridge writes
// no such exit code, and the consequence is a stale entry surviving one sweep
// rather than a wrong endpoint being trusted — the SID check on connect is what
// actually decides whether an endpoint may be used.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// #nosec G115 -- guarded above; a Windows pid is a DWORD that fits uint32.
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// ERROR_INVALID_PARAMETER means no such process; ERROR_ACCESS_DENIED
		// means it belongs to someone else, which is not a server of ours.
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
