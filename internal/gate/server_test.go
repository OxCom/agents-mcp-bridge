package gate

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/platform"
)

type fakeResolver struct{ last Ask }

func (f *fakeResolver) Resolve(_ context.Context, a Ask) Reply {
	f.last = a
	return Reply{Behavior: "deny", Message: "blue"}
}

func dialAndSend(t *testing.T, socket string, a Ask) Reply {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(a); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var r Reply
	if err := json.NewDecoder(conn).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return r
}

func TestGateForwardsAValidAskAndReturnsTheVerdict(t *testing.T) {
	f := &fakeResolver{}
	s, err := Listen(t.TempDir(), "run-1", "tok-1", platform.NewControlEndpoint(), f, slog.Default())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Close()

	got := dialAndSend(t, s.Socket(), Ask{Token: "tok-1", RunID: "run-1", Tool: "AskUserQuestion", ToolUseID: "toolu_1"})
	if got.Behavior != "deny" || got.Message != "blue" {
		t.Fatalf("verdict = %#v", got)
	}
	if f.last.Tool != "AskUserQuestion" {
		t.Fatalf("resolver saw %#v", f.last)
	}
}

func TestGateRefusesAWrongToken(t *testing.T) {
	f := &fakeResolver{}
	s, err := Listen(t.TempDir(), "run-1", "tok-1", platform.NewControlEndpoint(), f, slog.Default())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Close()

	got := dialAndSend(t, s.Socket(), Ask{Token: "guessed", RunID: "run-1", Tool: "Bash"})
	if got.Behavior != "deny" || got.Error == "" {
		t.Fatalf("a bad token must be refused, got %#v", got)
	}
	if f.last.Tool != "" {
		t.Fatal("resolver must never see an unauthenticated ask")
	}
}

func TestGateSocketIsOwnerOnly(t *testing.T) {
	s, err := Listen(t.TempDir(), "run-1", "tok-1", platform.NewControlEndpoint(), &fakeResolver{}, slog.Default())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Close()
	fi, err := os.Stat(s.Socket())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}
}

// TestGateSocketIsNotASiblingOfTheOperatorIndex covers C1: the gate socket
// must not sit in the same directory as internal/control's servers.json, so
// a child handed the gate socket path (cmd/bridge/gateconfig.go) is not
// also handed a pointer into the directory listing every live bridge's
// operator control socket.
func TestGateSocketIsNotASiblingOfTheOperatorIndex(t *testing.T) {
	dir := t.TempDir()
	// Mirror what internal/control.Listen writes at the top of the runtime
	// dir, without importing internal/control (which would create a rightward
	// dependency out of gate's declared order).
	index := filepath.Join(dir, "servers.json")
	if err := os.WriteFile(index, []byte("[]"), 0o600); err != nil {
		t.Fatalf("write fake index: %v", err)
	}

	s, err := Listen(dir, "run-1", "tok-1", platform.NewControlEndpoint(), &fakeResolver{}, slog.Default())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Close()

	if filepath.Dir(s.Socket()) == dir {
		t.Fatalf("gate socket %q sits directly in the runtime dir, alongside servers.json", s.Socket())
	}
	fi, err := os.Stat(filepath.Dir(s.Socket()))
	if err != nil {
		t.Fatalf("stat gate socket dir: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("gate socket dir mode = %o, want 700", perm)
	}
}

// TestGateSocketPathTooLongIsAnErrorNotAPanic covers the C1 fix's other
// requirement: moving the socket into a subdirectory lengthens the path, so
// the 104-byte portable unix-socket cap (platform/endpoint_posix.go) must
// still surface as an error.
func TestGateSocketPathTooLongIsAnErrorNotAPanic(t *testing.T) {
	dir := t.TempDir()
	longRunID := strings.Repeat("x", 200)
	if _, err := Listen(dir, longRunID, "tok-1", platform.NewControlEndpoint(), &fakeResolver{}, slog.Default()); err == nil {
		t.Fatal("expected an error for a socket path over the portable limit")
	}
}

func TestGateDeniesAWrongRunID(t *testing.T) {
	f := &fakeResolver{}
	s, err := Listen(t.TempDir(), "run-1", "tok-1", platform.NewControlEndpoint(), f, slog.Default())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Close()

	got := dialAndSend(t, s.Socket(), Ask{Token: "tok-1", RunID: "some-other-run", Tool: "Bash"})
	if got.Behavior != "deny" || got.Error == "" {
		t.Fatalf("a mismatched run id must be refused, got %#v", got)
	}
	if f.last.Tool != "" {
		t.Fatal("resolver must never see an ask for a different run")
	}
}

// TestGateClosesAnIdleConnection asserts the server does not park a goroutine
// forever on a peer that connects and never writes: it must enforce the read
// deadline and close its side. The test uses the unexported listen
// constructor to fix a short readTimeout before the accept loop starts,
// instead of sleeping for the real 10s value or mutating the field on an
// already-running server (which would itself race with the accept goroutine).
func TestGateClosesAnIdleConnection(t *testing.T) {
	s, err := listen(t.TempDir(), "run-1", "tok-1", platform.NewControlEndpoint(), &fakeResolver{}, slog.Default(), 50*time.Millisecond, defaultReplyWriteTimeout)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Close()

	conn, err := net.Dial("unix", s.Socket())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Never write anything. If the fix were reverted (no read deadline),
	// this read would block until the test's own generous deadline fires
	// and the test would fail instead of observing a prompt close.
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatalf("expected the server to close an idle connection, read succeeded")
	}
}

// TestGateRefusesOversizedRequest asserts a request larger than
// requestMaxBytes is refused before the resolver is ever invoked.
func TestGateRefusesOversizedRequest(t *testing.T) {
	f := &fakeResolver{}
	s, err := Listen(t.TempDir(), "run-1", "tok-1", platform.NewControlEndpoint(), f, slog.Default())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Close()

	conn, err := net.Dial("unix", s.Socket())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	big := make([]byte, requestMaxBytes+1024)
	for i := range big {
		big[i] = 'a'
	}
	oversized, err := json.Marshal(Ask{Token: "tok-1", RunID: "run-1", Tool: "Bash", Input: json.RawMessage(`"` + string(big) + `"`)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := conn.Write(oversized); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatalf("expected an oversized request to be refused without a reply")
	}

	if f.last.Tool != "" {
		t.Fatal("resolver must never see an oversized request")
	}
}

// TestGateCapsConcurrentConnections asserts a connection beyond maxInFlight
// is denied immediately rather than queued.
func TestGateCapsConcurrentConnections(t *testing.T) {
	// No need to shorten requestReadTimeout here: the filling connections
	// are closed by this test well within the default deadline, and
	// mutating the shared package var while their goroutines are still
	// starting up would itself be a data race.
	s, err := Listen(t.TempDir(), "run-1", "tok-1", platform.NewControlEndpoint(), &fakeResolver{}, slog.Default())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer s.Close()

	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	// Fill every in-flight slot with a connection that never writes, so
	// each one occupies a serve() goroutine until the read deadline fires.
	for i := 0; i < maxInFlight; i++ {
		c, err := net.Dial("unix", s.Socket())
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		conns = append(conns, c)
	}

	// Let the accept loop pick up each connection and acquire its slot.
	time.Sleep(100 * time.Millisecond)

	extra, err := net.Dial("unix", s.Socket())
	if err != nil {
		t.Fatalf("dial extra: %v", err)
	}
	defer extra.Close()

	// The overflow rejection happens in accept() the moment the connection
	// is accepted, before anything is read from it — writing a request here
	// would race the server's own close of the connection, so the test only
	// reads the reply.
	if err := extra.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var r Reply
	if err := json.NewDecoder(extra).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// fakeResolver always answers Behavior: "deny", Message: "blue" with no
	// Error set, so asserting Behavior alone cannot distinguish a
	// resolver-produced deny from the gate's own overflow rejection — that
	// gap is exactly what let a reverted cap pass this test undetected.
	// "gate busy" is a value only the gate's own reject() path produces.
	if r.Behavior != "deny" || r.Error != "gate busy" {
		t.Fatalf("expected the gate itself (not the resolver) to deny a connection beyond the cap, got %#v", r)
	}
	if r.Message != "" {
		t.Fatalf("expected no resolver-produced message on a cap rejection, got %#v", r)
	}
}
