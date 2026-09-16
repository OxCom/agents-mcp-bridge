// Package gate is the child's only address. It carries one thing: a blocking
// request from a delegated agent, and the verdict that unblocks it.
//
// It is NOT the operator's control socket, and never becomes one: the verbs
// here cannot list runs, change a feature, or reach another run. A token
// authenticates the run; SO_PEERCRED authenticates the account.
package gate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/platform"
)

// The peer on the other end of this socket is a delegated, untrusted agent,
// so the server must fail closed against it: a peer that never writes, a
// peer that writes an unbounded blob, a peer that never reads its reply, or
// more peers than one run's single outstanding question could ever need.
const (
	// defaultRequestReadTimeout bounds only reading the request off the
	// wire. It must never bound how long the resolver takes to answer — a
	// human answering a question legitimately takes minutes. Carried as a
	// per-Server field (readTimeout) initialised from this constant, so a
	// test can shorten one server's own instance instead of mutating shared
	// package state.
	defaultRequestReadTimeout = 10 * time.Second
	// defaultReplyWriteTimeout bounds writing the reply. It is applied only
	// after the resolver has returned, so a human's thinking time is never
	// counted against it.
	defaultReplyWriteTimeout = 10 * time.Second
	// requestMaxBytes caps the request body. The request is a small JSON
	// object that a well-behaved client sends immediately; a client that
	// cannot manage that is not one we owe service to.
	requestMaxBytes = 64 * 1024
	// maxInFlight bounds concurrently served connections. One run has
	// exactly one child asking one question at a time; eight is already
	// generous, and the cap means no peer behaviour can grow goroutines
	// without limit.
	maxInFlight = 8
)

// Server is the delegated child's only address. It carries one blocking
// question and its verdict, nothing else: no run listing, no transcript, no
// pointer to the operator's control socket (docs/03 C1).
type Server struct {
	socket       string
	ln           net.Listener
	ep           platform.ControlEndpoint
	resolver     Resolver
	log          *slog.Logger
	runID        string
	token        string
	once         sync.Once
	sem          chan struct{}
	readTimeout  time.Duration
	writeTimeout time.Duration
}

// Listen starts the gate for one run on a socket under <runtimeDir>/gate/ and
// begins accepting immediately. The child reaches it only via the per-run
// mcp-config; token must match on every request.
func Listen(runtimeDir, runID, token string, ep platform.ControlEndpoint, r Resolver, log *slog.Logger) (*Server, error) {
	return listen(runtimeDir, runID, token, ep, r, log, defaultRequestReadTimeout, defaultReplyWriteTimeout)
}

// listen is the unexported constructor behind Listen. A test that needs a
// shorter timeout calls it directly with its own value: the field is fixed
// before the accept loop starts, so there is no later mutation racing with a
// goroutine that might already be reading it — unlike setting the field on
// an already-running *Server, which the race detector (correctly) flags,
// since it has no proof of ordering between a raw field write and a
// concurrently running accept goroutine.
func listen(runtimeDir, runID, token string, ep platform.ControlEndpoint, r Resolver, log *slog.Logger, readTimeout, writeTimeout time.Duration) (*Server, error) {
	// Gate sockets live in their own subdirectory, never as siblings of
	// internal/control's servers.json index. The mcp-config written for the
	// child (cmd/bridge/gateconfig.go) hands it this socket path, so a
	// same-UID child that goes looking would otherwise be handed a pointer
	// into the directory that lists every live bridge's operator control
	// socket — defense in depth given away for free (C1). ep.Listen creates
	// the directory 0700 (platform/endpoint_posix.go), and a path too long
	// for the socket cap surfaces as an error from ep.Listen, never a panic.
	socket := filepath.Join(runtimeDir, "gate", "gate-"+runID+".sock")
	ln, err := ep.Listen(socket)
	if err != nil {
		return nil, err
	}
	s := &Server{
		socket:       socket,
		ln:           ln,
		ep:           ep,
		resolver:     r,
		log:          log,
		runID:        runID,
		token:        token,
		sem:          make(chan struct{}, maxInFlight),
		readTimeout:  readTimeout,
		writeTimeout: writeTimeout,
	}
	go s.accept()
	return s, nil
}

// Socket returns the path cmd/bridge/gateconfig.go writes into the child's
// mcp-config. It is never returned to the calling agent.
func (s *Server) Socket() string { return s.socket }

func (s *Server) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Transient (e.g. fd exhaustion): a hostile peer must not be
			// able to take the gate down for the run it serves. Log and
			// keep accepting, with a brief sleep to avoid a hot spin.
			s.log.Warn("gate accept error", "err", err)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		select {
		case s.sem <- struct{}{}:
			go func() {
				defer func() { <-s.sem }()
				s.serve(conn)
			}()
		default:
			// Every slot is busy. One run has exactly one child asking one
			// question at a time, so this connection is refused rather than
			// queued.
			s.reject(conn)
		}
	}
}

func (s *Server) reject(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	s.reply(conn, Reply{Behavior: "deny", Error: "gate busy"})
}

func (s *Server) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if err := s.ep.VerifyPeer(conn); err != nil {
		s.log.Warn("gate peer refused", "err", err)
		return
	}
	if err := conn.SetReadDeadline(time.Now().Add(s.readTimeout)); err != nil {
		s.log.Warn("gate set read deadline failed", "err", err)
		return
	}
	var a Ask
	lr := io.LimitReader(conn, requestMaxBytes)
	if err := json.NewDecoder(bufio.NewReader(lr)).Decode(&a); err != nil {
		// Logged at the same level as a peer-verify failure: the operator
		// needs to see a malformed request from a delegated agent.
		s.log.Warn("gate decode failed", "err", err)
		return
	}
	// Constant-time is unnecessary: the token is per-run, single-use and
	// unreachable from another user. Deny-by-default is what matters.
	if a.Token != s.token || a.RunID != s.runID {
		s.reply(conn, Reply{Behavior: "deny", Error: "unauthorised"})
		return
	}
	a.Token = "" // never hand it onward
	// The resolver may block for as long as a human takes to answer, so no
	// deadline covers this call.
	reply := s.resolver.Resolve(context.Background(), a)
	s.reply(conn, reply)
}

// reply writes r to conn under a write deadline. Callers that reach this
// after invoking the resolver do so only once it has already returned, so
// the deadline never counts against a human's thinking time.
func (s *Server) reply(conn net.Conn, r Reply) {
	if err := conn.SetWriteDeadline(time.Now().Add(s.writeTimeout)); err != nil {
		s.log.Warn("gate set write deadline failed", "err", err)
		return
	}
	if err := json.NewEncoder(conn).Encode(r); err != nil {
		s.log.Warn("gate reply write failed", "err", err)
	}
}

// Close stops accepting and unlinks the socket. It is idempotent and returns
// the listener's close error; the unlink is best-effort, since a closed
// listener has usually already removed the file.
func (s *Server) Close() error {
	var err error
	s.once.Do(func() {
		err = s.ln.Close()
		// Unlike internal/control's Server.Close, this had never unlinked its
		// socket: after C1 moved gate sockets into their own
		// <runtimeDir>/gate/ subdirectory, nothing else ever removes them, and
		// they accumulate for as long as the runtime dir survives (Minor).
		// Best-effort: a closed listener has usually already unlinked it, and
		// a run id never repeats, so a leftover here is harmless either way.
		_ = os.Remove(s.socket)
	})
	return err
}
