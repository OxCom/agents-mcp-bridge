//go:build !windows

package platform

import "os"

// WriteOwnerOnlyFile writes data to path so that no other account can read it.
// A file mode carries that guarantee on POSIX and carries nothing on Windows,
// which is why this is a seam rather than an os.WriteFile call at each site.
func WriteOwnerOnlyFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, ownerOnlyFileMode); err != nil {
		return err
	}
	// WriteFile honours the umask when it creates, and leaves an existing
	// file's mode alone, so the mode is asserted rather than assumed.
	return os.Chmod(path, ownerOnlyFileMode)
}

const ownerOnlyFileMode = 0o600
