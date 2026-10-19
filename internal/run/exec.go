package run

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/platform"
	"github.com/oxcom/agents-mcp-bridge/internal/stream"
)

// Spec is everything needed to start one child. Every field is already
// authorised: the run manager re-derives no policy.
type Spec struct {
	RunID     string
	Agent     string
	HostAgent string
	Command   string
	Args      []string
	Env       []string
	CWD       string
	Mode      string
	Confined  bool
	// SandboxEnforced is false when the target CLI offers no sandbox for this
	// mode. It travels with the run so every surface can say UNSANDBOXED
	// without re-reading the config.
	SandboxEnforced bool
	Depth           int
	Prompt          string
	PromptStdin     bool // deliver the prompt on stdin rather than in argv
	Timeout         time.Duration
	MaxOutput       int

	// Parser and Transcript are set for full-tier adapters. When Parser is nil
	// the run is basic tier: stdout is buffered and returned, with no events.
	Parser         stream.Parser
	Transcript     *stream.Transcript
	TranscriptPath string
	// MaxEventBytes caps one line of vendor output. A longer line is recorded
	// as oversized rather than growing the parser's buffer without limit.
	MaxEventBytes int

	// Worktree is set for a confined write run. The run manager only carries
	// it; the diff is taken by the caller once the run finishes.
	Worktree any

	// ResumedFrom is the predecessor's run id when this run is a
	// continuation's successor. Empty for an ordinary run. Start also uses
	// it to exclude the predecessor from the concurrency admission check
	// (liveCountExcludingLocked), so the successor inherits the
	// predecessor's still-occupied needs_input slot instead of needing it
	// freed in advance.
	ResumedFrom string
	// Continuation, when set, is attached to the started run so it can later
	// be continued itself if it stops on needs_input (Run.SetContinuation).
	Continuation *ContinuationRecord

	// StreamStdin keeps the child's stdin open so guidance can be written into
	// a live conversation. Such a child blocks on stdin EOF rather than exiting
	// when its turn ends, so the manager closes it explicitly.
	StreamStdin bool
	// FirstMessage is the initial user record for a stream-json child, whose
	// prompt is not an argument.
	FirstMessage string
	// CloseStdinAfterResult ends the child's input as soon as it reports a
	// completed turn. Without it the child blocks on stdin EOF forever and the
	// run only ends at its timeout. Steering is possible up to that moment.
	CloseStdinAfterResult bool
}

// Start spawns the child and returns immediately.
//
// The call returns a Run, not a result: a delegated agent may work for minutes,
// which is longer than any host will hold a tool call open.
//
// Start takes ownership of spec.Transcript on every path: the supervisor closes
// it on success, and Start closes it before returning an error. The caller
// opens the file before it knows whether the run will be admitted, and on
// Windows an open handle blocks the directory's removal — so a refused run used
// to leak the handle for the life of the process, invisibly on POSIX.
func (reg *Registry) Start(spec Spec, group platform.ProcessGroup) (*Run, error) {
	// Applies any due needs_input -> failed expiry (and its audit I/O) before
	// the concurrency check below, and strictly before Registry.mu is taken
	// (item 1) — a run stuck in needs_input past its deadline must free its
	// slot here just as promptly as it did before, just without the audit
	// write happening under the lock.
	reg.sweepExpired()

	reg.mu.Lock()
	// spec.ResumedFrom, when set, excludes the predecessor from the count:
	// a continuation's successor is admitted against the room the
	// predecessor's own needs_input rest state already occupies (see
	// liveCountExcludingLocked). For an ordinary run ResumedFrom is empty
	// and this is exactly the plain live count.
	if live := reg.liveCountExcludingLocked(spec.ResumedFrom); live >= reg.maxLive {
		reg.mu.Unlock()
		if spec.Transcript != nil {
			_ = spec.Transcript.Close()
		}
		return nil, fmt.Errorf("%w: %d of %d slots in use", ErrConcurrencyLimit, live, reg.maxLive)
	}
	reg.started = true
	reg.seq++
	r := &Run{
		ID:              spec.RunID,
		TranscriptPath:  spec.TranscriptPath,
		worktree:        spec.Worktree,
		Agent:           spec.Agent,
		HostAgent:       spec.HostAgent,
		CWD:             spec.CWD,
		Mode:            spec.Mode,
		Confined:        spec.Confined,
		SandboxEnforced: spec.SandboxEnforced,
		Depth:           spec.Depth,
		Started:         time.Now(),
		PromptBytes:     len(spec.Prompt),
		ResumedFrom:     spec.ResumedFrom,
		state:           StateRunning,
		done:            make(chan struct{}),
		transition:      reg.transition,
	}
	if spec.Continuation != nil {
		r.continuation = spec.Continuation
	}
	reg.runs[r.ID] = r
	// The predecessor stops occupying a slot here, in the same critical
	// section that admits its successor, not when Supersede lands a moment
	// later in continueRun. Without this the live set sits at maxLive+1 for
	// the width of that gap — harmless in isolation, but it made the cap an
	// approximation rather than an invariant, and several continuations
	// running at once widened it to +N.
	if spec.ResumedFrom != "" {
		if pred, ok := reg.runs[spec.ResumedFrom]; ok {
			pred.retire()
		}
	}
	reg.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()

	go func() {
		defer func() { _ = spec.Transcript.Close() }()
		reg.supervise(ctx, cancel, r, spec, group)
	}()
	return r, nil
}

func (reg *Registry) supervise(ctx context.Context, cancel context.CancelFunc, r *Run, spec Spec, group platform.ProcessGroup) {
	defer cancel()
	// State the panic handler needs to leave nothing behind: the read ends of
	// the output pipes, and a started child nobody has waited for yet.
	var (
		cmd        *exec.Cmd
		started    bool
		waitIssued bool
		readersEnd func()
	)
	// A panic here would leave the run stuck in "running" forever, with a live
	// child and a caller blocked on Await. Fail it and kill the group instead.
	defer func() {
		if p := recover(); p != nil {
			_ = group.KillAll()
			if readersEnd != nil {
				readersEnd()
			}
			if started && !waitIssued {
				_ = cmd.Wait()
			}
			r.finish(StateFailed, "", "the agent run failed unexpectedly",
				fmt.Sprintf("panic in supervise: %v", p), nil, false)
		}
	}()

	// #nosec G204 -- spec.Command is an adapter command from the operator's config, validated
	// at load; spec.Args are whole argv elements from adapter.BuildArgs. No shell, ever.
	cmd = exec.Command(spec.Command, spec.Args...)
	cmd.Dir = spec.CWD // authoritative: five surveyed agents have no cwd flag
	cmd.Env = spec.Env

	// Attach BEFORE Start: a group assigned afterwards races the child spawning
	// descendants that would then escape the kill.
	if err := group.Attach(cmd); err != nil {
		r.finish(StateFailed, "", "the agent could not be started",
			fmt.Sprintf("attach process group: %v", err), nil, false)
		return
	}

	var steer *steerChannel
	switch {
	case spec.StreamStdin:
		pipe, err := cmd.StdinPipe()
		if err != nil {
			r.finish(StateFailed, "", "the agent could not be started",
				fmt.Sprintf("stdin pipe: %v", err), nil, false)
			return
		}
		steer = &steerChannel{w: pipe}
		r.mu.Lock()
		r.steerCh = steer
		r.mu.Unlock()
		defer steer.close()
	case spec.PromptStdin:
		cmd.Stdin = strings.NewReader(spec.Prompt)
	}

	// os.Pipe, not cmd.StdoutPipe: Wait closes the read ends of StdoutPipe and
	// StderrPipe as soon as the process exits, discarding whatever a slower
	// reader has not yet taken from the kernel buffer. These read ends belong to
	// run and are closed only after the readers are done.
	outR, outW, err := os.Pipe()
	if err != nil {
		r.finish(StateFailed, "", "the agent could not be started",
			fmt.Sprintf("stdout pipe: %v", err), nil, false)
		return
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		r.finish(StateFailed, "", "the agent could not be started",
			fmt.Sprintf("stderr pipe: %v", err), nil, false)
		return
	}
	closeReaders := func() { _ = outR.Close(); _ = errR.Close() }
	readersEnd = closeReaders
	// Every path below ends with the readers finished or abandoned; closing is
	// idempotent, so this only matters for the paths that return early.
	defer closeReaders()
	cmd.Stdout = outW
	cmd.Stderr = errW

	// The caller learns that the agent did not start; it does not learn the
	// binary's path or the OS error, which describe the host, not the task.
	if err := cmd.Start(); err != nil {
		closeReaders()
		_ = outW.Close()
		_ = errW.Close()
		r.finish(StateFailed, "", "the agent could not be started",
			fmt.Sprintf("start %s: %v", spec.Command, err), nil, false)
		return
	}

	started = true

	// The child holds its own copies. Keeping ours open would stop the readers
	// from ever seeing EOF.
	_ = outW.Close()
	_ = errW.Close()

	// Completes a two-phase process-group binding where the OS needs one.
	// Windows creates the child suspended so it cannot spawn a descendant
	// before the job object captures it; AfterStart assigns it to the job and
	// only then resumes it. POSIX implements this as a no-op, so there is no
	// OS branch here. A child that cannot be bound must not be left running
	// outside the group: kill it and fail the run, since an unkillable tree is
	// exactly what the group exists to prevent.
	if ps, ok := group.(platform.PostStarter); ok {
		if err := ps.AfterStart(); err != nil {
			_ = group.KillAll()
			closeReaders()
			waitIssued = true
			_ = cmd.Wait()
			r.finish(StateFailed, "", "the agent could not be started",
				fmt.Sprintf("bind process group after start: %v", err), nil, false)
			return
		}
	}

	// The opening prompt is the first record on the same channel steering uses.
	if steer != nil && spec.FirstMessage != "" {
		if err := steer.send(spec.FirstMessage); err != nil {
			_ = group.KillAll()
			closeReaders()
			waitIssued = true
			_ = cmd.Wait()
			r.finish(StateFailed, "", "the agent could not be given its task",
				fmt.Sprintf("send first message: %v", err), nil, false)
			return
		}
	}

	// Both streams are drained continuously and concurrently. A child that
	// fills a pipe buffer nobody is reading deadlocks rather than finishing.
	var (
		wg           sync.WaitGroup
		outBuf       capped
		errBuf       capped
		outTruncated atomic.Bool
	)
	outBuf.limit = spec.MaxOutput
	r.mu.Lock()
	r.live = &outBuf
	r.mu.Unlock()
	errBuf.limit = 8 << 10 // stderr is diagnostic; a small tail is enough
	wg.Add(2)
	if spec.Parser != nil {
		go func() { defer wg.Done(); outTruncated.Store(r.streamEvents(outR, spec, &outBuf)) }()
	} else {
		go func() { defer wg.Done(); outTruncated.Store(outBuf.drain(outR)) }()
	}
	go func() { defer wg.Done(); errBuf.drain(errR) }()

	// Reaping frees the leader's pid for reuse, so no KillAll may follow it.
	// Where the group can wait for exit without reaping, the leader stays a
	// zombie until reap() runs after the last KillAll. Elsewhere, and when that
	// wait itself fails, the waiter goroutine reaps immediately.
	waitIssued = true
	var (
		exited       = make(chan struct{})
		waitErr      = make(chan error, 1)
		leaderReaped atomic.Bool // set before exited closes
	)
	if ew, ok := group.(platform.ExitWaiter); ok {
		go func() {
			if err := ew.WaitExit(); err != nil {
				// A failed wait says nothing about the child. Treating it as an
				// exit killed a live agent and reported status -1.
				r.setDiagnostic("waiting for the agent to exit failed, reaping instead: " + err.Error())
				leaderReaped.Store(true)
				waitErr <- cmd.Wait()
			}
			close(exited)
		}()
	} else {
		go func() {
			leaderReaped.Store(true)
			waitErr <- cmd.Wait()
			close(exited)
		}()
	}
	reap := func() error {
		<-exited
		if leaderReaped.Load() {
			return <-waitErr
		}
		return cmd.Wait()
	}

	// drain waits for the readers within the grace budget. Readers that outlive
	// it leave the output partial, which the result must say.
	var readersStuck bool
	drain := func() {
		if !drainReaders(&wg, group, closeReaders) {
			readersStuck = true
			r.setDiagnostic("output readers did not finish within the drain budget")
		}
	}
	truncated := func() bool { return outTruncated.Load() || readersStuck }

	timeout := time.NewTimer(spec.Timeout)
	defer timeout.Stop()

	select {
	case <-exited:
		// The child has exited. Its output may still be in the pipe, and a
		// descendant may still hold the write end open.
		if leaderReaped.Load() {
			// The pid is already free for reuse, so the group may not be
			// signalled before the readers have had their grace.
			drain()
			_ = group.KillAll()
		} else {
			// The leader is an unreaped zombie, so its pgid is reserved and the
			// kill is safe now. Bytes already in the pipe stay readable after
			// the writers die, and a descendant that inherited stderr no longer
			// holds the run open for the whole grace.
			_ = group.KillAll()
			drain()
		}
		err := reap()
		code := cmd.ProcessState.ExitCode()
		if err != nil && code == 0 {
			code = -1
		}
		if err != nil {
			// The agent's own stderr IS task output and is useful to the caller;
			// the Go error text describes our process handling and is not.
			// A vendor that reports its refusal on the event stream and exits
			// with empty stderr — a codex usage limit does exactly this —
			// would otherwise leave the caller "exited with status 1: ".
			reason := lastLine(errBuf.String())
			if reason == "" {
				reason = r.VendorFailure()
			}
			r.finish(StateFailed, outBuf.String(),
				fmt.Sprintf("the agent exited with status %d: %s", code, reason),
				fmt.Sprintf("%v: %s", err, errBuf.String()), &code, truncated())
			return
		}
		r.finish(StateCompleted, outBuf.String(), "", "", &code, truncated())

	case <-timeout.C:
		_ = group.KillAll()
		drain()
		_ = reap()
		r.finish(StateFailed, outBuf.String(),
			fmt.Sprintf("timed out after %s", spec.Timeout),
			fmt.Sprintf("timeout; stderr: %s", errBuf.String()), nil, truncated())

	case <-ctx.Done():
		_ = group.KillAll()
		drain()
		_ = reap()
		r.finish(StateCancelled, outBuf.String(), "cancelled", "cancelled by operator or shutdown", nil, truncated())
	}
}

// readerGrace bounds how long run waits for the stdout and stderr readers
// after the child has exited. The readers see EOF only when every holder of
// the write end is gone, so a descendant that outlives its parent would
// otherwise hold the run open. Tests shorten it.
var readerGrace = func() *atomic.Int64 {
	v := new(atomic.Int64)
	v.Store(int64(2 * time.Second))
	return v
}()

// drainReaders waits for the pipe readers to finish and never blocks forever.
// It reports whether they finished; false means the output may be partial.
// On the grace expiring it kills the whole group, which releases any
// descendant holding a write end, and waits once more. As a last resort it
// closes the read ends. On Windows a pending read on a pipe may ignore Close;
// the job-object kill is what frees it there, so the final wait is bounded too.
func drainReaders(wg *sync.WaitGroup, group platform.ProcessGroup, closeReaders func()) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	wait := func() bool {
		t := time.NewTimer(time.Duration(readerGrace.Load()))
		defer t.Stop()
		select {
		case <-done:
			return true
		case <-t.C:
			return false
		}
	}
	if wait() {
		return true
	}
	_ = group.KillAll()
	if wait() {
		return true
	}
	closeReaders()
	return wait()
}

// capped accumulates output up to a limit, then keeps draining and discarding.
//
// Discarding rather than stopping matters: closing the pipe early would send
// SIGPIPE to the child and turn an oversized answer into a crash.
type capped struct {
	mu    sync.Mutex
	buf   strings.Builder
	limit int
	over  bool
}

func (c *capped) drain(rc io.ReadCloser) bool {
	defer func() { _ = rc.Close() }()
	chunk := make([]byte, 32<<10)
	for {
		n, err := rc.Read(chunk)
		if n > 0 {
			c.mu.Lock()
			if c.limit <= 0 || c.buf.Len() < c.limit {
				room := n
				if c.limit > 0 && c.buf.Len()+n > c.limit {
					room = c.limit - c.buf.Len()
					c.over = true
				}
				c.buf.Write(chunk[:room])
			} else {
				c.over = true
			}
			c.mu.Unlock()
		}
		if err != nil {
			c.mu.Lock()
			over := c.over
			c.mu.Unlock()
			return over
		}
	}
}

// append adds already-parsed text, honouring the same cap as raw draining.
func (c *capped) append(s string) (over bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limit > 0 && c.buf.Len()+len(s) > c.limit {
		room := c.limit - c.buf.Len()
		if room > 0 {
			c.buf.WriteString(s[:room])
		}
		c.over = true
		return true
	}
	c.buf.WriteString(s)
	return c.over
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
