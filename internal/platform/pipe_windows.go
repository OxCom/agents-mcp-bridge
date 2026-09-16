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
	"sync/atomic"
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
//
// A single dot survives, so a base name like "1234.sock" stays recognisable to
// an operator, but never two in a row: allowing that let "..\..\evil" through
// as "..-..-evil", which still carries a path-shaped token.
func sanitisePipeComponent(s string) string {
	var b strings.Builder
	var last rune
	for _, r := range s {
		out := '-'
		switch {
		case r == '.' && last == '.':
			// A second consecutive dot would rebuild "..".
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out = r
		}
		b.WriteRune(out)
		last = out
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

	// One expiry event per direction: a read deadline must not unblock a writer.
	readDlEv  windows.Handle
	writeDlEv windows.Handle

	deadlineMu sync.Mutex
	readDl     deadline
	writeDl    deadline
	dlDead     bool

	closeOnce sync.Once
	closed    bool
	// aborted marks the handle as already closed by cancel's last-resort path,
	// so Close neither double-closes nor touches a recycled handle value.
	aborted atomic.Bool
}

// deadline is one direction's deadline and the timer that signals its expiry
// event. gen discards a timer that fires after the deadline moved.
type deadline struct {
	t     time.Time
	timer *time.Timer
	gen   uint64
}

func newPipeConn(h windows.Handle, addr string, isServer bool) (*pipeConn, error) {
	c := &pipeConn{handle: h, addr: pipeAddr(addr), isServer: isServer}
	for _, ev := range []*windows.Handle{&c.readEv, &c.writeEv, &c.cancelEv, &c.readDlEv, &c.writeDlEv} {
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
	for _, ev := range []windows.Handle{c.readEv, c.writeEv, c.cancelEv, c.readDlEv, c.writeDlEv} {
		if ev != 0 {
			_ = windows.CloseHandle(ev)
		}
	}
}

func (c *pipeConn) LocalAddr() net.Addr  { return c.addr }
func (c *pipeConn) RemoteAddr() net.Addr { return c.addr }

func (c *pipeConn) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.SetWriteDeadline(t))
}

func (c *pipeConn) SetReadDeadline(t time.Time) error {
	return c.setDeadline(&c.readDl, c.readDlEv, t)
}

func (c *pipeConn) SetWriteDeadline(t time.Time) error {
	return c.setDeadline(&c.writeDl, c.writeDlEv, t)
}

// setDeadline records the deadline and arms the timer that signals its event,
// so a deadline set on an operation already in flight interrupts it. net.Conn
// requires that, and SetReadDeadline(time.Now()) is the idiom for unblocking a
// stuck reader.
func (c *pipeConn) setDeadline(d *deadline, ev windows.Handle, t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	if c.dlDead {
		return net.ErrClosed
	}
	d.gen++
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	d.t = t
	if t.IsZero() {
		return windows.ResetEvent(ev)
	}
	remaining := time.Until(t)
	if remaining <= 0 {
		// Already past: the event stays signalled so this operation and the next
		// both fail, until the deadline is moved again.
		return windows.SetEvent(ev)
	}
	if err := windows.ResetEvent(ev); err != nil {
		return err
	}
	gen := d.gen
	d.timer = time.AfterFunc(remaining, func() { c.expire(d, ev, gen) })
	return nil
}

// expire signals the direction's event unless the deadline moved, or the
// connection closed and the event handle is gone.
func (c *pipeConn) expire(d *deadline, ev windows.Handle, gen uint64) {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	if c.dlDead || d.gen != gen {
		return
	}
	_ = windows.SetEvent(ev)
}

// stopDeadlines silences both timers before Close destroys the events, so no
// timer can signal a handle value the kernel has since handed to someone else.
func (c *pipeConn) stopDeadlines() {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.dlDead = true
	for _, d := range []*deadline{&c.readDl, &c.writeDl} {
		if d.timer != nil {
			d.timer.Stop()
			d.timer = nil
		}
	}
}

func (c *pipeConn) deadlineAt(d *deadline) time.Time {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	return d.t
}

func (c *pipeConn) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	n, err := c.do(c.readEv, &c.readDl, c.readDlEv, func(ov *windows.Overlapped) error {
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
	written := 0
	for written < len(b) {
		chunk := b[written:]
		n, err := c.do(c.writeEv, &c.writeDl, c.writeDlEv, func(ov *windows.Overlapped) error {
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
func (c *pipeConn) do(ev windows.Handle, d *deadline, dlEv windows.Handle, issue func(*windows.Overlapped) error) (uint32, error) {
	c.handleMu.RLock()
	defer c.handleMu.RUnlock()
	if c.closed || c.aborted.Load() {
		return 0, net.ErrClosed
	}
	if dl := c.deadlineAt(d); !dl.IsZero() && !time.Now().Before(dl) {
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
		// The recovered count matters: the kernel may have transferred part of
		// the chunk before the deadline, and a Write that under-reports it would
		// be retried from the wrong offset and duplicate bytes on the wire.
		if recovered, waitErr := c.await(ev, ov, d, dlEv); waitErr != nil {
			return recovered, waitErr
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

// await blocks until the operation completes, the deadline passes or is moved
// into the past, or Close signals cancelEv. It reports the bytes the kernel had
// already transferred when it gives up.
func (c *pipeConn) await(ev windows.Handle, ov *windows.Overlapped, d *deadline, dlEv windows.Handle) (uint32, error) {
	for {
		timeout := uint32(windows.INFINITE)
		if dl := c.deadlineAt(d); !dl.IsZero() {
			remaining := time.Until(dl)
			if remaining <= 0 {
				return c.cancel(ov, os.ErrDeadlineExceeded)
			}
			timeout = waitTimeout(remaining)
		}
		event, err := windows.WaitForMultipleObjects([]windows.Handle{ev, c.cancelEv, dlEv}, false, timeout)
		switch {
		case err != nil:
			return c.cancel(ov, fmt.Errorf("wait for pipe I/O: %w", err))
		case event == windows.WAIT_OBJECT_0:
			return 0, nil
		case event == windows.WAIT_OBJECT_0+1:
			return c.cancel(ov, net.ErrClosed)
		case event == windows.WAIT_OBJECT_0+2:
			return c.cancel(ov, os.ErrDeadlineExceeded)
		case event == uint32(windows.WAIT_TIMEOUT):
			// The timeout is only a backstop for the event. Re-read the deadline:
			// it may have been extended while this wait was outstanding.
			continue
		default:
			return c.cancel(ov, fmt.Errorf("unexpected wait result %#x", event))
		}
	}
}

// waitTimeout narrows a remaining duration to the millisecond count
// WaitForMultipleObjects takes. INFINITE is a sentinel, not a duration, so a
// wait long enough to reach it clamps one millisecond below.
func waitTimeout(remaining time.Duration) uint32 {
	ms := remaining.Milliseconds() + 1
	if ms <= 0 || ms >= int64(windows.INFINITE) {
		return windows.INFINITE - 1
	}
	return uint32(ms)
}

// cancel aborts an in-flight operation and waits for the cancellation to be
// acknowledged before returning cause, along with whatever the kernel had
// already transferred. Returning while the kernel still owns the caller's buffer
// would be a use-after-return.
func (c *pipeConn) cancel(ov *windows.Overlapped, cause error) (uint32, error) {
	if err := windows.CancelIoEx(c.handle, ov); err != nil && !errors.Is(err, windows.ERROR_NOT_FOUND) {
		// ERROR_NOT_FOUND only means the operation finished first. Any other
		// failure leaves the I/O uncancellable, and GetOverlappedResult would
		// then block past the caller's deadline; closing forces completion.
		c.abort()
		return 0, cause
	}
	var done uint32
	_ = windows.GetOverlappedResult(c.handle, ov, &done, true)
	return done, cause
}

// abort is cancel's last resort. It closes the handle outside Close, so the
// aborted flag is what keeps Close from closing a value the kernel has recycled.
func (c *pipeConn) abort() {
	if c.aborted.CompareAndSwap(false, true) {
		// Signal first: an operation running in the other direction is on this
		// same handle, and waking it with net.ErrClosed beats letting it fail
		// with ERROR_INVALID_HANDLE once the close lands.
		_ = windows.SetEvent(c.cancelEv)
		_ = windows.CloseHandle(c.handle)
	}
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
		if !c.aborted.Load() {
			_ = windows.CancelIoEx(c.handle, nil)
		}
		c.handleMu.RUnlock()

		c.handleMu.Lock()
		defer c.handleMu.Unlock()
		c.closed = true
		if c.aborted.CompareAndSwap(false, true) {
			if c.isServer {
				// Flush so a client blocked on the reply is not cut off
				// mid-message, then break the connection explicitly.
				_ = windows.FlushFileBuffers(c.handle)
				_ = windows.DisconnectNamedPipe(c.handle)
			}
			err = windows.CloseHandle(c.handle)
		}
		c.stopDeadlines()
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
	if c.closed || c.aborted.Load() {
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
	if c.closed || c.aborted.Load() {
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

	// wg counts the Accepts that may still be waiting on cancelEv, so Close can
	// destroy the event only once none of them can reference it.
	wg        sync.WaitGroup
	cancelEv  windows.Handle
	closeOnce sync.Once
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr(l.name) }

func (l *pipeListener) Accept() (net.Conn, error) {
	name, err := windows.UTF16PtrFromString(l.name)
	if err != nil {
		return nil, fmt.Errorf("encode pipe name: %w", err)
	}

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, net.ErrClosed
	}
	l.wg.Add(1)
	defer l.wg.Done()
	flags := uint32(windows.PIPE_ACCESS_DUPLEX | windows.FILE_FLAG_OVERLAPPED)
	first := l.first
	if first {
		flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
		// Claim the flag before unlocking, and restore it below if the create
		// fails. Two Accepts that both carried it would collide on the name and
		// report our own instance as a squatter.
		l.first = false
	}
	l.mu.Unlock()

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
		if first {
			l.mu.Lock()
			l.first = true
			l.mu.Unlock()
			if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				return nil, fmt.Errorf("pipe %s already exists: another process holds this control endpoint: %w", l.name, err)
			}
		}
		return nil, fmt.Errorf("create pipe %s: %w", l.name, err)
	}

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
		// No Accept can start once closed is set, so once the in-flight ones have
		// observed the event the handle has no remaining reader.
		l.wg.Wait()
		_ = windows.CloseHandle(l.cancelEv)
	})
	return nil
}
