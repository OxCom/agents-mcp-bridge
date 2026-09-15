package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/oxcom/agents-mcp-bridge/internal/gate"
)

// runGate is the server the DELEGATED agent talks to. It holds no config, writes
// no audit and knows exactly one socket. Everything it learns comes from the
// per-run mcp-config the bridge wrote for this run.
func runGate(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: bridge gate (no arguments; configured by environment)")
	}
	if os.Getenv("AGENTS_BRIDGE_GATE_SOCKET") == "" {
		return errors.New("bridge gate is spawned by a bridge run, not by hand")
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "bridge_gate", Version: version}, nil)
	mcp.AddTool(s, gateTool, gateHandler)
	return s.Run(context.Background(), &mcp.StdioTransport{})
}

// gateTool carries an explicit input schema rather than one inferred from
// gateInput. Inference described `input` — a json.RawMessage, i.e. []byte — as
// an array of integers 0..255, and a vendor MCP client validates a tools/call
// against the advertised schema before sending it: a real claude child's
// AskUserQuestion payload, whose `input` is the delegated tool's own arguments
// object (spike C7), was refused on its own side and never reached the gate.
// The run then completed with no question at all.
var gateTool = &mcp.Tool{
	Name:        "ask",
	Description: "Permission and question sink for a supervised delegated run.",
	InputSchema: &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"tool_name":   {Type: "string"},
			"tool_use_id": {Type: "string"},
			// The delegated tool's arguments, whatever shape that tool takes.
			// No constraint is asserted here: the gate treats this as opaque
			// untrusted data and only ever reads it for display.
			"input": {Type: "object"},
		},
		// tool_use_id is absent from some vendor payloads, and a question that
		// arrives without one must still reach the operator rather than being
		// refused by a schema.
		Required: []string{"tool_name", "input"},
	},
}

type gateInput struct {
	ToolName  string          `json:"tool_name"`
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
}

// gateOutput is unused by callers (the vendor-shaped verdict travels as the
// tool's text content, per spike C7) but AddTool needs a concrete output type
// for schema inference; the SDK does not accept `any` there.
type gateOutput struct{}

func gateHandler(ctx context.Context, _ *mcp.CallToolRequest, in gateInput) (*mcp.CallToolResult, gateOutput, error) {
	reply := forward(gate.Ask{
		Token:     os.Getenv("AGENTS_BRIDGE_GATE_TOKEN"),
		RunID:     os.Getenv("AGENTS_BRIDGE_GATE_RUN"),
		Tool:      in.ToolName,
		ToolUseID: in.ToolUseID,
		Input:     in.Input,
	})
	body, err := json.Marshal(reply)
	if err != nil {
		body = []byte(`{"behavior":"deny","message":"the supervising bridge produced no verdict"}`)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, gateOutput{}, nil
}

// forward fails closed: any transport fault denies rather than allowing.
func forward(a gate.Ask) gate.Reply {
	deny := gate.Reply{Behavior: "deny", Message: "the supervising bridge is unreachable"}
	conn, err := net.Dial("unix", os.Getenv("AGENTS_BRIDGE_GATE_SOCKET"))
	if err != nil {
		return deny
	}
	defer conn.Close()
	// No deadline on the read: a human is on the other end of this call, and
	// that can take minutes.
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(a); err != nil {
		return deny
	}
	var r gate.Reply
	if err := json.NewDecoder(conn).Decode(&r); err != nil {
		return deny
	}
	if r.Behavior == "" {
		// The round trip succeeded and the bridge said something; only the
		// verdict itself was missing. Carry its own Error/Message through
		// rather than substituting a generic string that would misreport a
		// reachable bridge as unreachable. It is bridge-authored text, not
		// agent-authored, so passing it on is safe. Fall back to the generic
		// reason only when the bridge genuinely said nothing at all.
		switch {
		case r.Error != "":
			deny.Error = r.Error
			deny.Message = ""
		case r.Message != "":
			deny.Message = r.Message
		}
		return deny
	}
	return r
}
