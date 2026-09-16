// Package stream turns a vendor's own event format into one normalised shape,
// so the watch TUI, the progress digest and the transcript are all
// vendor-independent.
//
// Unrecognised events are preserved verbatim under Raw and rendered as data.
// They are never interpreted, and never treated as instructions.
package stream

import (
	"encoding/json"
	"time"
)

// Kind is the normalised event vocabulary from docs/02-architecture.md §2.4.
type Kind string

// The normalised event kinds. These string values are written verbatim into
// on-disk transcripts, so renaming one breaks replay of existing runs.
const (
	KindRunStarted   Kind = "run.started"
	KindMessage      Kind = "agent.message"
	KindThinking     Kind = "agent.thinking"
	KindToolCall     Kind = "tool.call"
	KindToolResult   Kind = "tool.result"
	KindFileChanged  Kind = "file.changed"
	KindShellCommand Kind = "shell.command"
	KindUsage        Kind = "usage"
	KindQuestion     Kind = "question.asked"
	KindSteer        Kind = "steer.injected"
	KindRunFinished  Kind = "run.finished"
	KindRunFailed    Kind = "run.failed"
	KindVendorError  Kind = "vendor.error"
	KindOversized    Kind = "event.oversized"
	KindUnknown      Kind = "vendor.raw"
)

// Event is one normalised item from a target agent's stream.
type Event struct {
	Seq       int             `json:"seq"`
	Time      time.Time       `json:"time"`
	Kind      Kind            `json:"kind"`
	Text      string          `json:"text,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Path      string          `json:"path,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Usage     *Usage          `json:"usage,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

// Usage is token accounting, where the vendor reports it.
type Usage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	CachedTokens int `json:"cached_input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

// Parser turns one line of a vendor stream into an event.
//
// A parser never fails the run: a line it cannot understand becomes a
// KindUnknown event carrying the original bytes. A vendor adding a field must
// not take the bridge down.
type Parser interface {
	// Name is the adapter id this parser serves.
	Name() string
	// Parse converts one line. ok is false for a line that should be skipped
	// entirely, such as a blank line.
	Parse(line []byte) (Event, bool)
}
