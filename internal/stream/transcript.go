package stream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Transcript is the per-run event log on disk: the single source of truth that
// the watch TUI renders, replay reads, and the progress digest summarises.
//
// It contains whatever the delegated agent read or produced, so it is sensitive
// by construction and lives in an owner-only directory. It is NOT tamper-proof
// against the account the bridge runs as. See docs/01-requirements.md FR-11.
type Transcript struct {
	mu       sync.Mutex
	f        *os.File
	w        *bufio.Writer
	path     string
	seq      int
	written  int64
	limit    int64
	overflow bool
}

// CreateTranscript opens the log for a run. limit caps total bytes; once
// breached the run is stopped rather than silently truncated.
func CreateTranscript(dir, runID string, limit int64) (*Transcript, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create transcript directory: %w", err)
	}
	path := filepath.Join(dir, runID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create transcript: %w", err)
	}
	return &Transcript{f: f, w: bufio.NewWriter(f), path: path, limit: limit}, nil
}

// Path returns the transcript's location. It is for the operator and the TUI;
// it is never returned to the calling agent, which would otherwise read the
// raw, un-enveloped stream straight off disk.
func (t *Transcript) Path() string {
	if t == nil {
		return ""
	}
	return t.path
}

// Append writes one event, assigning its sequence number.
//
// It reports whether the transcript is now over its limit, which the caller
// treats as a reason to stop the run.
func (t *Transcript) Append(e Event) (overLimit bool, err error) {
	if t == nil {
		return false, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	t.seq++
	e.Seq = t.seq
	line, err := json.Marshal(e)
	if err != nil {
		// Raw is the only field that can carry malformed JSON. Losing an event
		// is worse than losing its raw payload, so drop the payload and keep
		// the event, with a note saying what happened.
		degraded := e
		degraded.Raw = nil
		if degraded.Text == "" {
			degraded.Text = "[raw payload dropped: it was not valid JSON]"
		}
		line, err = json.Marshal(degraded)
		if err != nil {
			return t.overflow, fmt.Errorf("encode event: %w", err)
		}
	}
	line = append(line, '\n')

	n, err := t.w.Write(line)
	t.written += int64(n)
	if err != nil {
		return t.overflow, fmt.Errorf("write event: %w", err)
	}
	// Flushed per event so `tail -f` and the watch TUI see it immediately;
	// buffering here would make the live view lag behind the agent.
	if err := t.w.Flush(); err != nil {
		return t.overflow, fmt.Errorf("flush event: %w", err)
	}
	if t.limit > 0 && t.written > t.limit {
		t.overflow = true
	}
	return t.overflow, nil
}

// Close flushes and closes the transcript.
func (t *Transcript) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.w.Flush(); err != nil {
		_ = t.f.Close()
		return err
	}
	return t.f.Close()
}

// ReadTranscript replays a transcript from disk.
func ReadTranscript(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return DecodeEvents(f)
}

// DecodeEvents reads newline-delimited events from r.
func DecodeEvents(r io.Reader) ([]Event, error) {
	var out []Event
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // a partially written final line is normal while a run is live
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Digest summarises a stream for a caller who is polling rather than watching.
// It reports activity without replaying the agent's words, so a progress check
// costs the caller almost nothing.
func Digest(events []Event) string {
	if len(events) == 0 {
		return "no activity yet"
	}
	var (
		tools, files, shells, messages int
		lastTool, lastFile             string
		usage                          *Usage
	)
	for _, e := range events {
		switch e.Kind {
		case KindToolCall:
			tools++
			if e.Tool != "" {
				lastTool = e.Tool
			}
		case KindFileChanged:
			files++
			lastFile = e.Path
		case KindShellCommand:
			shells++
		case KindMessage:
			messages++
		case KindUsage:
			if e.Usage != nil {
				usage = e.Usage
			}
		}
	}
	s := fmt.Sprintf("%d events: %d tool calls, %d shell commands, %d file changes, %d messages",
		len(events), tools, shells, files, messages)
	if lastTool != "" {
		s += fmt.Sprintf("; last tool %s", lastTool)
	}
	if lastFile != "" {
		s += fmt.Sprintf("; last file %s", lastFile)
	}
	if usage != nil {
		s += fmt.Sprintf("; %d in / %d out tokens", usage.InputTokens, usage.OutputTokens)
	}
	return s
}
