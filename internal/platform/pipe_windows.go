//go:build windows

package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// pipePrefix namespaces every endpoint this bridge creates. The pipe namespace
// is machine-wide and flat, so a prefix is the only grouping available.
const pipePrefix = `\\.\pipe\agents-bridge\`

// pipeBufferSize sizes the kernel's per-instance buffers. Control traffic is
// one small JSON request and one reply.
const pipeBufferSize = 64 * 1024

// pipeName maps a caller's path-shaped endpoint name onto a pipe name.
//
// The rest of the codebase addresses endpoints as file paths
// (<runtime>\1234.sock, <runtime>\gate\gate-<run-id>.sock) because that is what
// a unix socket is. Windows has no such file, so the path is translated rather
// than created.
//
// The mapping is deterministic and injective enough to be safe: the readable
// tail makes a live pipe recognisable in tooling, and the SHA-256 prefix of the
// full lower-cased path distinguishes two endpoints whose tails collide. It is
// case-insensitive because the paths it is given come from the filesystem.
//
// This is not a security boundary. Anyone able to compute the name can attempt
// to connect; what stops them is the DACL on the pipe and the SID check on both
// ends, not the obscurity of the name.
func pipeName(endpoint string) (string, error) {
	if endpoint == "" {
		return "", errors.New("empty endpoint name")
	}
	normalised := strings.ToLower(filepath.Clean(endpoint))
	sum := sha256.Sum256([]byte(normalised))
	tail := sanitisePipeComponent(filepath.Base(normalised))
	name := pipePrefix + tail + "-" + hex.EncodeToString(sum[:6])
	// The documented limit is 256 characters for the whole name. The tail is
	// capped below, so this only ever trips on an absurd base name.
	if len(name) > 256 {
		return "", fmt.Errorf("pipe name for %q exceeds 256 characters", endpoint)
	}
	return name, nil
}

// sanitisePipeComponent keeps the readable part of the name to characters that
// cannot alter the namespace. A backslash would create a sub-path, and any
// other punctuation is simply dropped rather than trusted.
func sanitisePipeComponent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
		if b.Len() >= 64 {
			break
		}
	}
	if b.Len() == 0 {
		return "endpoint"
	}
	return b.String()
}

// pipeAddr satisfies net.Addr for a named pipe.
type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

// pipeConn is a net.Conn over one named-pipe instance.
//
// It is written on overlapped I/O rather than blocking handles because net.Conn
// promises working deadlines, and the bridge relies on them: the gate bounds
// every request read (T23) and the control client bounds a whole round trip so a
// wedged server cannot freeze the TUI. A blocking handle offers no way to
// abandon a read that never completes.
//
// Concurrency matches net.Conn's contract: one reader and one writer may run at
// once, each serialised by its own mutex because each reuses a single event.
// handleMu is held for read while any I/O is in flight and for write by Close,
// so the handle cannot be closed out from under an operation in progress.
type pipeConn struct {
	handle   windows.Handle
	addr     pipeAddr
	isServer bool

	handleMu sync.RWMutex

	readMu   sync.Mutex
	readEv   windows.Handle
	writeMu  sync.Mutex
	writeEv  windows.Handle
	cancelEv windows.Handle

	deadlineMu sync.Mutex
	readDl     time.Time
	writeDl    time.Time

	closeOnce sync.Once
	closed    bool
}

func newPipeConn(h windows.Handle, addr string, isServer bool) (*pipeConn, error) {
	c := &pipeConn{handle: h, addr: pipeAddr(addr), isServer: isServer}
	for _, ev := range []*windows.Handle{&c.readEv, &c.writeEv, &c.cancelEv} {
		// Manual-reset, initially unsignalled.
		e, err := windows.CreateEvent(nil, 1, 0, nil)
		if err != nil {
			c.destroyEvents()
			return nil, fmt.Errorf("create pipe event: %w", err)
		}
		*ev = e
	}
	return c, nil
}

func (c *pipeConn) destroyEvents() {
	for _, ev := range []windows.Handle{c.readEv, c.writeEv, c.cancelEv} {
		if ev != 0 {
			_ = windows.CloseHandle(ev)
		}
	}
}

func (c *pipeConn) LocalAddr() net.Addr  { return c.addr }
func (c *pipeConn) RemoteAddr() net.Addr { return c.addr }

func (c *pipeConn) SetDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.readDl, c.writeDl = t, t
	return nil
}

func (c *pipeConn) SetReadDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.readDl = t
	return nil
}

func (c *pipeConn) SetWriteDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.writeDl = t
	return nil
}

func (c *pipeConn) deadlines() (read, write time.Time) {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	return c.readDl, c.writeDl
}

func (c *pipeConn) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	readDl, _ := c.deadlines()
	n, err := c.do(c.readEv, readDl, func(ov *windows.Overlapped) error {
		// done must not be nil even though the overlapped path ignores it and
		// GetOverlappedResult supplies the real count: the race-enabled build
		// of x/sys dereferences it unconditionally, and `go test -race` is the
		// suite CI runs.
		var done uint32
		return windows.ReadFile(c.handle, b, &done, ov)
	})
	if err != nil {
		// The peer closing its end is EOF, not a failure.
		if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) {
			return int(n), io.EOF
		}
		return int(n), &net.OpError{Op: "read", Net: "pipe", Addr: c.addr, Err: err}
	}
	if n == 0 {
		return 0, io.EOF
	}
	return int(n), nil
}

func (c *pipeConn) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, writeDl := c.deadlines()
	written := 0
	for written < len(b) {
		chunk := b[written:]
		n, err := c.do(c.writeEv, writeDl, func(ov *windows.Overlapped) error {
			// Non-nil for the same reason as the read path above.
			var done uint32
			return windows.WriteFile(c.handle, chunk, &done, ov)
		})
		written += int(n)
		if err != nil {
			return written, &net.OpError{Op: "write", Net: "pipe", Addr: c.addr, Err: err}
		}
		if n == 0 {
			return written, &net.OpError{Op: "write", Net: "pipe", Addr: c.addr, Err: io.ErrShortWrite}
		}
	}
	return written, nil
}

// do issues one overlapped operation and waits for it, honouring the deadline
// and Close. The OVERLAPPED and its event stay alive until the operation has
// genuinely finished — after a timeout the I/O is cancelled and then waited for,
// so the kernel is never left writing into a buffer this function has returned.
func (c *pipeConn) do(ev windows.Handle, deadline time.Time, issue func(*windows.Overlapped) error) (uint32, error) {
	c.handleMu.RLock()
	defer c.handleMu.RUnlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	if err := windows.ResetEvent(ev); err != nil {
		return 0, fmt.Errorf("reset event: %w", err)
	}

	ov := &windows.Overlapped{HEvent: ev}
	err := issue(ov)
	switch {
	case err == nil:
		// Completed inline; GetOverlappedResult below still reports the count.
	case errors.Is(err, windows.ERROR_IO_PENDING):
		if waitErr := c.await(ev, ov, deadline); waitErr != nil {
			return 0, waitErr
		}
	default:
		return 0, err
	}

	var done uint32
	if err := windows.GetOverlappedResult(c.handle, ov, &done, true); err != nil {
		return done, err
	}
	return done, nil
}

// await blocks until the operation completes, the deadline passes, or Close
// signals cancelEv.
func (c *pipeConn) await(ev windows.Handle, ov *windows.Overlapped, deadline time.Time) error {
	timeout := uint32(windows.INFINITE)
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return c.cancel(ov, os.ErrDeadlineExceeded)
		}
		timeout = uint32(remaining.Milliseconds()) + 1
	}
	event, err := windows.WaitForMultipleObjects([]windows.Handle{ev, c.cancelEv}, false, timeout)
	switch {
	case err != nil:
		return c.cancel(ov, fmt.Errorf("wait for pipe I/O: %w", err))
	case event == windows.WAIT_OBJECT_0:
		return nil
	case event == windows.WAIT_OBJECT_0+1:
		return c.cancel(ov, net.ErrClosed)
	case event == uint32(windows.WAIT_TIMEOUT):
		return c.cancel(ov, os.ErrDeadlineExceeded)
	default:
		return c.cancel(ov, fmt.Errorf("unexpected wait result %#x", event))
	}
}

// cancel aborts an in-flight operation and waits for the cancellation to be
// acknowledged before returning cause. Returning while the kernel still owns the
// caller's buffer would be a use-after-return.
func (c *pipeConn) cancel(ov *windows.Overlapped, cause error) error {
	_ = windows.CancelIoEx(c.handle, ov)
	var done uint32
	_ = windows.GetOverlappedResult(c.handle, ov, &done, true)
	return cause
}

// Close is idempotent. It signals cancelEv first so any blocked operation
// unwinds, then takes the write lock, which cannot be granted until every
// in-flight operation has released its read lock — only then is the handle
// closed, so no operation can ever touch a closed or recycled handle.
func (c *pipeConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.handleMu.RLock()
		_ = windows.SetEvent(c.cancelEv)
		_ = windows.CancelIoEx(c.handle, nil)
		c.handleMu.RUnlock()

		c.handleMu.Lock()
		defer c.handleMu.Unlock()
		c.closed = true
		if c.isServer {
			// Flush so a client blocked on the reply is not cut off mid-message,
			// then break the connection explicitly.
			_ = windows.FlushFileBuffers(c.handle)
			_ = windows.DisconnectNamedPipe(c.handle)
		}
		err = windows.CloseHandle(c.handle)
		c.destroyEvents()
	})
	return err
}

// clientPID reports the process id at the other end of a server-side instance.
func (c *pipeConn) clientPID() (uint32, error) {
	if !c.isServer {
		return 0, errors.New("clientPID called on a client-side pipe")
	}
	c.handleMu.RLock()
	defer c.handleMu.RUnlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(c.handle, &pid); err != nil {
		return 0, fmt.Errorf("read client process id: %w", err)
	}
	return pid, nil
}

// serverPID reports the process id serving a client-side instance.
func (c *pipeConn) serverPID() (uint32, error) {
	c.handleMu.RLock()
	defer c.handleMu.RUnlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(c.handle, &pid); err != nil {
		return 0, fmt.Errorf("read server process id: %w", err)
	}
	return pid, nil
}

// pipeListener accepts connections on one pipe name.
//
// Each Accept creates a fresh instance, so the listener is not a single-shot.
// Only the first instance carries FILE_FLAG_FIRST_PIPE_INSTANCE: that flag makes
// creation fail if the name already exists, which is exactly the anti-squatting
// property T21 asks for, and it must not be set on later instances because by
// then the name legitimately exists.
type pipeListener struct {
	name string
	sa   *windows.SecurityAttributes
	sd   *windows.SECURITY_DESCRIPTOR

	mu     sync.Mutex
	first  bool
	closed bool

	cancelEv  windows.Handle
	closeOnce sync.Once
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr(l.name) }

func (l *pipeListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, net.ErrClosed
	}
	flags := uint32(windows.PIPE_ACCESS_DUPLEX | windows.FILE_FLAG_OVERLAPPED)
	if l.first {
		flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	l.mu.Unlock()

	name, err := windows.UTF16PtrFromString(l.name)
	if err != nil {
		return nil, fmt.Errorf("encode pipe name: %w", err)
	}
	h, err := windows.CreateNamedPipe(
		name,
		flags,
		// PIPE_REJECT_REMOTE_CLIENTS keeps the endpoint local even though the
		// pipe namespace is reachable over SMB. There is never a network
		// listener (docs/03-threat-model.md T11).
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		windows.PIPE_UNLIMITED_INSTANCES,
		pipeBufferSize,
		pipeBufferSize,
		0,
		l.sa,
	)
	if err != nil {
		if l.first && errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, fmt.Errorf("pipe %s already exists: another process holds this control endpoint: %w", l.name, err)
		}
		return nil, fmt.Errorf("create pipe %s: %w", l.name, err)
	}
	l.mu.Lock()
	l.first = false
	l.mu.Unlock()

	conn, err := newPipeConn(h, l.name, true)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}

	if err := l.connect(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// connect waits for a client on this instance. ERROR_PIPE_CONNECTED means one
// arrived between creation and the ConnectNamedPipe call, which is success.
func (l *pipeListener) connect(conn *pipeConn) error {
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return fmt.Errorf("create accept event: %w", err)
	}
	defer func() { _ = windows.CloseHandle(ev) }()

	ov := &windows.Overlapped{HEvent: ev}
	err = windows.ConnectNamedPipe(conn.handle, ov)
	switch {
	case err == nil:
	case errors.Is(err, windows.ERROR_PIPE_CONNECTED):
		return nil
	case errors.Is(err, windows.ERROR_IO_PENDING):
		event, waitErr := windows.WaitForMultipleObjects([]windows.Handle{ev, l.cancelEv}, false, windows.INFINITE)
		switch {
		case waitErr != nil:
			_ = windows.CancelIoEx(conn.handle, ov)
			return fmt.Errorf("wait for client: %w", waitErr)
		case event == windows.WAIT_OBJECT_0:
		case event == windows.WAIT_OBJECT_0+1:
			_ = windows.CancelIoEx(conn.handle, ov)
			var done uint32
			_ = windows.GetOverlappedResult(conn.handle, ov, &done, true)
			return net.ErrClosed
		default:
			_ = windows.CancelIoEx(conn.handle, ov)
			return fmt.Errorf("unexpected wait result %#x", event)
		}
	default:
		return fmt.Errorf("connect pipe: %w", err)
	}

	var done uint32
	if err := windows.GetOverlappedResult(conn.handle, ov, &done, true); err != nil {
		return fmt.Errorf("complete connect: %w", err)
	}
	return nil
}

func (l *pipeListener) Close() error {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closed = true
		l.mu.Unlock()
		_ = windows.SetEvent(l.cancelEv)
	})
	return nil
}
