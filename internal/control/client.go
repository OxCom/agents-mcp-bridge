package control

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/platform"
)

// Client talks to one running server.
type Client struct {
	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder
}

// Dial connects to a server's control socket.
func Dial(socket string) (*Client, error) {
	// platform.DialControl is the OS seam: a unix socket on POSIX, a named
	// pipe on Windows whose server SID it verifies before handing the
	// connection back, so a squatted pipe cannot impersonate the bridge.
	conn, err := platform.DialControl(socket, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", socket, err)
	}
	return &Client{conn: conn, enc: json.NewEncoder(conn), dec: json.NewDecoder(conn)}, nil
}

// Close releases the connection.
func (c *Client) Close() error { return c.conn.Close() }

// doDeadline bounds one request/reply round trip. The TUI calls Do
// synchronously inside bubbletea's Update on every tick and keypress, so a
// wedged server must not be able to freeze the whole UI — the operator could
// not even press 'q'. Generous for a local unix-socket round trip, short
// enough to bound that freeze.
const doDeadline = 5 * time.Second

// Do sends one request and reads the reply.
func (c *Client) Do(req Request) (Response, error) {
	if err := c.conn.SetDeadline(time.Now().Add(doDeadline)); err != nil {
		return Response{}, err
	}
	if err := c.enc.Encode(req); err != nil {
		return Response{}, err
	}
	var resp Response
	if err := c.dec.Decode(&resp); err != nil {
		return Response{}, err
	}
	if !resp.OK && resp.Error != "" {
		return resp, fmt.Errorf("%s", resp.Error)
	}
	return resp, nil
}
