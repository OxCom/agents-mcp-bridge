// Package control is the operator's local channel to a running server: newline
// delimited JSON over a unix socket (a named pipe on Windows, in v1.1).
//
// It is NOT an operator boundary. Agent B runs as the same user, so a same-UID
// peer check authenticates the account, not the human. What it does provide is
// that another USER cannot reach it, and that the delegated agent is never told
// where it is. See docs/03-threat-model.md T10a.
package control

import "time"

// Request is one operator command.
type Request struct {
	Verb    string `json:"verb"`
	RunID   string `json:"run_id,omitempty"`
	Text    string `json:"text,omitempty"`
	Feature string `json:"feature,omitempty"`
	// QuestionID scopes a VerbAnswer to the specific question the operator
	// saw and answered. Empty answers whatever is currently pending (the
	// `bridge answer` CLI, which has no rendered question to bind to); a
	// non-empty id that no longer matches what is pending is refused rather
	// than misdelivered to whatever replaced it — see run.Run.AnswerQuestion.
	QuestionID string `json:"question_id,omitempty"`
}

// Response is the server's reply.
type Response struct {
	OK     bool      `json:"ok"`
	Error  string    `json:"error,omitempty"`
	Status *Status   `json:"status,omitempty"`
	Runs   []RunInfo `json:"runs,omitempty"`
	// SuccessorID is the new run's id, set on a successful VerbContinue reply.
	SuccessorID string `json:"successor_id,omitempty"`
}

// Status describes a live server.
type Status struct {
	PID               int      `json:"pid"`
	Host              string   `json:"host"`
	Depth             int      `json:"depth"`
	Adapters          []string `json:"adapters"`
	TranscriptDir     string   `json:"transcript_dir"`
	Features          []string `json:"features"`
	DisabledAtRuntime []string `json:"disabled_at_runtime,omitempty"`
}

// RunInfo is one run, as the operator sees it.
type RunInfo struct {
	RunID      string        `json:"run_id"`
	Agent      string        `json:"agent"`
	State      string        `json:"state"`
	Mode       string        `json:"mode"`
	Confined   bool          `json:"confined"`
	Sandboxed  bool          `json:"sandboxed"`
	Started    time.Time     `json:"started"`
	Duration   time.Duration `json:"duration"`
	Transcript string        `json:"transcript"`
	Awaited    bool          `json:"awaited"`
	// Question* describe a question the run is currently blocked on, if any —
	// the operator channel's only view into internal/run.Snapshot.Question.
	// Empty on a run with nothing pending. Deliberately excludes the vendor
	// session id and the transcript path's raw content: this struct is the
	// operator's whole picture, not a window onto anything wider.
	QuestionID      string   `json:"question_id,omitempty"`
	QuestionText    string   `json:"question_text,omitempty"`
	QuestionOptions []string `json:"question_options,omitempty"`
	// SupersededBy is the successor run's id once this run has been continued
	// (State == "superseded"). Empty otherwise.
	SupersededBy string `json:"superseded_by,omitempty"`
}

// Verbs understood by the server.
const (
	VerbStatus = "status"
	VerbRuns   = "runs"
	VerbStop   = "stop"
	// Accept and Reject decide what a confined write run's diff does next.
	// They live here, on the operator's channel, rather than on the MCP
	// surface: by default a human decides what reaches the working tree.
	VerbAccept = "accept"
	VerbReject = "reject"
	// Steer and Answer are the operator's own channel into a live run. The
	// operator is never capped; an agent is.
	VerbSteer  = "steer"
	VerbAnswer = "answer"
	// Continue creates a continuation successor for a run resting in
	// needs_input, carrying the operator's answer as the seed's untrusted-
	// data envelope (docs/superpowers/specs/2026-09-15-continuation-design.md
	// §7). Operator only, same as Answer: the calling agent has no route to
	// this verb.
	VerbContinue = "continue"
	// Enable and Disable narrow or restore a feature for a live server. They
	// can never exceed the config ceiling, and no MCP tool can reach them.
	VerbEnable  = "enable"
	VerbDisable = "disable"
	// Attach and Detach mark the TUI as watching this server, so the gate
	// knows a question can be routed to it instead of falling back to
	// elicitation. Scoped to the connection: a watcher that disconnects
	// without sending Detach is detached anyway, from the connection's own
	// close.
	VerbAttach = "attach"
	VerbDetach = "detach"
)

// IndexEntry records a live server so `bridge watch` can find it with no
// arguments. The socket path is never given to a delegated agent.
type IndexEntry struct {
	PID    int       `json:"pid"`
	Host   string    `json:"host"`
	Socket string    `json:"socket"`
	Since  time.Time `json:"since"`
}
