//go:build windows

// Windows support is a v1.1 target. Everything compiles and is built in CI from
// the first commit so the platform seams cannot rot, but each constructor
// refuses rather than pretending: a half-implemented job object or an
// unvalidated named pipe would be a security regression, not a partial feature.
//
// The blocker is not effort. npm-installed `claude` and `codex` are .cmd shims,
// which the adapter rules refuse, so the two reference adapters would not work
// via the install path most Windows users have. Resolving that changes the
// config schema, so it is settled before it is promised.
//
// See docs/01-requirements.md FR-9 and docs/08-roadmap.md phase 5a.
package platform

import (
	"errors"
	"net"
	"os/exec"
)

var errWindows = errors.New("Windows support lands in v1.1; see docs/08-roadmap.md phase 5a")

func NewPaths() (Paths, error) { return nil, errWindows }

func NewPathGuard() PathGuard { return windowsStub{} }

func NewProcessGroup() ProcessGroup { return windowsStub{} }

func NewControlEndpoint() ControlEndpoint { return windowsStub{} }

type windowsStub struct{}

func (windowsStub) Canonicalise(string) (string, error) { return "", errWindows }

// Contains returns false: an unimplemented containment check must never report
// that a path is inside an allowed root.
func (windowsStub) Contains(string, string) bool { return false }

func (windowsStub) Attach(*exec.Cmd) error { return errWindows }
func (windowsStub) KillAll() error         { return errWindows }

func (windowsStub) Listen(string) (net.Listener, error) { return nil, errWindows }
func (windowsStub) VerifyPeer(net.Conn) error           { return errWindows }
