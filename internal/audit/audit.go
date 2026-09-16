// Package audit writes the security record of what was delegated.
//
// It is NOT tamper-evident. Agent B runs as the same user, so under
// `worktree: off`, or if a vendor sandbox is bypassed, B can modify this file.
// The package therefore promises durability and truthful content, not
// integrity against the account it runs as. See docs/01-requirements.md FR-11.3.
package audit

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/platform"
)

// Entry is one line of the log. Bodies are absent by default: the digests are
// there to correlate and compare, not to reconstruct.
type Entry struct {
	Time            time.Time `json:"time"`
	Event           string    `json:"event"` // run.admitted, run.completed, run.refused, steer, ...
	RunID           string    `json:"run_id,omitempty"`
	HostAgent       string    `json:"host_agent,omitempty"`
	TargetAgent     string    `json:"target_agent,omitempty"`
	CWD             string    `json:"cwd,omitempty"`
	Mode            string    `json:"mode,omitempty"`
	Confinement     string    `json:"confinement,omitempty"` // worktree | none
	SandboxEnforced *bool     `json:"sandbox_enforced,omitempty"`
	Depth           int       `json:"depth"`
	Argv            []string  `json:"argv,omitempty"` // prompt and message elements are digested
	PromptDigest    string    `json:"prompt_digest,omitempty"`
	ResponseDigest  string    `json:"response_digest,omitempty"`
	PromptBytes     int       `json:"prompt_bytes,omitempty"`
	ResponseBytes   int       `json:"response_bytes,omitempty"`
	DurationMS      int64     `json:"duration_ms,omitempty"`
	ExitCode        *int      `json:"exit_code,omitempty"`
	SteerCount      int       `json:"steer_count,omitempty"`
	SteerOrigin     string    `json:"steer_origin,omitempty"` // operator | agent
	Reason          string    `json:"reason,omitempty"`       // why a refusal happened
	Message         string    `json:"message,omitempty"`
	// ResumedFrom is the predecessor's run id on a continuation's run.admitted
	// entry (docs/02 §2.3a).
	// Empty for an ordinary run.
	ResumedFrom string `json:"resumed_from,omitempty"`

	// Prompt and Response are populated only when bodies are enabled.
	Prompt   string `json:"prompt,omitempty"`
	Response string `json:"response,omitempty"`
}

// Writer appends entries to a JSONL file, rotating by size.
type Writer struct {
	mu          sync.Mutex
	path        string
	file        *os.File
	size        int64
	rotateBytes int64
	retainFiles int
	bodies      bool
	key         []byte
}

// Options configure a Writer.
type Options struct {
	Path        string
	Bodies      bool
	RotateBytes int64
	RetainFiles int
	// KeyFile holds the per-install HMAC key. It is created if absent.
	// Digests are keyed because an unsalted hash of a short, predictable prompt
	// is a dictionary oracle for anyone who reads the log.
	KeyFile string
}

// New opens the audit log, creating the key on first use.
func New(opts Options) (*Writer, error) {
	if opts.RotateBytes <= 0 {
		opts.RotateBytes = 100 << 20
	}
	if opts.RetainFiles <= 0 {
		opts.RetainFiles = 10
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o700); err != nil {
		return nil, fmt.Errorf("create audit directory: %w", err)
	}
	key, err := loadOrCreateKey(opts.KeyFile)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(opts.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat audit log: %w", err)
	}
	return &Writer{
		path:        opts.Path,
		file:        f,
		size:        fi.Size(),
		rotateBytes: opts.RotateBytes,
		retainFiles: opts.RetainFiles,
		bodies:      opts.Bodies,
		key:         key,
	}, nil
}

// Write appends an entry and fsyncs it.
//
// Every entry is synced rather than batched: the entries worth having are
// exactly the ones written just before something went wrong.
func (w *Writer) Write(e Entry) error {
	if w == nil {
		return nil // auditing disabled
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if !w.bodies {
		e.Prompt, e.Response = "", ""
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode audit entry: %w", err)
	}
	line = append(line, '\n')

	if w.size+int64(len(line)) > w.rotateBytes {
		if err := w.rotate(); err != nil {
			return err
		}
	}
	n, err := w.file.Write(line)
	w.size += int64(n)
	if err != nil {
		return fmt.Errorf("write audit entry: %w", err)
	}
	return w.file.Sync()
}

// Digest returns the keyed digest of a body, for correlating without storing.
func (w *Writer) Digest(s string) string {
	if w == nil || s == "" {
		return ""
	}
	mac := hmac.New(sha256.New, w.key)
	mac.Write([]byte(s))
	return "hmac-sha256:" + hex.EncodeToString(mac.Sum(nil))
}

// Close releases the log file.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

func (w *Writer) rotate() error {
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("close audit log for rotation: %w", err)
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	if err := os.Rename(w.path, w.path+"."+stamp); err != nil {
		return fmt.Errorf("rotate audit log: %w", err)
	}
	w.pruneOldFiles()

	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("reopen audit log: %w", err)
	}
	w.file, w.size = f, 0
	return nil
}

func (w *Writer) pruneOldFiles() {
	matches, err := filepath.Glob(w.path + ".*")
	if err != nil || len(matches) <= w.retainFiles {
		return
	}
	// Glob returns sorted names; the timestamp format sorts chronologically.
	for _, old := range matches[:len(matches)-w.retainFiles] {
		_ = os.Remove(old)
	}
}

func loadOrCreateKey(path string) ([]byte, error) {
	if path == "" {
		// No key file configured: use an ephemeral key. Digests then correlate
		// within a single server lifetime only, which is still enough to match
		// a prompt to its response and strictly better than an unsalted hash.
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate audit key: %w", err)
		}
		return key, nil
	}
	// #nosec G304 -- the key path is the operator's own audit hmac_key_file setting from
	// the validated config; no agent-supplied value reaches it.
	if key, err := os.ReadFile(path); err == nil && len(key) >= 32 {
		return key, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate audit key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create key directory: %w", err)
	}
	// This key signs every digest in the audit log, so it is owner-only on both
	// platforms: a mode on POSIX, a DACL on Windows, where a mode grants
	// nothing and this file landed 0666.
	if err := platform.WriteOwnerOnlyFile(path, key); err != nil {
		return nil, fmt.Errorf("write audit key: %w", err)
	}
	return key, nil
}

// RedactArgv replaces argv elements that equal a sensitive value with its
// digest, so the log records the shape of the command without its content.
func (w *Writer) RedactArgv(argv []string, sensitive ...string) []string {
	out := make([]string, len(argv))
	copy(out, argv)
	for i, a := range out {
		for _, s := range sensitive {
			if s != "" && a == s {
				out[i] = w.Digest(s)
			}
		}
	}
	return out
}
