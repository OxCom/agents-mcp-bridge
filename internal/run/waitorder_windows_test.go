//go:build windows

package run

// pidAlive is not checked on Windows: the job object terminates descendants and
// there is no signal-0 probe. The timing assertion still applies.
func pidAlive(pid int) bool { return false }
