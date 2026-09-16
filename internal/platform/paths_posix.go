//go:build !windows

package platform

import (
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
)

const dirMode = 0o700

type posixPaths struct {
	config, state, runtime string
}

// NewPaths resolves XDG locations, falling back per the XDG base directory
// specification. XDG_RUNTIME_DIR is absent on macOS and in many ssh and tmux
// sessions on Linux, so a TMPDIR fallback is mandatory rather than optional.
func NewPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}

	configHome := envOr("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	stateHome := envOr("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	var runtimeDir string
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		// XDG_RUNTIME_DIR is already per-user (e.g. /run/user/1000), so the
		// "agents-bridge" segment only namespaces us within it.
		runtimeDir = filepath.Join(xdg, "agents-bridge")
	} else {
		// os.TempDir() is TMPDIR on macOS: a long, per-process
		// /var/folders/<random>/T path (~45-50 bytes) that leaves too
		// little headroom under the 104-byte unix socket sun_path limit
		// once "gate/gate-<runID>.sock" is appended (verified in CI: a
		// gate socket path there reached 128 bytes). /tmp is short and, on
		// macOS, the same on every process, so the per-user isolation that
		// XDG_RUNTIME_DIR would have given us instead comes from the 0700
		// "agents-bridge-<uid>" directory created below.
		base := os.TempDir()
		if goruntime.GOOS == "darwin" {
			base = "/tmp"
		}
		runtimeDir = filepath.Join(base, fmt.Sprintf("agents-bridge-%d", os.Getuid()))
	}

	p := &posixPaths{
		config:  filepath.Join(configHome, "agents-bridge", "config.yaml"),
		state:   filepath.Join(stateHome, "agents-bridge"),
		runtime: runtimeDir,
	}
	for _, dir := range []string{p.state, p.runtime} {
		// #nosec G703 -- the directory is the bridge's own state or runtime path, derived from
		// this process's operator-owned environment; a delegated agent never sets it.
		if err := os.MkdirAll(dir, dirMode); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
		// MkdirAll honours umask and does nothing to an existing directory, so
		// the mode is asserted explicitly.
		// #nosec G703 -- the directory is the bridge's own state or runtime path, derived from
		// this process's operator-owned environment; a delegated agent never sets it.
		if err := os.Chmod(dir, dirMode); err != nil {
			return nil, fmt.Errorf("restrict %s: %w", dir, err)
		}
	}
	return p, nil
}

func (p *posixPaths) Config() string  { return p.config }
func (p *posixPaths) State() string   { return p.state }
func (p *posixPaths) Runtime() string { return p.runtime }

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
