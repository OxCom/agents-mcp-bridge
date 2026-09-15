package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests create throwaway git repositories, which means running git write
// commands. This session operates under a git read-only rule, so they are gated
// rather than silently skipped:
//
//	BRIDGE_GIT_TESTS=1 go test ./internal/worktree/
//
// Everything they cover is a v1 claim (FR-10), so they must be run before the
// write-isolation phase can be called done.
func requireGitTests(t *testing.T) {
	t.Helper()
	if os.Getenv("BRIDGE_GIT_TESTS") != "1" {
		t.Skip("set BRIDGE_GIT_TESTS=1 to run tests that create throwaway git repositories")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-qm", "initial")
	return dir
}

func TestWriteRunNeverTouchesTheCallersTree(t *testing.T) {
	requireGitTests(t)
	repo := newRepo(t)
	state := t.TempDir()

	wt, err := Create(repo, state, "run-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer wt.Remove()

	// The agent edits inside the worktree.
	if err := os.WriteFile(filepath.Join(wt.Dir, "a.txt"), []byte("agent edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Dir, "new.txt"), []byte("created\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The caller's tree is byte-identical.
	got, err := os.ReadFile(filepath.Join(repo, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original\n" {
		t.Fatalf("the caller's file changed before acceptance: %q", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "new.txt")); err == nil {
		t.Fatal("a file the agent created appeared in the caller's tree before acceptance")
	}
}

func TestDiffIncludesEditsAndNewFiles(t *testing.T) {
	requireGitTests(t)
	repo := newRepo(t)
	wt, err := Create(repo, t.TempDir(), "run-2")
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Remove()

	if err := os.WriteFile(filepath.Join(wt.Dir, "a.txt"), []byte("agent edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Dir, "new.txt"), []byte("created\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	patch, err := wt.Diff()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a.txt", "new.txt", "agent edit", "created"} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch missing %q:\n%s", want, patch)
		}
	}

	files, err := wt.ChangedFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("changed files = %v, want two", files)
	}
}

func TestAcceptedPatchLandsInTheCallersTree(t *testing.T) {
	requireGitTests(t)
	repo := newRepo(t)
	wt, err := Create(repo, t.TempDir(), "run-3")
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Remove()

	if err := os.WriteFile(filepath.Join(wt.Dir, "a.txt"), []byte("accepted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch, err := wt.Diff()
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(repo, patch); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(repo, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "accepted\n" {
		t.Fatalf("after acceptance the file is %q", got)
	}
}

func TestConflictingPatchIsRefusedNotForced(t *testing.T) {
	requireGitTests(t)
	repo := newRepo(t)
	wt, err := Create(repo, t.TempDir(), "run-4")
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Remove()

	if err := os.WriteFile(filepath.Join(wt.Dir, "a.txt"), []byte("from the agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch, err := wt.Diff()
	if err != nil {
		t.Fatal(err)
	}

	// The operator edited the same line while the agent worked.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("from the human\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Apply(repo, patch); err == nil {
		t.Fatal("a conflicting patch was applied; the operator's edit could have been overwritten")
	}
	got, _ := os.ReadFile(filepath.Join(repo, "a.txt"))
	if string(got) != "from the human\n" {
		t.Fatalf("the operator's work was disturbed by a refused patch: %q", got)
	}
}

func TestEmptyPatchIsRefused(t *testing.T) {
	requireGitTests(t)
	repo := newRepo(t)
	if err := Apply(repo, "   \n"); err == nil {
		t.Fatal("an empty patch must be refused rather than reported as success")
	}
}

func TestRemoveLeavesNothingBehind(t *testing.T) {
	requireGitTests(t)
	repo := newRepo(t)
	state := t.TempDir()
	wt, err := Create(repo, state, "run-5")
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(wt.Dir); !os.IsNotExist(err) {
		t.Fatal("the worktree directory survived removal")
	}
	out, err := exec.Command("git", "-C", repo, "worktree", "list").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "run-5") {
		t.Fatalf("git still lists the worktree:\n%s", out)
	}
}

func TestNonRepositoryIsRefused(t *testing.T) {
	requireGitTests(t)
	// There is no fallback that writes directly: that would silently turn a
	// confined run into an unconfined one.
	if _, err := Create(t.TempDir(), t.TempDir(), "run-6"); err == nil {
		t.Fatal("a plain directory must be refused for a confined write run")
	}
}

func TestWorktreeLivesOutsideTheRepository(t *testing.T) {
	requireGitTests(t)
	repo := newRepo(t)
	state := t.TempDir()
	wt, err := Create(repo, state, "run-7")
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Remove()

	// A worktree nested inside the repository would show up in the agent's own
	// file listings and in its diff.
	if strings.HasPrefix(wt.Dir, repo) {
		t.Fatalf("worktree %s is inside the caller's repository %s", wt.Dir, repo)
	}
}
