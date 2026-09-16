//go:build windows

package gate

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isPeerClosedWrite is the Windows counterpart of the POSIX EPIPE/ECONNRESET
// check: a write to a pipe whose server end has gone fails with
// ERROR_BROKEN_PIPE, or with ERROR_NO_DATA when the close lands between the
// disconnect and the write. Any other write error is a real failure.
func isPeerClosedWrite(err error) bool {
	return errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_NO_DATA)
}
