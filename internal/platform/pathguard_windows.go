//go:build windows

package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// fileNameNormalized and volumeNameDOS are the GetFinalPathNameByHandle flags
// that ask for a normalised path with a drive letter. Both are zero and neither
// is exported by golang.org/x/sys/windows, so they are named here rather than
// left as a bare 0 at the call site.
const (
	fileNameNormalized = 0x0
	volumeNameDOS      = 0x0
)

// reservedDeviceNames are the DOS device names the filesystem still resolves
// inside any directory. "C:\logs\nul" is not a file; it is the null device, and
// a containment check that accepted it would let a write escape the allowed root
// into a device. Matching ignores any extension: "nul.txt" is still the device.
var reservedDeviceNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

type windowsGuard struct{}

// NewPathGuard returns the Windows path guard, which closes the aliasing routes
// listed in docs/03-threat-model.md T20: 8.3 short names, junctions, symlinks,
// case variance, UNC paths, device namespaces, reserved device names and
// alternate data streams.
//
// Canonicalisation goes through a real handle rather than string rewriting.
// Only the kernel knows that PROGRA~1 is "Program Files" or that a directory is
// a junction pointing elsewhere, so a path that cannot be opened is refused
// rather than approximated.
//
// The same check-to-use race the POSIX guard carries applies here and is
// tracked as docs/03-threat-model.md T20: containment is evaluated against the
// path as it exists at the moment of the check.
func NewPathGuard() PathGuard { return windowsGuard{} }

func (windowsGuard) Canonicalise(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	// The syntactic rejections run on the caller's own spelling, before any
	// expansion, so the error names what the caller actually wrote.
	if err := rejectDeviceNamespace(path); err != nil {
		return "", err
	}

	expanded, err := expandTilde(path)
	if err != nil {
		return "", err
	}

	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("absolute path: %w", err)
	}
	if err := rejectUNC(abs); err != nil {
		return "", err
	}
	if err := rejectAlternateDataStream(abs); err != nil {
		return "", err
	}
	if err := rejectReservedDevice(abs); err != nil {
		return "", err
	}

	final, err := finalPathOf(abs)
	if err != nil {
		return "", err
	}
	// The resolved path is re-checked: a drive letter can be a mapped network
	// share and a directory can be a junction, so the kernel's answer may be UNC
	// or hold a device component even when the caller's spelling did not.
	if err := rejectUNC(final); err != nil {
		return "", fmt.Errorf("%q resolves outside local storage: %w", path, err)
	}
	if err := rejectReservedDevice(final); err != nil {
		return "", err
	}
	return filepath.Clean(final), nil
}

// Contains reports whether path is root or lies beneath it, comparing whole
// path components case-insensitively because the filesystem is. A string-prefix
// test would let C:\wwwx pass for root C:\www, and a case-sensitive test would
// let C:\WWW\x escape root C:\www. Both arguments must already be canonicalised.
func (windowsGuard) Contains(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)

	rootVol := filepath.VolumeName(root)
	pathVol := filepath.VolumeName(path)
	if !strings.EqualFold(rootVol, pathVol) {
		return false
	}
	rootParts := pathComponents(root[len(rootVol):])
	pathParts := pathComponents(path[len(pathVol):])
	if len(pathParts) < len(rootParts) {
		return false
	}
	for i, want := range rootParts {
		if !strings.EqualFold(want, pathParts[i]) {
			return false
		}
	}
	return true
}

// pathComponents splits on both separators, dropping empties, so a trailing or
// doubled separator cannot create a phantom component.
func pathComponents(p string) []string {
	raw := strings.FieldsFunc(p, func(r rune) bool { return r == '\\' || r == '/' })
	out := make([]string, 0, len(raw))
	for _, c := range raw {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}
	return out
}

// expandTilde mirrors the POSIX guard so one config file behaves the same on
// both platforms. ~user stays unsupported: resolving it needs the account
// database and invites surprises.
func expandTilde(path string) (string, error) {
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand ~: %w", err)
	}
	switch {
	case path == "~":
		return home, nil
	case strings.HasPrefix(path, "~/"), strings.HasPrefix(path, `~\`):
		return filepath.Join(home, path[2:]), nil
	default:
		return "", fmt.Errorf("unsupported ~user path: %q", path)
	}
}

// rejectDeviceNamespace refuses the two prefixes that bypass the Win32 path
// parser entirely. \\?\ skips normalisation, so it would hand the kernel a path
// this guard has not checked; \\.\ addresses devices rather than files.
func rejectDeviceNamespace(path string) error {
	norm := strings.ReplaceAll(path, "/", `\`)
	switch {
	case strings.HasPrefix(norm, `\\?\`):
		return fmt.Errorf(`%q uses the \\?\ extended-length namespace, which bypasses path normalisation`, path)
	case strings.HasPrefix(norm, `\\.\`):
		return fmt.Errorf(`%q uses the \\.\ device namespace`, path)
	}
	return nil
}

// rejectUNC refuses network paths. The allowed roots describe local storage; a
// UNC path is a different machine's namespace with a different access-control
// story, and its canonical form cannot be reasoned about locally.
func rejectUNC(path string) error {
	vol := filepath.VolumeName(path)
	if strings.HasPrefix(vol, `\\`) || strings.HasPrefix(vol, "//") {
		return fmt.Errorf("%q is a UNC network path", path)
	}
	norm := strings.ReplaceAll(path, "/", `\`)
	if strings.HasPrefix(norm, `\\`) {
		return fmt.Errorf("%q is a UNC network path", path)
	}
	return nil
}

// rejectAlternateDataStream refuses a second colon. "file.txt:hidden" writes a
// stream that a containment check on "file.txt" would have approved, and
// "C:\dir::$INDEX_ALLOCATION" names the directory's own stream.
func rejectAlternateDataStream(abs string) error {
	rest := abs
	if vol := filepath.VolumeName(abs); vol != "" {
		rest = abs[len(vol):]
	}
	if strings.Contains(rest, ":") {
		return fmt.Errorf("%q names an alternate data stream", abs)
	}
	return nil
}

func rejectReservedDevice(abs string) error {
	for _, component := range pathComponents(strings.TrimPrefix(abs, filepath.VolumeName(abs))) {
		// Trailing dots and spaces are stripped by the filesystem, so "nul. "
		// and "nul" name the same device.
		trimmed := strings.TrimRight(component, " .")
		base := trimmed
		if i := strings.IndexByte(trimmed, '.'); i >= 0 {
			base = trimmed[:i]
		}
		if reservedDeviceNames[strings.ToLower(base)] {
			return fmt.Errorf("%q contains the reserved device name %q", abs, component)
		}
	}
	return nil
}

// finalPathOf opens the path and asks the kernel what it really is. The handle
// is opened with no access rights at all — a zero DesiredAccess still permits
// querying metadata — so canonicalising a path grants no ability to read it.
// FILE_FLAG_BACKUP_SEMANTICS is what allows a directory to be opened;
// FILE_SHARE_* keeps the check from disturbing another process's use of the file.
func finalPathOf(abs string) (string, error) {
	name, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return "", fmt.Errorf("encode path %q: %w", abs, err)
	}
	h, err := windows.CreateFile(
		name,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", abs, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()

	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), fileNameNormalized|volumeNameDOS)
		if err != nil {
			return "", fmt.Errorf("canonicalise %q: %w", abs, err)
		}
		// n excludes the terminator on success and includes it when the buffer
		// was too small, which is the documented way to ask for the real size.
		if int(n) < len(buf) {
			return stripExtendedPrefix(windows.UTF16ToString(buf[:n])), nil
		}
		buf = make([]uint16, n+1)
	}
}

// stripExtendedPrefix removes the \\?\ that GetFinalPathNameByHandle prepends.
// The \\?\UNC\ form is left intact so the UNC check downstream still sees it as
// a network path rather than as a directory literally named "UNC".
func stripExtendedPrefix(p string) string {
	if strings.HasPrefix(p, `\\?\UNC\`) {
		return `\\` + p[len(`\\?\UNC\`):]
	}
	return strings.TrimPrefix(p, `\\?\`)
}
