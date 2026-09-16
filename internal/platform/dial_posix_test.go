//go:build !windows

package platform

import (
	"path/filepath"
	"testing"
	"time"
)

// TestDialControlReachesTheEndpointAndVerifiesThePeer exercises the new dial
// seam against a real listener: the name the endpoint was created with must be
// the name DialControl accepts, and the resulting connection must pass
// VerifyPeer. On Windows the same pair has to agree on a derived pipe name, so
// this test pins the contract both implementations owe their call sites.
func TestDialControlReachesTheEndpointAndVerifiesThePeer(t *testing.T) {
	ep := NewControlEndpoint()
	name := filepath.Join(t.TempDir(), "control.sock")

	ln, err := ep.Listen(name)
	if err != nil {
		t.Fatalf("Listen(%q): %v", name, err)
	}
	defer func() { _ = ln.Close() }()

	accepted := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			accepted <- err
			return
		}
		defer func() { _ = conn.Close() }()
		accepted <- ep.VerifyPeer(conn)
	}()

	conn, err := DialControl(name, 2*time.Second)
	if err != nil {
		t.Fatalf("DialControl(%q): %v", name, err)
	}
	defer func() { _ = conn.Close() }()

	select {
	case err := <-accepted:
		if err != nil {
			t.Fatalf("VerifyPeer rejected a same-user peer: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the listener never accepted the dialled connection")
	}
}

// TestDialControlFailsOnAnAbsentEndpoint pins that dialling an endpoint nobody
// serves is an error rather than a hang, on both platforms.
func TestDialControlFailsOnAnAbsentEndpoint(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nobody-is-listening.sock")
	conn, err := DialControl(missing, 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("DialControl(%q) succeeded against nothing", missing)
	}
}

// TestPosixGroupImplementsPostStarter pins that both platforms satisfy the
// optional seam, so the run supervisor calls AfterStart unconditionally rather
// than branching on the operating system.
func TestPosixGroupImplementsPostStarter(t *testing.T) {
	g := NewProcessGroup()
	ps, ok := g.(PostStarter)
	if !ok {
		t.Fatal("the POSIX process group does not implement PostStarter")
	}
	if err := ps.AfterStart(); err != nil {
		t.Fatalf("AfterStart on POSIX must be a no-op, got %v", err)
	}
}
