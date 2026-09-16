//go:build windows

package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

type windowsPaths struct {
	config, state, runtime string
}

// NewPaths resolves the standard Windows per-user locations: roaming
// configuration under %APPDATA%, machine-local state and runtime data under
// %LOCALAPPDATA%. Runtime data is deliberately local rather than roaming —
// control endpoints and the server index describe processes on this machine and
// must never follow the user to another one.
//
// Unlike the POSIX build there is no sun_path length limit to respect, because
// the control endpoint is a named pipe whose name is derived from these paths
// rather than being one of them (see pipePathFor).
//
// The state and runtime directories are created with a DACL granting only this
// account, which is the Windows equivalent of the POSIX 0700. Failure to apply
// that DACL is fatal: proceeding would leave transcripts and the control
// endpoint index readable by every other account on the machine.
func NewPaths() (Paths, error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return nil, errors.New("APPDATA is not set; cannot resolve the configuration directory")
	}
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		return nil, errors.New("LOCALAPPDATA is not set; cannot resolve the state directory")
	}

	p := &windowsPaths{
		config:  filepath.Join(appData, "agents-bridge", "config.yaml"),
		state:   filepath.Join(localAppData, "agents-bridge"),
		runtime: filepath.Join(localAppData, "agents-bridge", "runtime"),
	}

	sa, _, err := ownerOnlySecurityAttributes()
	if err != nil {
		return nil, err
	}
	// state is created before runtime because runtime is nested inside it; the
	// order means the parent already carries the restrictive DACL when the child
	// is created, so there is no window in which runtime inherits a wider one.
	for _, dir := range []string{p.state, p.runtime} {
		if err := mkdirOwnerOnly(dir, sa); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *windowsPaths) Config() string  { return p.config }
func (p *windowsPaths) State() string   { return p.state }
func (p *windowsPaths) Runtime() string { return p.runtime }

// mkdirOwnerOnly creates dir and any missing parents below the volume root, then
// asserts the owner-only DACL.
//
// CreateDirectory applies the descriptor only to directories it actually
// creates, and says nothing about one that already exists — the same gap the
// POSIX build closes by calling Chmod after MkdirAll. setOwnerOnlyDACL is
// therefore applied unconditionally, so a directory left behind by an earlier
// install with a permissive DACL is repaired rather than trusted.
func mkdirOwnerOnly(dir string, sa *windows.SecurityAttributes) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("absolute path for %s: %w", dir, err)
	}
	abs = filepath.Clean(abs)

	// Collect the missing ancestors, deepest last, and create them top down.
	// Ancestors above the bridge's own directory (e.g. %LOCALAPPDATA% itself)
	// normally exist already and keep whatever DACL Windows gave them.
	var missing []string
	for cur := abs; ; {
		if _, statErr := os.Stat(cur); statErr == nil {
			break
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect %s: %w", cur, statErr)
		}
		missing = append(missing, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	for i := len(missing) - 1; i >= 0; i-- {
		name, err := windows.UTF16PtrFromString(missing[i])
		if err != nil {
			return fmt.Errorf("encode path %s: %w", missing[i], err)
		}
		if err := windows.CreateDirectory(name, sa); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return fmt.Errorf("create %s: %w", missing[i], err)
		}
	}
	if err := refuseReparsePoint(abs); err != nil {
		return err
	}
	return setOwnerOnlyDACL(abs)
}

// refuseReparsePoint fails unless abs is a real directory. os.Stat follows a
// junction, so a pre-planted %LOCALAPPDATA%\agents-bridge link would have the
// owner-only DACL written onto whatever it points at instead.
//
// FILE_FLAG_OPEN_REPARSE_POINT opens the link itself rather than its target, so
// the attribute below describes abs and not the destination. Every failure to
// establish what abs is refuses: T10a is same-user, but this must fail closed.
func refuseReparsePoint(abs string) error {
	name, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return fmt.Errorf("encode path %s: %w", abs, err)
	}
	h, err := windows.CreateFile(
		name,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return fmt.Errorf("open %s to check for a reparse point: %w", abs, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return fmt.Errorf("inspect %s for a reparse point: %w", abs, err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%s is a reparse point (junction or symlink); refusing to apply the owner-only DACL through it", abs)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("%s exists but is not a directory", abs)
	}
	return nil
}

// setOwnerOnlyDACL replaces dir's DACL with one granting this account alone, and
// marks it protected so no ACE is inherited from the parent.
func setOwnerOnlyDACL(dir string) error {
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(ownerOnlySDDL(sid))
	if err != nil {
		return fmt.Errorf("build owner-only security descriptor: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("extract DACL: %w", err)
	}
	// PROTECTED_DACL_SECURITY_INFORMATION is what actually removes inherited
	// ACEs; without it the explicit ACE is merely added alongside whatever the
	// parent granted to Users.
	if err := windows.SetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	); err != nil {
		return fmt.Errorf("restrict %s to the current user: %w", dir, err)
	}
	return nil
}
