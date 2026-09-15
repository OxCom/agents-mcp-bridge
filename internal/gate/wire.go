package gate

import (
	"context"
	"encoding/json"
)

// Ask is a blocking question forwarded from a delegated agent over the gate
// socket. Token authenticates the run; the server strips it before handing
// the Ask to the Resolver.
type Ask struct {
	Token     string          `json:"token"`
	RunID     string          `json:"run_id"`
	Tool      string          `json:"tool_name"`
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
}

// Reply is the verdict that unblocks an Ask.
type Reply struct {
	Behavior     string          `json:"behavior"`
	Message      string          `json:"message,omitempty"`
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
	Error        string          `json:"error,omitempty"`
}

// Resolver answers an authenticated Ask. Implementations may be constructed
// before the run they serve exists and look it up lazily; Resolve is only
// ever called for a request that has already passed authentication.
type Resolver interface {
	Resolve(context.Context, Ask) Reply
}
