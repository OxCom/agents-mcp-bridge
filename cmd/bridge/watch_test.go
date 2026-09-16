package main

import (
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/control"
	"github.com/oxcom/agents-mcp-bridge/internal/platform"
	"github.com/oxcom/agents-mcp-bridge/internal/run"
)

// TestAttachToRunCarriesTheRunID pins the C-NEW fix: the only production
// caller of VerbAttach (runWatch, via attachToRun) must send the run id it
// resolved to watch. Before this fix, runWatch sent
// control.Request{Verb: control.VerbAttach} with no RunID — a "picker"
// attach (internal/control/server.go) that never populates byRun, so
// WatchersAttachedTo(runID) stayed false for every run and the gate's
// resolver could never route a question to the operator TUI. This test
// drives attachToRun itself, the exact call cmd/bridge/watch.go:runWatch
// makes, against a real control.Server — not a hand-built request — so it
// fails the same way the regression did if the run id is ever dropped
// again.
func TestAttachToRunCarriesTheRunID(t *testing.T) {
	b, _ := newTestBridge(t, self(t), stubArgs("--sleep", "30s"))
	b.log = slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := control.Listen(shortTempDir(t), platform.NewControlEndpoint(), b, b.log)
	if err != nil {
		t.Fatalf("control.Listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	c, err := control.Dial(srv.Socket())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	const runID = "run-watched"
	if err := attachToRun(c, runID); err != nil {
		t.Fatalf("attachToRun: %v", err)
	}

	if !srv.WatchersAttachedTo(runID) {
		t.Fatal("attachToRun did not register against its run id — the C-NEW regression: " +
			"the production attach path must never send a run-id-less \"picker\" attach")
	}
	if srv.WatchersAttachedTo("some-other-run") {
		t.Fatal("attachToRun must not appear to watch a run it was never given")
	}

	if _, err := c.Do(control.Request{Verb: control.VerbDetach}); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if srv.WatchersAttachedTo(runID) {
		t.Fatal("detach did not release the watcher")
	}
}

// TestRunsCommandListsLiveRuns pins `bridge runs` (docs/04-config-schema.md:438):
// main.go had no case for control.VerbRuns despite the verb, the server-side
// handler, and collectRuns/printRuns all already existing, so the documented
// command did not exist. This drives runRuns itself — the function main.go's
// "runs" case now calls — against a real control server backed by a live run,
// isolated to a per-test XDG_RUNTIME_DIR so it never touches this machine's
// actual bridge servers.
func TestRunsCommandListsLiveRuns(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", shortTempDir(t))

	b, _ := newTestBridge(t, self(t), stubArgs("--sleep", "5s"))
	b.log = slog.New(slog.NewTextHandler(io.Discard, nil))

	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatalf("NewPaths: %v", err)
	}
	srv, err := control.Listen(paths.Runtime(), platform.NewControlEndpoint(), b, b.log)
	if err != nil {
		t.Fatalf("control.Listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	if _, err := b.runs.Start(run.Spec{
		RunID: "run-listed", Agent: "echoer", HostAgent: "claude",
		Command: self(t), Args: stubArgs("--sleep", "5s"), CWD: t.TempDir(),
		Env:     []string{"PATH=" + os.Getenv("PATH")},
		Timeout: 5 * time.Second, MaxOutput: 1 << 10,
	}, platform.NewProcessGroup()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(b.runs.CancelAll)

	runs, err := collectRuns(paths.Runtime())
	if err != nil {
		t.Fatalf("collectRuns: %v", err)
	}
	found := false
	for _, r := range runs {
		if r.info.RunID == "run-listed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("collectRuns did not report the live run; got %+v", runs)
	}

	// runRuns is exactly what main.go's "runs" case invokes; it must succeed
	// with the run visible above and with none at all.
	if err := runRuns(nil); err != nil {
		t.Fatalf("runRuns: %v", err)
	}
	_ = srv.Close()
	if err := runRuns(nil); err != nil {
		t.Fatalf("runRuns with no servers must report, not error: %v", err)
	}
}
