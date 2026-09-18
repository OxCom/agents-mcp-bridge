package stream

import (
	"encoding/json"
	"time"
)

// CodexParser reads `codex exec --json`.
//
// Shapes verified against codex-cli 0.154.0 on 2026-09-14
// (docs/12-spike-results.md S1):
//
//	{"type":"thread.started","thread_id":"..."}
//	{"type":"turn.started"}
//	{"type":"item.completed","item":{"id":"...","type":"agent_message","text":"..."}}
//	{"type":"turn.completed","usage":{...}}
//
// The final assistant message is NOT a distinct event type: it is an
// item.completed whose item.type is agent_message. A parser keying on a
// "message" event finds nothing.
type CodexParser struct{}

// Name reports the adapter id ParserFor keys on to reach this parser.
func (CodexParser) Name() string { return "codex" }

type codexLine struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	// Message carries a TOP-LEVEL {"type":"error"} event's text. It is a
	// different shape from item.completed's error item, and the one a usage
	// limit or an auth failure arrives in.
	Message string `json:"message"`
	// Error carries {"type":"turn.failed","error":{"message":"..."}}, the
	// event that immediately precedes exit status 1.
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Item *struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Text    string `json:"text"`
		Message string `json:"message"`
		Command string `json:"command"`
		Path    string `json:"path"`
		Kind    string `json:"kind"`
	} `json:"item"`
	Usage *struct {
		InputTokens       int `json:"input_tokens"`
		CachedInputTokens int `json:"cached_input_tokens"`
		OutputTokens      int `json:"output_tokens"`
	} `json:"usage"`
}

// Parse consumes one line of `codex exec --json` and emits one normalised
// Event. Thread and turn records map to run and usage kinds; every payload
// arrives as item.completed, so item.type selects the kind.
// An unrecognised or non-JSON line survives as KindUnknown, never an error.
func (p CodexParser) Parse(line []byte) (Event, bool) {
	if len(line) == 0 {
		return Event{}, false
	}
	e := Event{Time: time.Now().UTC()}
	// Raw is a json.RawMessage, so it may only ever hold valid JSON: a
	// non-JSON line assigned here makes the whole EVENT unmarshalable, and the
	// transcript silently loses the line instead of preserving it.
	if json.Valid(line) {
		e.Raw = append(json.RawMessage(nil), line...)
	}

	var v codexLine
	if err := json.Unmarshal(line, &v); err != nil {
		// Not JSON at all: keep it as data rather than discarding evidence.
		e.Kind, e.Text = KindUnknown, string(line)
		return e, true
	}

	switch v.Type {
	case "thread.started":
		e.Kind, e.SessionID = KindRunStarted, v.ThreadID
	case "turn.started":
		e.Kind = KindRunStarted
	case "turn.completed":
		e.Kind = KindUsage
		if v.Usage != nil {
			e.Usage = &Usage{
				InputTokens:  v.Usage.InputTokens,
				CachedTokens: v.Usage.CachedInputTokens,
				OutputTokens: v.Usage.OutputTokens,
			}
		}
	case "error":
		// Top-level, not an item: this is how codex 0.154.0 reports a usage
		// limit, an auth failure, or any other refusal of the turn itself.
		// Mapping it to KindUnknown dropped the only line that says WHY a run
		// exited 1, and the caller got "exit status 1:" with nothing after it.
		e.Kind, e.Text = KindVendorError, v.Message
	case "turn.failed":
		// The turn's own verdict, carrying the same message one level down.
		// It is the last event before a non-zero exit.
		e.Kind = KindRunFailed
		if v.Error != nil {
			e.Text = v.Error.Message
		}
	case "item.completed":
		if v.Item == nil {
			e.Kind = KindUnknown
			return e, true
		}
		switch v.Item.Type {
		case "agent_message":
			e.Kind, e.Text = KindMessage, v.Item.Text
		case "reasoning":
			e.Kind, e.Text = KindThinking, v.Item.Text
		case "command_execution":
			e.Kind, e.Text = KindShellCommand, v.Item.Command
		case "file_change":
			e.Kind, e.Path, e.Text = KindFileChanged, v.Item.Path, v.Item.Kind
		case "error":
			// A vendor error is usually a warning about the operator's own
			// config, not a run failure. It is surfaced, not fatal.
			e.Kind, e.Text = KindVendorError, v.Item.Message
		default:
			e.Kind, e.Tool, e.Text = KindToolCall, v.Item.Type, v.Item.Text
		}
	default:
		e.Kind = KindUnknown
	}
	return e, true
}
