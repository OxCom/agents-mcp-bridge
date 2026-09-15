//go:build !windows

package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContainsRejectsSiblingPrefix(t *testing.T) {
	g := NewPathGuard()
	// The defect a string-prefix check would have: /var/wwwx is not inside /var/www.
	cases := []struct {
		root, path string
		want       bool
	}{
		{"/var/www", "/var/www", true},
		{"/var/www", "/var/www/project", true},
		{"/var/www", "/var/www/a/b/c", true},
		{"/var/www", "/var/wwwx", false},
		{"/var/www", "/var/wwwx/project", false},
		{"/var/www", "/var", false},
		{"/var/www", "/etc/passwd", false},
		{"/var/www", "/", false},
		{"", "/var/www", false},
		{"/var/www", "", false},
	}
	for _, c := range cases {
		if got := g.Contains(c.root, c.path); got != c.want {
			t.Errorf("Contains(%q, %q) = %v, want %v", c.root, c.path, got, c.want)
		}
	}
}

func TestContainsRejectsTraversal(t *testing.T) {
	g := NewPathGuard()
	for _, p := range []string{
		"/var/www/../etc",
		"/var/www/project/../../etc",
		"/var/www/./../../etc/passwd",
	} {
		if g.Contains("/var/www", filepath.Clean(p)) {
			t.Errorf("Contains(/var/www, %q) = true, want false", p)
		}
	}
}

func TestCanonicaliseResolvesSymlinkEscape(t *testing.T) {
	g := NewPathGuard()
	tmp := t.TempDir()
	real, err := g.Canonicalise(tmp) // macOS /var -> /private/var
	if err != nil {
		t.Fatalf("canonicalise tmp: %v", err)
	}
	outside := t.TempDir()
	realOutside, err := g.Canonicalise(outside)
	if err != nil {
		t.Fatalf("canonicalise outside: %v", err)
	}

	root := filepath.Join(real, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(realOutside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// The symlink lives inside the root but points outside it. Canonicalising
	// must reveal that, and containment must then fail.
	resolved, err := g.Canonicalise(link)
	if err != nil {
		t.Fatalf("canonicalise link: %v", err)
	}
	if g.Contains(root, resolved) {
		t.Fatalf("symlink escape not detected: %q resolved to %q, still reported inside %q", link, resolved, root)
	}
}

func TestCanonicaliseRefusesMissingPath(t *testing.T) {
	g := NewPathGuard()
	if _, err := g.Canonicalise(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("expected error for a path that does not exist; a path that cannot be canonicalised must be refused, never approximated")
	}
}

func TestCanonicaliseRefusesTildeUser(t *testing.T) {
	g := NewPathGuard()
	if _, err := g.Canonicalise("~root/x"); err == nil {
		t.Fatal("expected ~user to be refused")
	}
}

func TestNewPathsCreatesOwnerOnlyDirs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "run"))
	p, err := NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	for _, dir := range []string{p.State(), p.Runtime()} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if mode := fi.Mode().Perm(); mode != 0o700 {
			t.Errorf("%s mode = %o, want 700", dir, mode)
		}
	}
}

func TestNewPathsFallsBackWhenRuntimeDirUnset(t *testing.T) {
	// XDG_RUNTIME_DIR is unset on macOS and in many ssh/tmux sessions.
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("XDG_RUNTIME_DIR", "")
	p, err := NewPaths()
	if err != nil {
		t.Fatalf("NewPaths with no XDG_RUNTIME_DIR: %v", err)
	}
	if p.Runtime() == "" {
		t.Fatal("empty runtime path")
	}
	if _, err := os.Stat(p.Runtime()); err != nil {
		t.Fatalf("runtime fallback dir not created: %v", err)
	}
}
