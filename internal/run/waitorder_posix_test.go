//go:build !windows

package run

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// pidAlive reports whether pid is a running process. On Linux a zombie counts
// as dead: under a pid 1 that does not reap (a container without --init) a
// killed orphan stays a zombie for good, and signal 0 still answers for it.
func pidAlive(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return false
	}
	if runtime.GOOS == "linux" {
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return false
		}
		// "pid (comm) S ...": comm may hold spaces and parentheses, so the
		// state is the first field after the last ')'.
		s := string(raw)
		if i := strings.LastIndexByte(s, ')'); i >= 0 {
			if f := strings.Fields(s[i+1:]); len(f) > 0 && f[0] == "Z" {
				return false
			}
		}
	}
	return true
}
