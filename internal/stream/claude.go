package stream

import (
	"encoding/json"
	"time"
)

// ClaudeParser reads `claude -p --output-format stream-json --verbose`.
//
// Verified against claude 2.1.270 on 2026-09-14 (docs/12-spike-results.md C1,
// C6): session_id appears top level on EVERY record, and the turn ends with a
// result record — the PROCESS does not exit until stdin closes, so the result
// record is the only reliable completion signal.
type ClaudeParser struct{}

func (ClaudeParser) Name() string { return "claude" }

type claudeLine struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Result    string          `json:"result"`
	Message   *claudeMessage  `json:"message"`
	Usage     json.RawMessage `json:"usage"`
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	Usage *struct {
		InputTokens     int `json:"input_tokens"`
		CacheReadTokens int `json:"cache_read_input_tokens"`
		OutputTokens    int `json:"output_tokens"`
	} `json:"usage"`
}

func (p ClaudeParser) Parse(line []byte) (Event, bool) {
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

	var v claudeLine
	if err := json.Unmarshal(line, &v); err != nil {
		e.Kind, e.Text = KindUnknown, string(line)
		return e, true
	}
	e.SessionID = v.SessionID

	switch v.Type {
	case "system":
		e.Kind = KindRunStarted
	case "result":
		e.Kind, e.Text = KindRunFinished, v.Result
	case "assistant":
		if v.Message == nil {
			e.Kind = KindUnknown
			return e, true
		}
		if v.Message.Usage != nil {
			e.Usage = &Usage{
				InputTokens:  v.Message.Usage.InputTokens,
				CachedTokens: v.Message.Usage.CacheReadTokens,
				OutputTokens: v.Message.Usage.OutputTokens,
			}
		}
		for _, c := range v.Message.Content {
			switch c.Type {
			case "text":
				e.Kind, e.Text = KindMessage, c.Text
				return e, true
			case "thinking":
				e.Kind, e.Text = KindThinking, c.Text
				return e, true
			case "tool_use":
				e.Kind, e.Tool = KindToolCall, c.Name
				return e, true
			}
		}
		e.Kind = KindUnknown
	case "user":
		// A user record in the OUTPUT stream is a tool result being fed back.
		e.Kind = KindToolResult
	default:
		e.Kind = KindUnknown
	}
	return e, true
}

// ParserFor returns the parser for an adapter id, and whether one exists.
// An adapter with no parser is tier basic: buffered output, no events.
func ParserFor(agentID string) (Parser, bool) {
	switch agentID {
	case "codex":
		return CodexParser{}, true
	case "claude":
		return ClaudeParser{}, true
	default:
		return nil, false
	}
}
