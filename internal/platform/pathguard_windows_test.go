//go:build windows

package platform

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestCanonicaliseRejectsAliasingRoutes pins docs/03-threat-model.md T20. Each
// input names a way to reach a path the containment check would otherwise
// approve; every one must be refused before any handle is opened.
func TestCanonicaliseRejectsAliasingRoutes(t *testing.T) {
	g := NewPathGuard()
	cases := []struct {
		name string
		path string
		want string
	}{
		{"UNC share", `\\server\share\work`, "UNC"},
		{"UNC forward slashes", `//server/share/work`, "UNC"},
		{"extended-length namespace", `\\?\C:\work`, `\\?\`},
		{"device namespace", `\\.\PhysicalDrive0`, `\\.\`},
		{"device namespace pipe", `\\.\pipe\agents-bridge\x`, `\\.\`},
		{"reserved CON", `C:\work\con`, "reserved device"},
		{"reserved NUL with extension", `C:\work\nul.txt`, "reserved device"},
		{"reserved COM1", `C:\work\COM1`, "reserved device"},
		{"reserved LPT9 mixed case", `C:\work\LpT9`, "reserved device"},
		{"reserved AUX trailing dot", `C:\work\aux.`, "reserved device"},
		{"alternate data stream", `C:\work\file.txt:hidden`, "alternate data stream"},
		{"directory stream", `C:\work::$INDEX_ALLOCATION`, "alternate data stream"},
		{"empty", ``, "empty path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := g.Canonicalise(tc.path)
			if err == nil {
				t.Fatalf("Canonicalise(%q) returned %q, want refusal", tc.path, got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Canonicalise(%q) error = %v, want it to mention %q", tc.path, err, tc.want)
			}
		})
	}
}

// TestCanonicaliseRefusesAPathThatCannotBeResolved pins the rule that a path
// which cannot be canonicalised is refused, never passed through: an
// unresolvable path must not reach the containment check as a bare string.
func TestCanonicaliseRefusesAPathThatCannotBeResolved(t *testing.T) {
	g := NewPathGuard()
	missing := filepath.Join(t.TempDir(), "no-such-dir", "no-such-file")
	if got, err := g.Canonicalise(missing); err == nil {
		t.Fatalf("Canonicalise(%q) returned %q, want refusal", missing, got)
	}
}

// TestCanonicaliseCollapsesShortNamesAndCase verifies against the real
// filesystem that the 8.3 alias and a case variant both resolve to one answer.
// It needs a live Windows filesystem and does not run on other platforms.
func TestCanonicaliseCollapsesShortNamesAndCase(t *testing.T) {
	g := NewPathGuard()
	dir := t.TempDir()
	canonical, err := g.Canonicalise(dir)
	if err != nil {
		t.Fatalf("Canonicalise(%q): %v", dir, err)
	}
	upper, err := g.Canonicalise(strings.ToUpper(dir))
	if err != nil {
		t.Fatalf("Canonicalise(upper): %v", err)
	}
	if !strings.EqualFold(canonical, upper) {
		t.Fatalf("case variants disagree: %q vs %q", canonical, upper)
	}
	if strings.HasPrefix(canonical, `\\?\`) {
		t.Fatalf("canonical path kept the extended-length prefix: %q", canonical)
	}
}

// TestContainsIsCaseInsensitiveAndComponentWise pins both halves of the Windows
// containment rule: the filesystem ignores case, so a case variant must not
// escape the root, and a prefix that is not a whole component must not pass.
func TestContainsIsCaseInsensitiveAndComponentWise(t *testing.T) {
	g := NewPathGuard()
	cases := []struct {
		name, root, path string
		want             bool
	}{
		{"identical", `C:\www`, `C:\www`, true},
		{"child", `C:\www`, `C:\www\site`, true},
		{"deep child", `C:\www`, `C:\www\a\b\c`, true},
		{"upper-case root", `C:\www`, `C:\WWW\site`, true},
		{"mixed-case child", `C:\WwW`, `C:\wWw\Site`, true},
		{"upper-case drive", `c:\www`, `C:\www\site`, true},
		{"sibling sharing a prefix", `C:\www`, `C:\wwwx`, false},
		{"sibling sharing a prefix, other case", `C:\www`, `C:\WWWX`, false},
		{"parent", `C:\www\site`, `C:\www`, false},
		{"escape upwards", `C:\www`, `C:\other`, false},
		{"different volume", `C:\www`, `D:\www\site`, false},
		{"empty root", ``, `C:\www`, false},
		{"empty path", `C:\www`, ``, false},
		{"volume root contains everything on it", `C:\`, `C:\anything\here`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := g.Contains(tc.root, tc.path); got != tc.want {
				t.Fatalf("Contains(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
			}
		})
	}
}

// TestStripExtendedPrefixKeepsUNCVisible pins that unwrapping the \\?\ prefix
// never disguises a UNC path as a local directory named "UNC" — that would slip
// a network path past the rejection that runs on the resolved result.
func TestStripExtendedPrefixKeepsUNCVisible(t *testing.T) {
	if got := stripExtendedPrefix(`\\?\C:\work`); got != `C:\work` {
		t.Fatalf("local path: got %q", got)
	}
	got := stripExtendedPrefix(`\\?\UNC\server\share`)
	if got != `\\server\share` {
		t.Fatalf("UNC path: got %q, want %q", got, `\\server\share`)
	}
	if err := rejectUNC(got); err == nil {
		t.Fatalf("unwrapped UNC path %q was not rejected", got)
	}
}
