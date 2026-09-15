//go:build !windows

package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type posixGuard struct{}

// NewPathGuard returns the POSIX path guard.
//
// Containment here is lexical after canonicalisation, which is sound for a path
// that exists at the moment it is checked. It is NOT a defence against a path
// component being replaced between the check and the spawn: that race needs
// handle-relative primitives (openat2) and is tracked as a known limitation in
// docs/03-threat-model.md T20.
func NewPathGuard() PathGuard { return posixGuard{} }

func (posixGuard) Canonicalise(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~: %w", err)
		}
		switch {
		case path == "~":
			path = home
		case strings.HasPrefix(path, "~/"):
			path = filepath.Join(home, path[2:])
		default:
			// ~user is deliberately unsupported: resolving it needs the user
			// database and invites surprises. Refuse rather than guess.
			return "", fmt.Errorf("unsupported ~user path: %q", path)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("absolute path: %w", err)
	}
	// EvalSymlinks requires the path to exist. A path that cannot be resolved is
	// refused, never approximated.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", abs, err)
	}
	return filepath.Clean(resolved), nil
}

// Contains reports whether path is root or lies beneath it, comparing whole path
// components. A string-prefix test would let /var/wwwx pass for root /var/www.
// Both arguments must already be canonicalised.
func (posixGuard) Contains(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if root == path {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}
