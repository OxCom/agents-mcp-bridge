//go:build !windows

package config

import "syscall"

func syscallMkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }
