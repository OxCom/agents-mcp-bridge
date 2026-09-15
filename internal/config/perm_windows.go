//go:build windows

package config

import "os"

// Windows permission checking is unimplemented and therefore refuses.
//
// The POSIX checks ask whether another user could rewrite the config, which is
// the root of all authority here. The Windows equivalent is a DACL inspection
// (owner, SYSTEM and Administrators may write; nobody else) and it lands in
// v1.1 with the rest of the port. Until then the honest behaviour is to refuse:
// a permission check that silently passes is worse than none at all.
//
// Options.SkipPermissionCheck bypasses this, deliberately.
func checkOpenFile(f *os.File) error {
	return errf("", "config permission checking is not implemented on Windows (v1.1); "+
		"refusing to assume %s is protected. See docs/08-roadmap.md phase 5a", f.Name())
}

func checkParentDir(path string) error {
	return errf("", "config directory permission checking is not implemented on Windows (v1.1); "+
		"refusing to assume %s is protected. See docs/08-roadmap.md phase 5a", path)
}
