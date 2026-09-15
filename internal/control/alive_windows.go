//go:build windows

package control

// processAlive is unimplemented on Windows (v1.1). Reporting every entry as
// dead is the safe direction: the operator sees no servers rather than being
// pointed at a socket that may not be theirs.
func processAlive(pid int) bool { return false }
