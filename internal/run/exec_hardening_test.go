package run

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/platform"
	"github.com/oxcom/agents-mcp-bridge/internal/stream"
)

// shortGrace shrinks readerGrace for one test.
func shortGrace(t *testing.T, d time.Duration) {
	t.Helper()
	old := readerGrace.Load()
	readerGrace.Store(int64(d))
	t.Cleanup(func() { readerGrace.Store(old) })
}

// waiterGroup adds an ExitWaiter to a real group and records the attached cmd.
type waiterGroup struct {
	platform.ProcessGroup
	waitErr error
	cmd     *exec.Cmd
}

func (g *waiterGroup) Attach(cmd *exec.Cmd) error { g.cmd = cmd; return g.ProcessGroup.Attach(cmd) }
func (g *waiterGroup) WaitExit() error            { return g.waitErr }

// AfterStart forwards the two-phase binding (Windows starts the child suspended
// and only AfterStart resumes it); embedding the interface does not promote it.
func (g *waiterGroup) AfterStart() error {
	if ps, ok := g.ProcessGroup.(platform.PostStarter); ok {
		return ps.AfterStart()
	}
	return nil
}

// postGroup adds a PostStarter to a real group and records the attached cmd.
type postGroup struct {
	platform.ProcessGroup
	err   error
	panic bool
	cmd   *exec.Cmd
}

func (g *postGroup) Attach(cmd *exec.Cmd) error { g.cmd = cmd; return g.ProcessGroup.Attach(cmd) }
func (g *postGroup) AfterStart() error {
	if g.panic {
		panic("boom in AfterStart")
	}
	return g.err
}

func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd")
	}
	return len(ents)
}

// A transcript that fails must not strand a stream-json child that waits for
// stdin EOF after its result.
func TestATranscriptFailureStillEndsAStreamJSONChild(t *testing.T) {
	spec := steerSpec(t, "hello")
	_ = spec.Transcript.Close() // every Append now fails
	spec.Timeout = 4 * time.Second

	start := time.Now()
	r, err := NewRegistry(4, time.Hour).Start(spec, platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	// The first echoed event fails to append; the result that follows must
	// still be seen.
	time.Sleep(300 * time.Millisecond)
	if err := r.Steer("finish", OriginOperator, -1); err != nil {
		t.Fatal(err)
	}
	s := r.Await(15 * time.Second)
	if s.State != StateCompleted {
		t.Fatalf("state = %q (%s), want completed", s.State, s.Failure)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("run took %s, the child was left waiting for stdin EOF", took)
	}
	if !strings.Contains(s.Diagnostic, "transcript write failed") {
		t.Fatalf("diagnostic = %q, want the transcript failure kept", s.Diagnostic)
	}
}

// A WaitExit that fails says nothing about the child. A live child must not be
// killed and reported as status -1.
func TestAFailingWaitExitDoesNotKillALiveChild(t *testing.T) {
	shortGrace(t, 200*time.Millisecond)
	spec := waitOrderSpec(t, "run-waiterr", "sleep", "800ms")
	g := &waiterGroup{ProcessGroup: platform.NewProcessGroup(), waitErr: errors.New("waitid: injected")}

	r, err := NewRegistry(4, time.Hour).Start(spec, g)
	if err != nil {
		t.Fatal(err)
	}
	s := r.Await(15 * time.Second)
	if s.State != StateCompleted {
		t.Fatalf("state = %q (%s), want completed", s.State, s.Failure)
	}
	if s.ExitCode == nil || *s.ExitCode != 0 {
		t.Fatalf("exit code = %v, want 0", s.ExitCode)
	}
	if !strings.Contains(s.Diagnostic, "injected") {
		t.Fatalf("diagnostic = %q, want the WaitExit failure", s.Diagnostic)
	}
}

func TestAFailedAfterStartReapsTheChildAndLeaksNoDescriptors(t *testing.T) {
	for _, tc := range []struct {
		name string
		g    *postGroup
	}{
		{"error", &postGroup{ProcessGroup: platform.NewProcessGroup(), err: errors.New("bind failed")}},
		{"panic", &postGroup{ProcessGroup: platform.NewProcessGroup(), panic: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runtime.GOOS != "linux" {
				t.Skip("descriptor count needs /proc")
			}
			spec := waitOrderSpec(t, "run-after-"+tc.name, "sleep", "30s")
			before := openFDs(t)
			r, err := NewRegistry(4, time.Hour).Start(spec, tc.g)
			if err != nil {
				t.Fatal(err)
			}
			s := r.Await(15 * time.Second)
			if s.State != StateFailed {
				t.Fatalf("state = %q, want failed", s.State)
			}
			if tc.g.cmd.ProcessState == nil {
				t.Fatal("the killed child was never reaped")
			}
			deadline := time.Now().Add(2 * time.Second)
			for openFDs(t) > before && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if after := openFDs(t); after > before {
				t.Fatalf("descriptors: %d before, %d after; the pipe read ends leaked", before, after)
			}
		})
	}
}

type stuckParser struct{ stream.CodexParser }

func (stuckParser) Parse(line []byte) (stream.Event, bool) {
	time.Sleep(1500 * time.Millisecond)
	return stream.Event{}, false
}

// Readers that outlive the drain budget leave the output partial; the run must
// say so rather than report a complete result.
func TestReadersThatMissTheDrainBudgetMarkTheResultTruncated(t *testing.T) {
	shortGrace(t, 100*time.Millisecond)
	spec := waitOrderSpec(t, "run-stuck", "manyagent")
	spec.Parser = stuckParser{}

	r, err := NewRegistry(4, time.Hour).Start(spec, platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	s := r.Await(15 * time.Second)
	if !s.Truncated {
		t.Fatalf("Truncated = false with a reader still running (state %q)", s.State)
	}
	if !strings.Contains(s.Diagnostic, "output readers did not finish within the drain budget") {
		t.Fatalf("diagnostic = %q", s.Diagnostic)
	}
}

// With an ExitWaiter the leader is an unreaped zombie, so the group can be
// killed at once and a descendant holding the pipe does not cost the grace.
func TestADescendantHoldingThePipeCostsNoGraceWhereExitIsObservable(t *testing.T) {
	if _, ok := platform.NewProcessGroup().(platform.ExitWaiter); !ok {
		t.Skip("platform has no ExitWaiter")
	}
	const grace = 3 * time.Second
	shortGrace(t, grace)
	spec := waitOrderSpec(t, "run-fast-orphan", "orphan", t.TempDir()+"/pid")
	spec.Parser = stream.CodexParser{}

	start := time.Now()
	r, err := NewRegistry(4, time.Hour).Start(spec, platform.NewProcessGroup())
	if err != nil {
		t.Fatal(err)
	}
	s := r.Await(15 * time.Second)
	if s.State != StateCompleted {
		t.Fatalf("state = %q (%s)", s.State, s.Failure)
	}
	if took := time.Since(start); took > grace/2 {
		t.Fatalf("run took %s, want well under the %s reader grace", took, grace)
	}
}
