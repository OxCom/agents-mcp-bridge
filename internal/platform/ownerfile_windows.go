//go:build windows

package platform

import (
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/windows"
)

// WriteOwnerOnlyFile writes data to path so that no other account can read it.
// A Go file mode grants nothing on Windows, so the restriction is a DACL: the
// descriptor is handed to CreateFile, leaving no window in which the file
// exists carrying the parent directory's inherited ACEs.
//
// Two files depend on this: servers.json, the index of every live bridge's
// control endpoint, and the per-run gate config, which carries the gate token.
func WriteOwnerOnlyFile(path string, data []byte) error {
	sa, sd, err := ownerOnlySecurityAttributes()
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, sa,
		windows.CREATE_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	runtime.KeepAlive(sd)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}

	f := os.NewFile(uintptr(h), path)
	writeErr := func() error {
		if _, err := f.Write(data); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		return f.Close()
	}()
	if writeErr != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return writeErr
	}

	// CREATE_ALWAYS over an existing file keeps that file's DACL and ignores
	// the descriptor above, so the restriction is asserted afterwards too.
	// Failing here removes the file rather than leaving a readable one.
	if err := setOwnerOnlyDACL(path); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}
