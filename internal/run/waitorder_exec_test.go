package run

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/platform"
	"github.com/oxcom/agents-mcp-bridge/internal/stream"
)

// slowParser makes the reader lag behind the child, so the child has exited
// while most of its output still sits in the kernel pipe buffer.
type slowParser struct{ stream.CodexParser }

func (p slowParser) Parse(line []byte) (stream.Event, bool) {
	time.Sleep(5 * time.Millisecond)
	return p.CodexParser.Parse(line)
}

func waitOrderSpec(t *testing.T, id string, args ...string) Spec {
	t.Helper()
	dir := t.TempDir()
	tr, err := stream.CreateTranscript(dir, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return Spec{
		RunID:          id,
		Agent:          "codex",
		Command:        helperCommand(t),
		Args:           args,
		CWD:            dir,
		Env:            helperEnviron(),
		Timeout:        20 * time.Second,
		MaxOutput:      1 << 16,
		Transcript:     tr,
		TranscriptPath: tr.Path(),
	}
}

// A child that exits at once must not cost the caller its final message because
// the reader is slower than the child: Wait may not close the read end while
// data is still buffered in the pipe.
func TestAFastExitDoesNotDiscardBufferedOutputFromASlowReader(t *testing.T) {
	spec := waitOrderSpec(t, "run-slow", "manyagent")
	spec.Parser = slowParser{}

	r, err := NewRegistry(4, time.Hour).Start(spec, platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	s := r.Await(15 * time.Second)
	if s.State != StateCompleted {
		t.Fatalf("state = %q (%s), want completed", s.State, s.Failure)
	}
	if !strings.Contains(s.Output, "FINAL") {
		t.Fatalf("Output lost the final message: %q", s.Output)
	}
}

// A descendant that outlives its parent and keeps the pipe open must not hold
// the run past the grace period, and must not survive the run.
func TestAGrandchildHoldingStdoutDoesNotHangTheRun(t *testing.T) {
	t.Run("parser", func(t *testing.T) { grandchildHoldingStdout(t, stream.CodexParser{}) })
	// No parser: the raw capped.drain path.
	t.Run("raw", func(t *testing.T) { grandchildHoldingStdout(t, nil) })
}

func grandchildHoldingStdout(t *testing.T, parser stream.Parser) {
	t.Helper()
	old := readerGrace
	readerGrace = 300 * time.Millisecond
	t.Cleanup(func() { readerGrace = old })

	pidFile := filepath.Join(t.TempDir(), "pid")
	spec := waitOrderSpec(t, "run-orphan", "orphan", pidFile)
	spec.Parser = parser

	start := time.Now()
	r, err := NewRegistry(4, time.Hour).Start(spec, platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	s := r.Await(15 * time.Second)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("run took %s with a grandchild holding stdout, want about the grace period", took)
	}
	if s.State != StateCompleted {
		t.Fatalf("state = %q (%s), want completed", s.State, s.Failure)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Fatalf("grandchild %d survived the run", pid)
	}
}
