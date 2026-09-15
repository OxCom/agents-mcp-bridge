// Package worktree confines a write-mode run.
//
// A write run does not edit the caller's tree. It runs in a disposable git
// worktree created from HEAD, and produces a diff that a human — or an agent,
// if the operator explicitly allowed it — must accept before anything reaches
// the real tree. See docs/01-requirements.md FR-10.
//
// This is confinement, not containment: it bounds where the agent's WRITES
// land. It does not stop the agent running commands, using the network, or
// reading anything the operator can read.
package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotARepository is returned when confinement was required but the target is
// not a git repository. There is no fallback that writes directly: falling back
// would silently turn a confined run into an unconfined one.
var ErrNotARepository = errors.New("not a git repository")

// Worktree is one disposable checkout.
type Worktree struct {
	Dir      string // where the agent runs
	RepoRoot string // the caller's repository
	branch   string
}

const gitTimeout = 60 * time.Second

// Create makes a worktree for runID from the repository containing dir.
//
// The worktree lives under stateDir, never inside the caller's tree: a
// worktree nested in the repository would appear in the agent's own file
// listings and in its diff.
func Create(dir, stateDir, runID string) (*Worktree, error) {
	root, err := repoRoot(dir)
	if err != nil {
		return nil, err
	}
	wtDir := filepath.Join(stateDir, "worktrees", runID)
	if err := os.MkdirAll(filepath.Dir(wtDir), 0o700); err != nil {
		return nil, fmt.Errorf("create worktree directory: %w", err)
	}
	branch := "agents-bridge/" + runID

	// A named branch rather than --detach: it is what lets `git worktree list`
	// explain an abandoned worktree to a confused operator.
	if _, err := git(root, "worktree", "add", "-b", branch, wtDir, "HEAD"); err != nil {
		return nil, fmt.Errorf("create worktree: %w", err)
	}
	return &Worktree{Dir: wtDir, RepoRoot: root, branch: branch}, nil
}

// Diff returns the agent's changes as a patch that applies to the original
// HEAD, including files it created.
func (w *Worktree) Diff() (string, error) {
	// Intent-to-add makes new files appear in the diff without copying their
	// contents into the index.
	if _, err := git(w.Dir, "add", "-AN", "."); err != nil {
		return "", fmt.Errorf("stage changes: %w", err)
	}
	out, err := git(w.Dir, "diff", "HEAD")
	if err != nil {
		return "", fmt.Errorf("diff worktree: %w", err)
	}
	return out, nil
}

// ChangedFiles lists the paths the agent touched.
func (w *Worktree) ChangedFiles() ([]string, error) {
	out, err := git(w.Dir, "status", "--porcelain=v1")
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		files = append(files, strings.TrimSpace(line[3:]))
	}
	return files, nil
}

// Apply lands a patch on the caller's tree.
//
// --3way so a patch that no longer applies cleanly REFUSES, rather than
// silently producing something the agent never wrote.
func Apply(repoRoot, patch string) error {
	if strings.TrimSpace(patch) == "" {
		return errors.New("empty patch")
	}
	cmd := exec.Command("git", "apply", "--3way", "--whitespace=nowarn", "-")
	cmd.Dir = repoRoot
	cmd.Stdin = strings.NewReader(patch)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("apply patch: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Remove deletes the worktree and its branch.
//
// Called on acceptance, rejection and expiry. A worktree that survives its run
// is a reported fault, not a silent leak.
func (w *Worktree) Remove() error {
	if w == nil {
		return nil
	}
	var errs []string
	if _, err := git(w.RepoRoot, "worktree", "remove", "--force", w.Dir); err != nil {
		errs = append(errs, err.Error())
		// Even when git refuses, the directory must not be left behind.
		if rmErr := os.RemoveAll(w.Dir); rmErr != nil {
			errs = append(errs, rmErr.Error())
		}
	}
	if _, err := git(w.RepoRoot, "branch", "-D", w.branch); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("remove worktree: %s", strings.Join(errs, "; "))
	}
	return nil
}

func repoRoot(dir string) (string, error) {
	out, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", ErrNotARepository
	}
	return strings.TrimSpace(out), nil
}

// git runs one git command with a timeout and a minimal environment.
//
// Hooks are disabled: `git worktree add` would otherwise run a hostile
// repository's post-checkout hook as the operator, which is the same class of
// defect as the .claude/settings.json hook the adapters already defend against.
// The environment is trimmed so repository config cannot reach us through GIT_*
// variables.
func git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	full := append([]string{"-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"GIT_TERMINAL_PROMPT=0", // never block waiting for credentials
		"GIT_CONFIG_NOSYSTEM=1",
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
