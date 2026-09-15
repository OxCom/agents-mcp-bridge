package config

// Config is the loaded, validated configuration. Field names mirror
// schema/config.schema.json exactly; the schema is the normative document.
type Config struct {
	Version      int                 `yaml:"version"`
	Defaults     Defaults            `yaml:"defaults"`
	Features     Features            `yaml:"features"`
	AllowedRoots []string            `yaml:"allowed_roots"`
	EnvAllowlist []string            `yaml:"env_allowlist"`
	Audit        Audit               `yaml:"audit"`
	Agents       map[string]*Adapter `yaml:"agents"`
}

type Defaults struct {
	TimeoutS           int   `yaml:"timeout_s"`
	MaxOutputBytes     int   `yaml:"max_output_bytes"`
	MaxPromptBytes     int   `yaml:"max_prompt_bytes"`
	MaxEventBytes      int   `yaml:"max_event_bytes"`
	MaxTranscriptBytes int64 `yaml:"max_transcript_bytes"`
	// MaxDepth is a pointer because 0 is meaningful (delegation disabled) and
	// must be distinguishable from absent (use the default of 1).
	MaxDepth             *int   `yaml:"max_depth"`
	MaxConcurrentRuns    int    `yaml:"max_concurrent_runs"`
	MaxAgentSteers       int    `yaml:"max_agent_steers"`
	MaxTurns             int    `yaml:"max_turns"`
	QuestionTimeoutS     int    `yaml:"question_timeout_s"`
	RunRetentionS        int    `yaml:"run_retention_s"`
	RatePerMinute        int    `yaml:"rate_per_minute"`
	OnConcurrencyLimit   string `yaml:"on_concurrency_limit"`
	QueueDepth           int    `yaml:"queue_depth"`
	AllowWriteMode       bool   `yaml:"allow_write_mode"`
	AllowUnconfinedWrite bool   `yaml:"allow_unconfined_write"`
	AgentAcceptance      bool   `yaml:"agent_acceptance"`
}

// Features are tri-state on purpose: absent must mean "use the default", not
// "false". Go's zero value would silently disable everything an operator did
// not spell out, which is how a feature ends up mysteriously off.
type Features struct {
	Stream                *bool `yaml:"stream"`
	Watch                 *bool `yaml:"watch"`
	Interactive           *bool `yaml:"interactive"`
	OperatorSteering      *bool `yaml:"operator_steering"`
	AgentSteering         *bool `yaml:"agent_steering"`
	Sessions              *bool `yaml:"sessions"`
	Audit                 *bool `yaml:"audit"`
	ProgressNotifications *bool `yaml:"progress_notifications"`
}

// Enabled reports a feature's effective value. Everything defaults on except
// agent_steering and interactive, which are opt-in: agent_steering removes
// the human from the loop, and interactive routes a vendor's permission
// prompt to the operator over MCP, which only makes sense once an adapter has
// actually declared the flags for it (rule 18).
func (f Features) Enabled(name string) bool {
	switch name {
	case "stream":
		return boolOr(f.Stream, true)
	case "watch":
		return boolOr(f.Watch, true)
	case "interactive":
		return boolOr(f.Interactive, false)
	case "operator_steering":
		return boolOr(f.OperatorSteering, true)
	case "agent_steering":
		return boolOr(f.AgentSteering, false)
	case "sessions":
		return boolOr(f.Sessions, true)
	case "audit":
		return boolOr(f.Audit, true)
	case "progress_notifications":
		return boolOr(f.ProgressNotifications, true)
	default:
		return false // unknown feature is off: deny by default
	}
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

type Audit struct {
	Path        string `yaml:"path"`
	Bodies      bool   `yaml:"bodies"`
	HMACKeyFile string `yaml:"hmac_key_file"`
	RotateBytes int64  `yaml:"rotate_bytes"`
	RetainFiles int    `yaml:"retain_files"`
}

// Tier decides what a declaration alone can buy. Basic is YAML-only and
// one-shot; full binds to an in-tree Go adapter.
type Tier string

const (
	TierBasic Tier = "basic"
	TierFull  Tier = "full"
)

// Mode selects which sandbox flag list is emitted. It is not confinement.
type Mode string

const (
	ModeReadOnly Mode = "read-only"
	ModeWrite    Mode = "write"
)

// Worktree is confinement, not mode: whether a write run's changes are staged
// in a disposable worktree or land directly in the caller's tree.
type Worktree string

const (
	WorktreeRequired Worktree = "required"
	WorktreeOff      Worktree = "off"
)

// Steer distinguishes true mid-turn interruption from delivery at the next turn
// boundary. Codex's queue is neither: it is a next-RUN mailbox, so Codex is
// SteerNone. See docs/12-spike-results.md S3.
type Steer string

const (
	SteerNone   Steer = "false"
	SteerTrue   Steer = "true"
	SteerQueued Steer = "queued"
)

type Adapter struct {
	ID              string              `yaml:"-"`
	Description     string              `yaml:"description"`
	Command         string              `yaml:"command"`
	Tier            Tier                `yaml:"tier"`
	Mode            Mode                `yaml:"mode"`
	Worktree        Worktree            `yaml:"worktree"`
	SandboxEnforced *bool               `yaml:"sandbox_enforced"`
	AllowedModels   []string            `yaml:"allowed_models"`
	Capabilities    Capabilities        `yaml:"capabilities"`
	Sandbox         map[string][]string `yaml:"sandbox"`
	ResumeSandbox   map[string][]string `yaml:"resume_sandbox"`
	// Interactive holds the flags that point the child at this bridge's gate.
	// Keyed by mode like Sandbox, because a read-only run and a write run do
	// not necessarily get the same interaction surface.
	Interactive  map[string][]string `yaml:"interactive"`
	Invoke       *Invocation         `yaml:"invoke"`
	ResumeInvoke *Invocation         `yaml:"resume_invoke"`
	SteerSpec    *SteerSpec          `yaml:"steer"`
	Stream       *StreamSpec         `yaml:"stream"`

	// ResolvedCommand is the absolute path Command resolved to at load.
	ResolvedCommand string `yaml:"-"`
}

type Capabilities struct {
	Stream           bool  `yaml:"stream"`
	Resume           bool  `yaml:"resume"`
	Steer            Steer `yaml:"steer"`
	Interactive      bool  `yaml:"interactive"`
	StructuredOutput bool  `yaml:"structured_output"`
}

type Invocation struct {
	Args []string `yaml:"args"`
	// Prompt is stdin, stdin_stream_json or argv. argv exposes the prompt in
	// process listings and crash reports and is a last resort.
	Prompt string `yaml:"prompt"`
	// CloseStdinAfter is required for stdin_stream_json children: such a child
	// blocks on stdin EOF instead of exiting when its turn completes.
	CloseStdinAfter string `yaml:"close_stdin_after"`
}

type SteerSpec struct {
	Mode   string   `yaml:"mode"`
	Args   []string `yaml:"args"`
	Record string   `yaml:"record"`
}

type StreamSpec struct {
	Format              string            `yaml:"format"`
	Source              string            `yaml:"source"`
	VendorSessionIDPath string            `yaml:"vendor_session_id_path"`
	FinalMessage        map[string]string `yaml:"final_message"`
	Usage               string            `yaml:"usage"`
}

// MaxDepthOrDefault returns the configured delegation depth, defaulting to 1.
// Absent means "one level of delegation"; an explicit 0 means "none".
func (d Defaults) MaxDepthOrDefault() int {
	if d.MaxDepth == nil {
		return 1
	}
	return *d.MaxDepth
}

// SandboxEnforcedOrDefault reports whether the vendor enforces anything for this
// adapter's mode. Absent means true; false must be declared deliberately and is
// surfaced as UNSANDBOXED wherever the run appears.
func (a *Adapter) SandboxEnforcedOrDefault() bool {
	return a.SandboxEnforced == nil || *a.SandboxEnforced
}
