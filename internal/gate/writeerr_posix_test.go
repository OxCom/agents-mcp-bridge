//go:build !windows

package gate

import (
	"errors"
	"syscall"
)

// isPeerClosedWrite reports whether a write failed because the gate had already
// closed the connection, which is one of the two legal outcomes of
// TestGateRefusesOversizedRequest. Any other write error is a real failure.
func isPeerClosedWrite(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}
