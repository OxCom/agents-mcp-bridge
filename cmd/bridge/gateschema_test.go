package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// gateClient connects an in-memory client to the same server bridge gate runs,
// so the tool's advertised schema and its argument validation are exercised the
// way a vendor MCP client exercises them.
func gateClient(t *testing.T) *mcp.ClientSession {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "bridge_gate", Version: version}, nil)
	s.AddTool(gateTool, gateHandler)
	ct, st := mcp.NewInMemoryTransports()
	if _, err := s.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: version}, nil).
		Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// TestGateToolAcceptsAVendorQuestionPayload pins the one thing the gate must
// accept: the payload spike C7 recorded, whose `input` is the delegated tool's
// own arguments object. Schema inference from a json.RawMessage field described
// that object as an array of bytes, so a real claude child's AskUserQuestion
// call was refused by its own MCP client before the gate ever saw it — the run
// then finished with no question, which is how three conformance tests failed
// while the transport itself was healthy.
func TestGateToolAcceptsAVendorQuestionPayload(t *testing.T) {
	cs := gateClient(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ask",
		Arguments: map[string]any{
			"tool_name":   "AskUserQuestion",
			"tool_use_id": "toolu_01",
			"input": map[string]any{
				"questions": []any{map[string]any{
					"question": "red.txt or blue.txt?",
					"header":   "Filename",
					"options":  []any{"red.txt", "blue.txt"},
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("the gate refused a vendor-shaped question payload: %v", err)
	}
	if res.IsError {
		t.Fatalf("the gate returned a protocol error for a vendor-shaped payload: %+v", res.Content)
	}
	// No bridge is listening in a unit test, so the verdict must be a fail-closed
	// deny — but it must be a verdict, not a validation refusal.
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected text content, got %T", res.Content[0])
	}
	var verdict struct {
		Behavior string `json:"behavior"`
	}
	if err := json.Unmarshal([]byte(text.Text), &verdict); err != nil {
		t.Fatalf("verdict is not JSON: %v: %q", err, text.Text)
	}
	if verdict.Behavior != "deny" {
		t.Errorf("behavior = %q, want deny with no bridge listening", verdict.Behavior)
	}
	// The vendor refuses a permission-prompt result that carries anything but
	// one text block: structuredContent, which the SDK's generic AddTool emits
	// for a typed output value, made a real claude child report "Permission
	// prompt tool returned an invalid result" and retry the question.
	if len(res.Content) != 1 {
		t.Errorf("result carries %d content blocks, want exactly 1", len(res.Content))
	}
	if res.StructuredContent != nil {
		t.Errorf("result carries structuredContent %v; a permission-prompt result must be text only", res.StructuredContent)
	}
}

// TestGateToolAdvertisesNoOutputSchema pins the other half: an output schema is
// what makes the SDK attach structuredContent in the first place.
func TestGateToolAdvertisesNoOutputSchema(t *testing.T) {
	if gateTool.OutputSchema != nil {
		t.Errorf("gate tool advertises an output schema: %+v", gateTool.OutputSchema)
	}
}

// TestGateToolSchemaDescribesInputAsAnObject fails if schema inference is ever
// allowed back: a []byte-shaped `input` is what broke the vendor path, and the
// wire payload is an arbitrary JSON object.
func TestGateToolSchemaDescribesInputAsAnObject(t *testing.T) {
	raw, err := json.Marshal(gateTool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"maximum":255`) {
		t.Errorf("input is advertised as a byte array, which no vendor sends: %s", raw)
	}
	var schema struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if got := schema.Properties["input"].Type; got != "object" {
		t.Errorf("input type = %v, want object: %s", got, raw)
	}
}
