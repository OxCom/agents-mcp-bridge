package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/oxcom/agents-mcp-bridge/internal/platform"
)

// Handler answers operator commands. The server owns transport and identity;
// the handler owns meaning.
type Handler interface {
	Status() Status
	Runs() []RunInfo
	Stop(runID string) error
	Accept(runID string) error
	Reject(runID string) error
	Steer(runID, text string) error
	// Answer delivers an operator's answer. questionID, when non-empty, must
	// match the question currently pending on runID or the answer is
	// refused — see run.Run.AnswerQuestion.
	Answer(runID, questionID, text string) error
	// Continue creates a continuation successor for a run resting in
	// needs_input, seeded with text as the operator's answer, and returns
	// the new run's id.
	Continue(runID, text string) (string, error)
	SetFeature(feature string, on bool) error
}

// Server listens on the per-user control endpoint.
type Server struct {
	ln       net.Listener
	endpoint platform.ControlEndpoint
	handler  Handler
	log      *slog.Logger
	socket   string
	index    string

	mu       sync.Mutex
	closed   bool
	watchers int
	// byRun counts watchers attached to one specific run (I1). A watcher
	// attached with no run id (a run picker that has not chosen yet) counts
	// toward watchers but never appears here — see attach.
	byRun map[string]int
}

// watchState is one connection's own attachment, tracked in serve's local
// scope. A connection watches at most one thing at a time: no run (a
// picker), or exactly one run id.
type watchState struct {
	attached bool
	runID    string
}

// Listen starts the control server and registers it in the per-user index.
func Listen(runtimeDir string, endpoint platform.ControlEndpoint, h Handler, log *slog.Logger) (*Server, error) {
	socket := filepath.Join(runtimeDir, fmt.Sprintf("%d.sock", os.Getpid()))
	ln, err := endpoint.Listen(socket)
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln: ln, endpoint: endpoint, handler: h, log: log,
		socket: socket,
		index:  filepath.Join(runtimeDir, "servers.json"),
	}
	if err := s.register(); err != nil {
		_ = ln.Close()
		return nil, err
	}
	go s.accept()
	return s, nil
}

// Socket returns the endpoint path, for the operator's tooling only.
func (s *Server) Socket() string { return s.socket }

func (s *Server) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			s.log.Warn("control accept failed", "error", err)
			return
		}
		go s.serve(conn)
	}
}

func (s *Server) serve(conn net.Conn) {
	defer conn.Close()

	// Identity is checked before a single byte is read: a peer that is not this
	// user gets nothing, not even a parse error to probe with.
	if err := s.endpoint.VerifyPeer(conn); err != nil {
		s.log.Warn("control connection refused", "error", err)
		return
	}

	// A watcher that attaches and then dies without sending Detach must not
	// leave the count high: decrement here regardless of how the loop below
	// exits.
	ws := watchState{}
	defer func() {
		if ws.attached {
			s.detach(ws.runID)
		}
	}()

	dec := json.NewDecoder(bufio.NewReader(conn))
	enc := json.NewEncoder(conn)
	for {
		var req Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		_ = enc.Encode(s.dispatch(req, &ws))
	}
}

// attach and detach adjust the watcher count under the server's own mutex,
// which also guards closed. runID == "" is a run picker: it counts toward
// watchers (some TUI is live) but never toward byRun (I1) — nothing is on
// screen for any run yet, so claiming an attachment to one would reproduce
// the exact bug this fixes, just moved one level down: a question for that
// run would wait out its whole timeout for a picker that never renders it.
func (s *Server) attach(runID string) {
	s.mu.Lock()
	s.watchers++
	if runID != "" {
		if s.byRun == nil {
			s.byRun = make(map[string]int)
		}
		s.byRun[runID]++
	}
	s.mu.Unlock()
}

func (s *Server) detach(runID string) {
	s.mu.Lock()
	if s.watchers > 0 {
		s.watchers--
	}
	if runID != "" && s.byRun[runID] > 0 {
		s.byRun[runID]--
		if s.byRun[runID] == 0 {
			delete(s.byRun, runID)
		}
	}
	s.mu.Unlock()
}

// WatchersAttached reports whether any TUI is currently watching this
// server at all, attached to a run or not. Kept for status/diagnostic use;
// the gate must never use this alone to decide whether a specific run's
// question can reach a human — see WatchersAttachedTo.
func (s *Server) WatchersAttached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watchers > 0
}

// WatchersAttachedTo reports whether a watcher is attached specifically to
// runID (I1). The gate consults this — not WatchersAttached — to decide
// whether THIS run's question can be routed to a human: a watcher attached
// to a different run, or a picker attached to none, must not claim it.
func (s *Server) WatchersAttachedTo(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byRun[runID] > 0
}

func (s *Server) dispatch(req Request, ws *watchState) Response {
	switch req.Verb {
	case VerbStatus:
		st := s.handler.Status()
		return Response{OK: true, Status: &st}
	case VerbRuns:
		return Response{OK: true, Runs: s.handler.Runs()}
	case VerbStop:
		if req.RunID == "" {
			return Response{Error: "stop needs a run_id"}
		}
		if err := s.handler.Stop(req.RunID); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true}
	case VerbAccept:
		if req.RunID == "" {
			return Response{Error: "accept needs a run_id"}
		}
		if err := s.handler.Accept(req.RunID); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true}
	case VerbReject:
		if req.RunID == "" {
			return Response{Error: "reject needs a run_id"}
		}
		if err := s.handler.Reject(req.RunID); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true}
	case VerbSteer:
		if req.RunID == "" || req.Text == "" {
			return Response{Error: "steer needs a run_id and text"}
		}
		if err := s.handler.Steer(req.RunID, req.Text); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true}
	case VerbAnswer:
		if req.RunID == "" || req.Text == "" {
			return Response{Error: "answer needs a run_id and text"}
		}
		if err := s.handler.Answer(req.RunID, req.QuestionID, req.Text); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true}
	case VerbContinue:
		if req.RunID == "" || req.Text == "" {
			return Response{Error: "continue needs a run_id and text"}
		}
		successorID, err := s.handler.Continue(req.RunID, req.Text)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, SuccessorID: successorID}
	case VerbEnable, VerbDisable:
		if req.Feature == "" {
			return Response{Error: req.Verb + " needs a feature"}
		}
		if err := s.handler.SetFeature(req.Feature, req.Verb == VerbEnable); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true}
	case VerbAttach:
		// Idempotent for the same target (including two pickers in a row:
		// runID == "" both times). Switching to a different run moves the
		// attachment rather than adding a second one, so one connection
		// never counts against two runs at once.
		if ws.attached && ws.runID == req.RunID {
			return Response{OK: true}
		}
		if ws.attached {
			s.detach(ws.runID)
		}
		s.attach(req.RunID)
		ws.attached = true
		ws.runID = req.RunID
		return Response{OK: true}
	case VerbDetach:
		if ws.attached {
			s.detach(ws.runID)
			ws.attached = false
			ws.runID = ""
		}
		return Response{OK: true}
	default:
		return Response{Error: "unknown verb " + req.Verb}
	}
}

// Close stops the server and removes it from the index.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.deregister()
	_ = os.Remove(s.socket)
	return s.ln.Close()
}

// register adds this server to the index, pruning entries whose process is gone
// so a crash does not leave the operator chasing a dead socket.
func (s *Server) register() error {
	entries := s.readIndex()
	entries = append(entries, IndexEntry{
		PID:    os.Getpid(),
		Host:   s.handler.Status().Host,
		Socket: s.socket,
	})
	return s.writeIndex(entries)
}

func (s *Server) deregister() {
	var kept []IndexEntry
	for _, e := range s.readIndex() {
		if e.PID != os.Getpid() {
			kept = append(kept, e)
		}
	}
	_ = s.writeIndex(kept)
}

func (s *Server) readIndex() []IndexEntry {
	raw, err := os.ReadFile(s.index)
	if err != nil {
		return nil
	}
	var entries []IndexEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	return LiveEntries(entries)
}

func (s *Server) writeIndex(entries []IndexEntry) error {
	raw, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	tmp := s.index + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.index) // atomic: a reader never sees a half-written index
}

// LiveEntries drops index entries whose process no longer exists. A stale
// entry would otherwise point the TUI at a socket nobody is listening on, or
// worse, at a recycled pid.
func LiveEntries(entries []IndexEntry) []IndexEntry {
	var live []IndexEntry
	for _, e := range entries {
		if processAlive(e.PID) {
			live = append(live, e)
		}
	}
	return live
}

// ReadIndex returns the live servers for this user.
func ReadIndex(runtimeDir string) []IndexEntry {
	// #nosec G304 -- runtimeDir is the 0700 per-user runtime directory from platform.Paths
	// and the file name is a constant; the delegated agent never receives this path.
	raw, err := os.ReadFile(filepath.Join(runtimeDir, "servers.json"))
	if err != nil {
		return nil
	}
	var entries []IndexEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	return LiveEntries(entries)
}
