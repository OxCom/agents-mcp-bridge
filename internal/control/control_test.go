package control

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/platform"
)

type fakeHandler struct {
	stopped     []string
	accepted    []string
	rejected    []string
	steered     []string
	answered    []string
	continued   []string
	features    []string
	stopErr     error
	continueID  string
	continueErr error
}

func (h *fakeHandler) Status() Status {
	return Status{PID: os.Getpid(), Host: "claude", Adapters: []string{"codex"}}
}

func (h *fakeHandler) Runs() []RunInfo {
	return []RunInfo{{RunID: "run-1", Agent: "codex", State: "running"}}
}

func (h *fakeHandler) Stop(id string) error {
	h.stopped = append(h.stopped, id)
	return h.stopErr
}

func (h *fakeHandler) Accept(id string) error {
	h.accepted = append(h.accepted, id)
	return nil
}

func (h *fakeHandler) Reject(id string) error {
	h.rejected = append(h.rejected, id)
	return nil
}

func (h *fakeHandler) Steer(id, text string) error {
	h.steered = append(h.steered, id+":"+text)
	return nil
}

func (h *fakeHandler) Answer(id, questionID, text string) error {
	h.answered = append(h.answered, id+":"+questionID+":"+text)
	return nil
}

func (h *fakeHandler) Continue(id, text string) (string, error) {
	h.continued = append(h.continued, id+":"+text)
	if h.continueErr != nil {
		return "", h.continueErr
	}
	return h.continueID, nil
}

func (h *fakeHandler) SetFeature(feature string, on bool) error {
	h.features = append(h.features, fmt.Sprintf("%s=%v", feature, on))
	return nil
}

func newServer(t *testing.T) (*Server, *fakeHandler, string) {
	t.Helper()
	dir := shortTempDir(t)
	h := &fakeHandler{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := Listen(dir, platform.NewControlEndpoint(), h, log)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, h, dir
}

func TestStatusAndRuns(t *testing.T) {
	s, _, _ := newServer(t)
	c, err := Dial(s.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	resp, err := c.Do(Request{Verb: VerbStatus})
	if err != nil || resp.Status == nil || resp.Status.Host != "claude" {
		t.Fatalf("status = %+v err = %v", resp.Status, err)
	}
	resp, err = c.Do(Request{Verb: VerbRuns})
	if err != nil || len(resp.Runs) != 1 {
		t.Fatalf("runs = %+v err = %v", resp.Runs, err)
	}
}

func TestStopReachesTheHandler(t *testing.T) {
	s, h, _ := newServer(t)
	c, _ := Dial(s.Socket())
	defer c.Close()
	if _, err := c.Do(Request{Verb: VerbStop, RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	if len(h.stopped) != 1 || h.stopped[0] != "run-1" {
		t.Fatalf("stop not delivered: %v", h.stopped)
	}
}

func TestStopWithoutRunIDIsRejected(t *testing.T) {
	s, _, _ := newServer(t)
	c, _ := Dial(s.Socket())
	defer c.Close()
	if _, err := c.Do(Request{Verb: VerbStop}); err == nil {
		t.Fatal("stop without a run id must be refused")
	}
}

func TestAcceptAndRejectReachTheHandler(t *testing.T) {
	// These live on the operator channel, not the MCP surface: by default a
	// human decides what reaches the working tree.
	s, h, _ := newServer(t)
	c, _ := Dial(s.Socket())
	defer c.Close()
	if _, err := c.Do(Request{Verb: VerbAccept, RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(Request{Verb: VerbReject, RunID: "run-2"}); err != nil {
		t.Fatal(err)
	}
	if len(h.accepted) != 1 || len(h.rejected) != 1 {
		t.Fatalf("accepted=%v rejected=%v", h.accepted, h.rejected)
	}
	if _, err := c.Do(Request{Verb: VerbAccept}); err == nil {
		t.Fatal("accept without a run id must be refused")
	}
}

func TestSteerAndAnswerReachTheHandler(t *testing.T) {
	s, h, _ := newServer(t)
	c, _ := Dial(s.Socket())
	defer c.Close()
	if _, err := c.Do(Request{Verb: VerbSteer, RunID: "run-1", Text: "look elsewhere"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(Request{Verb: VerbAnswer, RunID: "run-1", Text: "no"}); err != nil {
		t.Fatal(err)
	}
	if len(h.steered) != 1 || len(h.answered) != 1 {
		t.Fatalf("steered=%v answered=%v", h.steered, h.answered)
	}
	// Both need text: an empty steer would be a silent no-op.
	if _, err := c.Do(Request{Verb: VerbSteer, RunID: "run-1"}); err == nil {
		t.Fatal("steer without text must be refused")
	}
	if _, err := c.Do(Request{Verb: VerbAnswer, RunID: "run-1"}); err == nil {
		t.Fatal("answer without text must be refused")
	}
}

func TestContinueReachesTheHandlerAndReturnsTheSuccessorID(t *testing.T) {
	s, h, _ := newServer(t)
	h.continueID = "run-2"
	c, _ := Dial(s.Socket())
	defer c.Close()
	resp, err := c.Do(Request{Verb: VerbContinue, RunID: "run-1", Text: "use bcrypt"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.SuccessorID != "run-2" {
		t.Fatalf("SuccessorID = %q, want run-2", resp.SuccessorID)
	}
	if len(h.continued) != 1 || h.continued[0] != "run-1:use bcrypt" {
		t.Fatalf("continued=%v", h.continued)
	}
	if _, err := c.Do(Request{Verb: VerbContinue, RunID: "run-1"}); err == nil {
		t.Fatal("continue without text must be refused")
	}
	h.continueErr = fmt.Errorf("this continuation chain has reached its configured limit")
	resp, err = c.Do(Request{Verb: VerbContinue, RunID: "run-1", Text: "again"})
	if err == nil || resp.OK {
		t.Fatalf("expected the handler's refusal to surface as an error, got resp=%+v err=%v", resp, err)
	}
}

func TestEnableAndDisableReachTheHandler(t *testing.T) {
	s, h, _ := newServer(t)
	c, _ := Dial(s.Socket())
	defer c.Close()
	if _, err := c.Do(Request{Verb: VerbDisable, Feature: "agent_steering"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(Request{Verb: VerbEnable, Feature: "agent_steering"}); err != nil {
		t.Fatal(err)
	}
	if len(h.features) != 2 || h.features[0] != "agent_steering=false" {
		t.Fatalf("features = %v", h.features)
	}
	if _, err := c.Do(Request{Verb: VerbDisable}); err == nil {
		t.Fatal("disable without a feature must be refused")
	}
}

func TestUnknownVerbIsRejected(t *testing.T) {
	s, _, _ := newServer(t)
	c, _ := Dial(s.Socket())
	defer c.Close()
	if _, err := c.Do(Request{Verb: "exfiltrate"}); err == nil {
		t.Fatal("an unknown verb must be refused, not ignored")
	}
}

func TestSocketIsOwnerOnly(t *testing.T) {
	// POSIX-only: on Windows the endpoint is a named pipe, so Socket() names
	// \\.\pipe\... and no file exists to stat. The property still has to hold
	// there, carried by the pipe's owner-only DACL
	// (platform/endpoint_windows.go). No test asserts that DACL yet: that is
	// the Windows gap, not a reason to soften the mode check here.
	if runtime.GOOS == "windows" {
		t.Skip("no file behind a named pipe; the pipe DACL is unasserted on Windows")
	}
	s, _, _ := newServer(t)
	fi, err := os.Stat(s.Socket())
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Fatalf("socket mode = %o, want 600", mode)
	}
}

func TestServerRegistersAndDeregisters(t *testing.T) {
	s, _, dir := newServer(t)
	if got := ReadIndex(dir); len(got) != 1 || got[0].PID != os.Getpid() {
		t.Fatalf("server not registered: %+v", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := ReadIndex(dir); len(got) != 0 {
		t.Fatalf("server still listed after close: %+v", got)
	}
	// What this asserts is that nothing dialable outlives the server. On POSIX
	// that is also visible on disk, because Close unlinks the socket file. On
	// Windows a named pipe leaves no on-disk remnant at all, so os.Stat would
	// report IsNotExist for a live pipe too and would pass for the wrong
	// reason; there the check is that a dial no longer succeeds.
	if runtime.GOOS != "windows" {
		if _, err := os.Stat(s.Socket()); !os.IsNotExist(err) {
			t.Fatal("the socket file outlived the server")
		}
	}
	if conn, err := platform.DialControl(s.Socket(), 200*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("the control endpoint still accepts connections after Close")
	}
}

func TestStaleIndexEntriesArePruned(t *testing.T) {
	// A crashed server would otherwise point the TUI at a dead socket, or at a
	// recycled pid belonging to something else entirely.
	dir := t.TempDir()
	raw := `[{"pid":999999,"host":"ghost","socket":"/nonexistent.sock"}]`
	if err := os.WriteFile(filepath.Join(dir, "servers.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ReadIndex(dir); len(got) != 0 {
		t.Fatalf("a dead server survived pruning: %+v", got)
	}
}

func TestIndexIsOwnerOnly(t *testing.T) {
	// POSIX-only, because a Go file mode is the control only here. On Windows
	// platform.WriteOwnerOnlyFile gives the index an owner-only DACL and the
	// mode bits still read 0666, so asserting them there would prove nothing.
	// Reading the DACL back needs a Windows-only helper no test has yet.
	if runtime.GOOS == "windows" {
		t.Skip("on Windows the index is restricted by its DACL, which no test reads back yet")
	}
	_, _, dir := newServer(t)
	fi, err := os.Stat(filepath.Join(dir, "servers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Fatalf("index mode = %o, want 600", mode)
	}
}

func TestMultipleServersCoexist(t *testing.T) {
	// One host session per agent means several live servers; the index must
	// hold them all.
	dir := shortTempDir(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s1, err := Listen(dir, platform.NewControlEndpoint(), &fakeHandler{}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	if got := ReadIndex(dir); len(got) != 1 {
		t.Fatalf("expected 1 server, got %d", len(got))
	}
	// A second Listen from the same process reuses the pid, so the socket name
	// collides; that is expected and is why the name includes the pid. The
	// endpoint is checked by dialling it, not by stat: a named pipe has no file
	// behind its name.
	conn, err := platform.DialControl(s1.Socket(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	_ = time.Now()
}

func TestAttachAndDetachTrackWatchers(t *testing.T) {
	s, _, _ := newServer(t)
	c, err := Dial(s.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if s.WatchersAttached() {
		t.Fatal("no watcher attached yet")
	}
	if _, err := c.Do(Request{Verb: VerbAttach}); err != nil {
		t.Fatal(err)
	}
	if !s.WatchersAttached() {
		t.Fatal("attach did not register a watcher")
	}
	if _, err := c.Do(Request{Verb: VerbDetach}); err != nil {
		t.Fatal(err)
	}
	if s.WatchersAttached() {
		t.Fatal("detach did not release the watcher")
	}
}

// TestAttachTwiceAndDetachWithoutAttachAreSafe covers fix-round-1 finding 5:
// the *attached guard in dispatch makes both idempotent today, but nothing
// stops a later refactor from breaking that silently and leaving the
// watcher count wrong.
func TestAttachTwiceAndDetachWithoutAttachAreSafe(t *testing.T) {
	s, _, _ := newServer(t)
	c, err := Dial(s.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Detach before any attach must not underflow the count.
	if _, err := c.Do(Request{Verb: VerbDetach}); err != nil {
		t.Fatal(err)
	}
	if s.WatchersAttached() {
		t.Fatal("detach with nothing attached must not report a watcher")
	}

	// Attaching twice on the same connection must count as one watcher, not
	// two: a matching detach must be enough to release it.
	if _, err := c.Do(Request{Verb: VerbAttach}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(Request{Verb: VerbAttach}); err != nil {
		t.Fatal(err)
	}
	if !s.WatchersAttached() {
		t.Fatal("attach did not register a watcher")
	}
	if _, err := c.Do(Request{Verb: VerbDetach}); err != nil {
		t.Fatal(err)
	}
	if s.WatchersAttached() {
		t.Fatal("one detach must release a double-attached connection entirely")
	}
}

// TestWatcherAttachmentIsPerRun covers I1: watching run A must never make
// run B's question wait for a TUI that will never display it.
func TestWatcherAttachmentIsPerRun(t *testing.T) {
	s, _, _ := newServer(t)
	c, err := Dial(s.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Do(Request{Verb: VerbAttach, RunID: "run-A"}); err != nil {
		t.Fatal(err)
	}
	if !s.WatchersAttachedTo("run-A") {
		t.Fatal("attach did not register against run-A")
	}
	if s.WatchersAttachedTo("run-B") {
		t.Fatal("a watcher on run-A must not appear attached to run-B")
	}
	if _, err := c.Do(Request{Verb: VerbDetach}); err != nil {
		t.Fatal(err)
	}
	if s.WatchersAttachedTo("run-A") {
		t.Fatal("detach did not release run-A")
	}
}

// TestAttachWithNoRunIDIsAPickerAndClaimsNoRun pins the deliberate meaning of
// an attach with no run id (I1): a run picker that has not chosen yet counts
// as a live watcher but claims no specific run.
func TestAttachWithNoRunIDIsAPickerAndClaimsNoRun(t *testing.T) {
	s, _, _ := newServer(t)
	c, err := Dial(s.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Do(Request{Verb: VerbAttach}); err != nil {
		t.Fatal(err)
	}
	if !s.WatchersAttached() {
		t.Fatal("a run picker still counts as a live watcher")
	}
	if s.WatchersAttachedTo("run-A") {
		t.Fatal("a picker that has not chosen a run must not claim to watch one")
	}
}

func TestSwitchingWatchedRunMovesTheAttachment(t *testing.T) {
	s, _, _ := newServer(t)
	c, err := Dial(s.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Do(Request{Verb: VerbAttach, RunID: "run-A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(Request{Verb: VerbAttach, RunID: "run-B"}); err != nil {
		t.Fatal(err)
	}
	if s.WatchersAttachedTo("run-A") {
		t.Fatal("switching runs must release the old attachment")
	}
	if !s.WatchersAttachedTo("run-B") {
		t.Fatal("switching runs must register the new attachment")
	}
	if _, err := c.Do(Request{Verb: VerbDetach}); err != nil {
		t.Fatal(err)
	}
	if s.WatchersAttachedTo("run-B") {
		t.Fatal("detach must release whichever run is currently attached")
	}
}

// TestWatcherDisconnectingWithoutDetachStillReleasesPerRun mirrors
// TestWatcherDisconnectingWithoutDetachStillReleases for the per-run count:
// a watcher that dies without detaching must not leave THAT run's count high
// either.
func TestWatcherDisconnectingWithoutDetachStillReleasesPerRun(t *testing.T) {
	s, _, _ := newServer(t)
	c, err := Dial(s.Socket())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(Request{Verb: VerbAttach, RunID: "run-A"}); err != nil {
		t.Fatal(err)
	}
	if !s.WatchersAttachedTo("run-A") {
		t.Fatal("attach did not register")
	}

	c.Close()

	deadline := time.Now().Add(2 * time.Second)
	for s.WatchersAttachedTo("run-A") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.WatchersAttachedTo("run-A") {
		t.Fatal("a dead watcher connection must not leave the per-run count high")
	}
}

func TestWatcherDisconnectingWithoutDetachStillReleases(t *testing.T) {
	s, _, _ := newServer(t)
	c, err := Dial(s.Socket())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(Request{Verb: VerbAttach}); err != nil {
		t.Fatal(err)
	}
	if !s.WatchersAttached() {
		t.Fatal("attach did not register a watcher")
	}

	// The watcher dies without ever sending Detach: closing the connection
	// must release the count from the server's own defer, not from a message
	// the dead peer never sends.
	c.Close()

	deadline := time.Now().Add(2 * time.Second)
	for s.WatchersAttached() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.WatchersAttached() {
		t.Fatal("a dead watcher connection must not leave the count high")
	}
}
