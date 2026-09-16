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
	// base is the chain's ORIGINAL base commit — the resolved HEAD sha
	// Create started from, carried forward unchanged by ContinueFrom. Diff
	// diffs against base, not HEAD, so a continued chain's diff spans every
	// predecessor's WIP-committed work, not just the run in this worktree.
	base string
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
	sha, err := git(root, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve HEAD: %w", err)
	}
	base := strings.TrimSpace(sha)

	wtDir := filepath.Join(stateDir, "worktrees", runID)
	if err := os.MkdirAll(filepath.Dir(wtDir), 0o700); err != nil {
		return nil, fmt.Errorf("create worktree directory: %w", err)
	}
	branch := "agents-bridge/" + runID

	// A named branch rather than --detach: it is what lets `git worktree list`
	// explain an abandoned worktree to a confused operator.
	if _, err := git(root, "worktree", "add", "-b", branch, wtDir, base); err != nil {
		return nil, fmt.Errorf("create worktree: %w", err)
	}
	return &Worktree{Dir: wtDir, RepoRoot: root, branch: branch, base: base}, nil
}

// Diff returns the agent's changes as a patch that applies to the chain's
// original base commit, including files it created. For a worktree produced
// by ContinueFrom, base is the FIRST predecessor's starting commit, not this
// worktree's own checkout point, so the patch spans the whole chain's work —
// see ContinueFrom.
func (w *Worktree) Diff() (string, error) {
	// Intent-to-add makes new files appear in the diff without copying their
	// contents into the index.
	if _, err := git(w.Dir, "add", "-AN", "."); err != nil {
		return "", fmt.Errorf("stage changes: %w", err)
	}
	out, err := git(w.Dir, "diff", w.base)
	if err != nil {
		return "", fmt.Errorf("diff worktree: %w", err)
	}
	return out, nil
}

// ErrNoPredecessor is returned when ContinueFrom is called with no
// predecessor worktree to continue from.
var ErrNoPredecessor = errors.New("worktree: no predecessor worktree to continue from")

// ContinueFrom hands a confined write run's workspace forward to its
// successor, in one call, before the successor starts:
//
//  1. Commit pred's partial diff as a WIP commit on the chain's branch (pred's
//     own worktree and branch — created by Create or a prior ContinueFrom).
//     A pred whose diff is empty produces no commit; the successor derives
//     from the same base pred had, so the chain is indistinguishable from a
//     fresh run.
//  2. Create the successor's worktree from that commit (or, when pred was
//     clean, from pred's own starting base).
//
// The successor's base (used by its own Diff) is carried forward unchanged
// from pred's — the chain's ORIGINAL base — so get_changes on the chain
// spans every run's work, not just the successor's. No directory is ever
// shared between a live predecessor and its successor: pred's worktree is
// left in place (the caller removes it, same as any other finished run's
// worktree) and the successor gets its own, checked out from a commit that
// exists by construction rather than being inferred.
func ContinueFrom(pred *Worktree, stateDir, successorRunID string) (*Worktree, error) {
	if pred == nil {
		return nil, ErrNoPredecessor
	}
	checkoutFrom := pred.base

	// A real stage (not intent-to-add): a WIP commit must actually carry the
	// predecessor's content, not just announce which paths changed.
	if _, err := git(pred.Dir, "add", "-A", "."); err != nil {
		return nil, fmt.Errorf("stage predecessor changes: %w", err)
	}
	status, err := git(pred.Dir, "diff", "--cached", "--name-only")
	if err != nil {
		return nil, fmt.Errorf("check predecessor staged changes: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		// A fixed, bridge-owned identity rather than relying on git config
		// being set up for whatever account runs the bridge: -c overrides
		// like core.hooksPath above, not a dependency on the environment.
		if _, err := git(pred.Dir,
			"-c", "user.name=agents-mcp-bridge",
			"-c", "user.email=agents-mcp-bridge@localhost",
			"commit", "--no-verify", "-m", "agents-bridge: continuation checkpoint",
		); err != nil {
			return nil, fmt.Errorf("commit predecessor WIP: %w", err)
		}
		sha, err := git(pred.Dir, "rev-parse", "HEAD")
		if err != nil {
			return nil, fmt.Errorf("resolve WIP commit: %w", err)
		}
		checkoutFrom = strings.TrimSpace(sha)
	}

	wtDir := filepath.Join(stateDir, "worktrees", successorRunID)
	if err := os.MkdirAll(filepath.Dir(wtDir), 0o700); err != nil {
		return nil, fmt.Errorf("create worktree directory: %w", err)
	}
	branch := "agents-bridge/" + successorRunID
	if _, err := git(pred.RepoRoot, "worktree", "add", "-b", branch, wtDir, checkoutFrom); err != nil {
		return nil, fmt.Errorf("create successor worktree: %w", err)
	}
	return &Worktree{Dir: wtDir, RepoRoot: pred.RepoRoot, branch: branch, base: pred.base}, nil
}

// ChangedFiles lists the paths the agent touched, from the working-tree
// status. On a worktree whose changes have already been committed (a WIP
// checkpoint — see ContinueFrom), the working tree is clean and this returns
// nothing even though the commit itself carries real changes; callers that
// need the truth across a commit boundary want ChangedFilesAgainstBase
// instead.
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

// ChangedFilesAgainstBase lists the paths that differ from the worktree's
// recorded base commit, committed or not. Unlike ChangedFiles (working-tree
// status only), this still reports a predecessor's changes after
// ContinueFrom has committed them as a WIP checkpoint: a retried
// continuation attempt otherwise reports an empty file list for a
// predecessor that changed several (wave-4 review, cmd/bridge/continuation.go).
func (w *Worktree) ChangedFilesAgainstBase() ([]string, error) {
	// Intent-to-add mirrors Diff(): an uncommitted new file must appear here
	// too, not just a committed one.
	if _, err := git(w.Dir, "add", "-AN", "."); err != nil {
		return nil, fmt.Errorf("stage changes: %w", err)
	}
	out, err := git(w.Dir, "diff", "--name-only", w.base)
	if err != nil {
		return nil, fmt.Errorf("diff --name-only against base: %w", err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			files = append(files, line)
		}
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
	// #nosec G204 -- the binary is the constant "git" and every argument is a whole argv
	// element built here from constants, bridge-minted run ids and resolved shas; no shell.
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
