//go:build !windows

package platform

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// shortTempDir is t.TempDir with a base short enough to leave room for a socket
// name: t.TempDir builds its path from TMPDIR plus the test's own name, and on a
// macOS runner TMPDIR alone is a ~50-byte /var/folders path, so a descriptive
// test name pushes the socket past the 104-byte sun_path limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bridge")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestListenCreatesOwnerOnlySocket(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "ctl.sock")
	ln, err := NewControlEndpoint().Listen(sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("socket mode = %o, want 600", mode)
	}
}

func TestListenClearsStaleSocket(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "ctl.sock")
	ep := NewControlEndpoint()
	ln1, err := ep.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ln1.Close() // leaves the socket file behind, as a crash would

	ln2, err := ep.Listen(sock)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	ln2.Close()
}

func TestListenRefusesOverlongPath(t *testing.T) {
	long := filepath.Join(t.TempDir(), string(make([]byte, sunPathMax))+".sock")
	if _, err := NewControlEndpoint().Listen(long); err == nil {
		t.Fatal("expected refusal of a path beyond the portable sun_path limit")
	}
}

func TestVerifyPeerAcceptsSameUser(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "ctl.sock")
	ep := NewControlEndpoint()
	ln, err := ep.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		done <- ep.VerifyPeer(conn)
	}()

	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if err := <-done; err != nil {
		t.Fatalf("VerifyPeer rejected a same-user peer: %v", err)
	}
}

func TestVerifyPeerRejectsNonUnixConn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("tcp unavailable: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err == nil {
			defer c.Close()
		}
	}()
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := NewControlEndpoint().VerifyPeer(conn); err == nil {
		t.Fatal("VerifyPeer must refuse a non-unix connection")
	}
}
