package config

import "fmt"

// Error is a configuration fault. Every one is fatal: a typo in a security
// setting must stop the server, never be silently ignored.
type Error struct {
	Path string // where in the config, e.g. "agents.codex.steer"
	Msg  string
}

func (e *Error) Error() string {
	if e.Path == "" {
		return e.Msg
	}
	return fmt.Sprintf("%s: %s", e.Path, e.Msg)
}

func errf(path, format string, args ...any) *Error {
	return &Error{Path: path, Msg: fmt.Sprintf(format, args...)}
}
