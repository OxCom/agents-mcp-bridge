//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"syscall"
)

// checkOpenFile validates the file the loader actually reads, by inspecting the
// open descriptor rather than the path.
//
// This is the anti-TOCTOU form: a path-based Stat followed by an Open leaves a
// window in which the name can be re-pointed at a different file. Everything
// here is asserted against the descriptor whose bytes we go on to parse, so
// there is no window at all for the file itself.
func checkOpenFile(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return errf("", "stat config: %v", err)
	}
	if !fi.Mode().IsRegular() {
		return errf("", "%s is not a regular file", f.Name())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errf("", "cannot determine ownership of %s", f.Name())
	}
	if self := uint32(os.Getuid()); st.Uid != self {
		return errf("", "%s is owned by uid %d, not %d; the config is the root of authority and must be yours", f.Name(), st.Uid, self)
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return errf("", "%s is mode %04o; a group- or world-writable file is refused", f.Name(), perm)
	}
	return nil
}

// checkParentDir refuses a config whose directory another user could rewrite:
// write access to the directory is write access to the file, whatever the
// file's own mode says.
//
// Symlinks are resolved first. A parent path that traverses a symlink into a
// world-writable directory must be judged by its target, not by the link.
func checkParentDir(path string) error {
	dir := filepath.Dir(path)
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return errf("", "resolve config directory %s: %v", dir, err)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return errf("", "stat %s: %v", resolved, err)
	}
	if !fi.IsDir() {
		return errf("", "%s is not a directory", resolved)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errf("", "cannot determine ownership of %s", resolved)
	}
	if self := uint32(os.Getuid()); st.Uid != self && st.Uid != 0 {
		return errf("", "config directory %s is owned by uid %d, not %d or root", resolved, st.Uid, self)
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return errf("", "config directory %s is mode %04o; a group- or world-writable directory is refused, because write access to it is write access to the config", resolved, perm)
	}
	return nil
}
