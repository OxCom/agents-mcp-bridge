package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/oxcom/agents-mcp-bridge/internal/gate"
	"github.com/oxcom/agents-mcp-bridge/internal/platform"
)

// A fake bridge-side gate: accepts one ask, returns a fixed verdict.
func fakeGateSocket(t *testing.T) (string, chan gate.Ask) {
	t.Helper()
	socket := filepath.Join(shortTempDir(t), "g.sock")
	ln, err := platform.NewControlEndpoint().Listen(socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	seen := make(chan gate.Ask, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var a gate.Ask
		_ = json.NewDecoder(conn).Decode(&a)
		seen <- a
		_ = json.NewEncoder(conn).Encode(gate.Reply{Behavior: "deny", Message: "use blue.txt"})
	}()
	return socket, seen
}

// fakeGateSocketMalformed accepts one connection and writes bytes that are
// not valid JSON at all, exercising the Decode-failure branch of forward.
func fakeGateSocketMalformed(t *testing.T) string {
	t.Helper()
	socket := filepath.Join(shortTempDir(t), "malformed.sock")
	ln, err := platform.NewControlEndpoint().Listen(socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var a gate.Ask
		_ = json.NewDecoder(conn).Decode(&a)
		_, _ = conn.Write([]byte("not json at all"))
	}()
	return socket
}

// fakeGateSocketEmptyBehavior accepts one connection and writes a
// well-formed reply with no Behavior set, exercising the empty-Behavior
// branch of forward — the round trip succeeds but carries no verdict.
func fakeGateSocketEmptyBehavior(t *testing.T) string {
	t.Helper()
	socket := filepath.Join(shortTempDir(t), "empty-behavior.sock")
	ln, err := platform.NewControlEndpoint().Listen(socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var a gate.Ask
		_ = json.NewDecoder(conn).Decode(&a)
		_ = json.NewEncoder(conn).Encode(gate.Reply{})
	}()
	return socket
}

// fakeGateSocketBusy accepts one connection and writes a well-formed reply
// that is itself a denial — Behavior set, plus Error and no Message, mirroring
// the server's documented "gate busy" shape. It exercises the ordinary,
// successful round-trip path through forward, not an error branch.
func fakeGateSocketBusy(t *testing.T) string {
	t.Helper()
	socket := filepath.Join(shortTempDir(t), "busy.sock")
	ln, err := platform.NewControlEndpoint().Listen(socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var a gate.Ask
		_ = json.NewDecoder(conn).Decode(&a)
		_ = json.NewEncoder(conn).Encode(gate.Reply{Behavior: "deny", Error: "gate busy"})
	}()
	return socket
}

// callGateTool constructs the tool handler directly, rather than driving
// stdio, and returns the text content of the result.
func callGateTool(t *testing.T, input map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	result, err := gateHandler(context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "ask", Arguments: raw},
	})
	if err != nil {
		t.Fatalf("gateHandler: %v", err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("expected exactly one content block, got %d", len(result.Content))
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected text content, got %T", result.Content[0])
	}
	return text.Text
}

func TestGateClientForwardsToolCallAndReturnsVendorShapedJSON(t *testing.T) {
	socket, seen := fakeGateSocket(t)
	t.Setenv("AGENTS_BRIDGE_GATE_SOCKET", socket)
	t.Setenv("AGENTS_BRIDGE_GATE_TOKEN", "tok")
	t.Setenv("AGENTS_BRIDGE_GATE_RUN", "run-1")

	out := callGateTool(t, map[string]any{
		"tool_name":   "AskUserQuestion",
		"tool_use_id": "toolu_9",
		"input":       map[string]any{"questions": []any{}},
	})
	// The child parses the tool's TEXT content as JSON; the shape is the
	// vendor's, not ours (spike C7).
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("tool result is not JSON: %v (%q)", err, out)
	}
	if got["behavior"] != "deny" || got["message"] != "use blue.txt" {
		t.Fatalf("verdict = %v", got)
	}
	select {
	case a := <-seen:
		if a.ToolUseID != "toolu_9" || a.Token != "tok" || a.RunID != "run-1" {
			t.Fatalf("forwarded ask = %#v", a)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing reached the bridge socket")
	}
}

func TestGateClientForwardTransportFailureDenies(t *testing.T) {
	t.Setenv("AGENTS_BRIDGE_GATE_SOCKET", filepath.Join(t.TempDir(), "no-such.sock"))
	t.Setenv("AGENTS_BRIDGE_GATE_TOKEN", "tok")
	t.Setenv("AGENTS_BRIDGE_GATE_RUN", "run-1")

	out := callGateTool(t, map[string]any{
		"tool_name":   "Bash",
		"tool_use_id": "toolu_1",
		"input":       map[string]any{},
	})
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("tool result is not JSON: %v (%q)", err, out)
	}
	if got["behavior"] != "deny" {
		t.Fatalf("expected fail-closed deny, got %v", got)
	}
}

func TestGateClientMalformedReplyDenies(t *testing.T) {
	socket := fakeGateSocketMalformed(t)
	t.Setenv("AGENTS_BRIDGE_GATE_SOCKET", socket)
	t.Setenv("AGENTS_BRIDGE_GATE_TOKEN", "tok")
	t.Setenv("AGENTS_BRIDGE_GATE_RUN", "run-1")

	out := callGateTool(t, map[string]any{
		"tool_name":   "Bash",
		"tool_use_id": "toolu_2",
		"input":       map[string]any{},
	})
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("tool result is not JSON: %v (%q)", err, out)
	}
	if got["behavior"] != "deny" {
		t.Fatalf("a malformed reply must deny, got %v", got)
	}
}

func TestGateClientEmptyBehaviorDeniesAndCarriesBridgeMessage(t *testing.T) {
	socket := fakeGateSocketEmptyBehavior(t)
	t.Setenv("AGENTS_BRIDGE_GATE_SOCKET", socket)
	t.Setenv("AGENTS_BRIDGE_GATE_TOKEN", "tok")
	t.Setenv("AGENTS_BRIDGE_GATE_RUN", "run-1")

	out := callGateTool(t, map[string]any{
		"tool_name":   "Bash",
		"tool_use_id": "toolu_3",
		"input":       map[string]any{},
	})
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("tool result is not JSON: %v (%q)", err, out)
	}
	if got["behavior"] != "deny" {
		t.Fatalf("a well-formed reply with no Behavior must still deny, got %v", got)
	}
	// The round trip succeeded; a genuinely empty reply carries no bridge
	// text at all, so the generic reason stands. This pins that specific
	// case so the substitution logic cannot silently regress into an
	// unconditional generic message that would swallow a bridge-authored
	// Error/Message on some other input.
	if got["message"] != "the supervising bridge is unreachable" {
		t.Fatalf("expected the generic fallback reason for a genuinely empty reply, got %v", got)
	}
}

func TestGateClientBusyReplyPassesErrorThroughUnchanged(t *testing.T) {
	socket := fakeGateSocketBusy(t)
	t.Setenv("AGENTS_BRIDGE_GATE_SOCKET", socket)
	t.Setenv("AGENTS_BRIDGE_GATE_TOKEN", "tok")
	t.Setenv("AGENTS_BRIDGE_GATE_RUN", "run-1")

	out := callGateTool(t, map[string]any{
		"tool_name":   "Bash",
		"tool_use_id": "toolu_4",
		"input":       map[string]any{},
	})
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("tool result is not JSON: %v (%q)", err, out)
	}
	// This is the ordinary "gate busy" path (Behavior already set), so it
	// must pass straight through untouched, not through the substitution
	// logic added for the empty-Behavior branch.
	if got["behavior"] != "deny" || got["error"] != "gate busy" {
		t.Fatalf("verdict = %v", got)
	}
}

func TestRunGateRefusesWhenNotConfigured(t *testing.T) {
	t.Setenv("AGENTS_BRIDGE_GATE_SOCKET", "")
	if err := runGate(nil); err == nil {
		t.Fatal("expected runGate to refuse when AGENTS_BRIDGE_GATE_SOCKET is unset")
	}
}
