//go:build !windows

package platform

import (
	"fmt"
	"os"
	"path/filepath"
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

	runtimeHome := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeHome == "" {
		runtimeHome = filepath.Join(os.TempDir(), fmt.Sprintf("agents-bridge-%d", os.Getuid()))
	}

	p := &posixPaths{
		config:  filepath.Join(configHome, "agents-bridge", "config.yaml"),
		state:   filepath.Join(stateHome, "agents-bridge"),
		runtime: filepath.Join(runtimeHome, "agents-bridge"),
	}
	for _, dir := range []string{p.state, p.runtime} {
		if err := os.MkdirAll(dir, dirMode); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
		// MkdirAll honours umask and does nothing to an existing directory, so
		// the mode is asserted explicitly.
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
