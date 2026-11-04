package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Intellexie/intellexor/internal/store"
)

// initRepo makes a small git repository with one commit on main.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func TestCreateResolvesToParentProject(t *testing.T) {
	repo := initRepo(t)
	wt, err := Create(repo, "card-1", Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(wt.Path)
	if wt.Branch != "itx/card-1" {
		t.Fatalf("branch = %q, want itx/card-1", wt.Branch)
	}
	if wt.Base != "main" {
		t.Fatalf("base = %q, want main", wt.Base)
	}
	if fi, err := os.Stat(wt.Path); err != nil || !fi.IsDir() {
		t.Fatalf("worktree path missing: %v", err)
	}
	// the worktree's .git is a file pointing back at the main repo — the
	// project ID (and board/session grouping) must follow the parent
	if got := store.GitRoot(wt.Path); got != repo {
		t.Fatalf("GitRoot(worktree) = %q, want %q", got, repo)
	}
	if store.ProjectID(wt.Path) != store.ProjectID(repo) {
		t.Fatal("worktree project ID differs from the parent repo's")
	}
	// the in-repo worktree dir must not show up as untracked
	if st := runGit(t, repo, "status", "--porcelain"); strings.Contains(st, ".intellexor") {
		t.Fatalf("worktree dir is untracked in the repo: %q", st)
	}
}

func TestCreateUniqueBranchOnRetry(t *testing.T) {
	repo := initRepo(t)
	first, err := Create(repo, "card-2", Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(first.Path)
	second, err := Create(repo, "card-2", Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(second.Path)
	if first.Branch == second.Branch {
		t.Fatalf("both worktrees got branch %q", first.Branch)
	}
	if second.Branch != "itx/card-2-2" {
		t.Fatalf("second branch = %q, want itx/card-2-2", second.Branch)
	}
}

func TestMergeCommitsAndCleansUp(t *testing.T) {
	repo := initRepo(t)
	wt, err := Create(repo, "card-3", Config{})
	if err != nil {
		t.Fatal(err)
	}
	// the agent's work: a new file, committed in the worktree
	if err := os.WriteFile(filepath.Join(wt.Path, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, wt.Path, "add", "-A")
	runGit(t, wt.Path, "commit", "-m", "feature")

	if err := Merge(repo, wt, ""); err != nil {
		t.Fatal(err)
	}
	if err := Remove(repo, wt); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(repo, "feature.txt"))
	if err != nil {
		t.Fatalf("merged file missing from the repo: %v", err)
	}
	if string(b) != "work\n" {
		t.Fatalf("merged content = %q", b)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatal("worktree path still exists after Remove")
	}
	if branchExists(repo, wt.Branch) {
		t.Fatal("branch still exists after Remove")
	}
}

func TestMergeCheckpointsUncommittedChanges(t *testing.T) {
	repo := initRepo(t)
	wt, err := Create(repo, "card-4", Config{})
	if err != nil {
		t.Fatal(err)
	}
	// the agent never committed: the merge checkpoints the worktree
	if err := os.WriteFile(filepath.Join(wt.Path, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Merge(repo, wt, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(filepath.Join(repo, "wip.txt")); err != nil {
		t.Fatalf("uncommitted work not merged: %v", err)
	}
}

func TestDiffAgainstBase(t *testing.T) {
	repo := initRepo(t)
	wt, err := Create(repo, "card-5", Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(wt.Path)
	if err := os.WriteFile(filepath.Join(wt.Path, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch, err := Diff(wt.Path, wt.Base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(patch, "-one") || !strings.Contains(patch, "+two") {
		t.Fatalf("diff does not show the change: %q", patch)
	}
}

func TestSanitize(t *testing.T) {
	if got := Sanitize("card-12"); got != "card-12" {
		t.Fatalf("Sanitize(card-12) = %q", got)
	}
	if got := Sanitize("ses_123/.. x"); got != "ses_123-..-x" && !strings.ContainsAny(got, "/ ") {
		t.Fatalf("Sanitize kept unsafe characters: %q", got)
	}
	if Sanitize("///") == "" {
		t.Fatal("Sanitize collapsed to empty")
	}
}
