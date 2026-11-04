// Package worktree manages the optional per-session git worktree isolation:
// a board card (or task subagent) can run in a fresh worktree on its own
// branch instead of the shared working directory, so several sessions can
// work on the same repository in parallel without touching each other's
// files or sharing a git index.
//
// Worktrees are created inside the repository (default .intellexor/worktrees,
// excluded via .git/info/exclude) so project config files and AGENTS.md
// resolve from the worktree exactly as they do from the main checkout —
// same repo, same results. The parent repo's project ID is kept via
// store.GitRoot, which resolves a worktree's .git file back to the main
// repository.
package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Intellexie/intellexor/internal/config"
	"github.com/Intellexie/intellexor/internal/store"
)

// BranchPrefix namespaces the branches intellexor mints for worktrees.
const BranchPrefix = "itx/"

// Info describes one created worktree: its path, the branch checked out in
// it and the ref it was branched from.
type Info struct {
	Branch string `json:"branch"`
	Path   string `json:"path"`
	Base   string `json:"base,omitempty"`
}

// Config is the resolved worktree configuration for a directory.
type Config struct {
	Enabled bool
	Dir     string
	Base    string
}

// ConfigOf reads the "worktree" config block for dir (global + project
// merged). Missing block means isolation is off and defaults apply.
func ConfigOf(dir string) Config {
	var cfg Config
	merged := config.Merge(dir)
	if merged.Worktree == nil {
		return cfg
	}
	if merged.Worktree.Enabled != nil {
		cfg.Enabled = *merged.Worktree.Enabled
	}
	cfg.Dir = merged.Worktree.Dir
	cfg.Base = merged.Worktree.Base
	return cfg
}

// Sanitize makes a name safe for a branch name and a path segment.
func Sanitize(name string) string {
	var sb strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			sb.WriteRune(r)
		default:
			sb.WriteByte('-')
		}
	}
	s := strings.Trim(sb.String(), "-.")
	if s == "" {
		s = "session"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// Create adds a git worktree for name under the configured directory,
// checked out on a fresh itx/<name> branch based on cfg.Base (or the
// repository's current HEAD). The branch name is uniquified with a numeric
// suffix when it already exists, so retrying a card never collides.
func Create(repoDir, name string, cfg Config) (Info, error) {
	root := store.GitRoot(repoDir)
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return Info{}, fmt.Errorf("not a git repository: %s", root)
	}

	base := cfg.Base
	if base == "" {
		b, err := git(root, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return Info{}, fmt.Errorf("resolve HEAD: %v: %s", err, strings.TrimSpace(b))
		}
		base = strings.TrimSpace(b)
		if base == "" || base == "HEAD" { // detached HEAD: use the commit
			if b, err := git(root, "rev-parse", "HEAD"); err == nil {
				base = strings.TrimSpace(b)
			}
		}
	}

	dir := cfg.Dir
	if dir == "" {
		dir = filepath.Join(".intellexor", "worktrees")
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	// uniquify branch and path together: retrying a card (or two cards
	// racing on the same name) must never collide
	name = Sanitize(name)
	branch := BranchPrefix + name
	path := filepath.Join(dir, name)
	for i := 2; branchExists(root, branch) || pathExists(path); i++ {
		branch = fmt.Sprintf("%s%s-%d", BranchPrefix, name, i)
		path = filepath.Join(dir, fmt.Sprintf("%s-%d", name, i))
	}
	if _, err := os.Stat(path); err == nil {
		return Info{}, fmt.Errorf("worktree path already exists: %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Info{}, err
	}
	if out, err := git(root, "worktree", "add", "-b", branch, path, base); err != nil {
		return Info{}, fmt.Errorf("git worktree add: %v: %s", err, strings.TrimSpace(out))
	}
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		excludeInternal(root) // worktree lives inside the repo: keep it untracked
	}
	return Info{Branch: branch, Path: path, Base: base}, nil
}

// Remove deletes the worktree and its branch. Missing paths/branches are
// not an error — cleanup should be idempotent.
func Remove(repoDir string, wt Info) error {
	root := store.GitRoot(repoDir)
	if wt.Path != "" {
		if out, err := git(root, "worktree", "remove", "--force", wt.Path); err != nil {
			if !strings.Contains(out, "is not a working tree") && !strings.Contains(out, "does not exist") {
				// keep going: the branch delete and path cleanup still apply
				_ = os.RemoveAll(wt.Path)
			}
		}
	}
	if wt.Branch != "" {
		_, _ = git(root, "branch", "-D", wt.Branch)
	}
	return nil
}

// Merge brings the worktree's branch into the repository's current branch.
// Uncommitted changes in the worktree are committed first (checkpoint), so
// an agent that never ran git commit still gets its work merged. A conflicted
// merge is aborted before returning the error, leaving the repo untouched.
func Merge(repoDir string, wt Info, message string) error {
	root := store.GitRoot(repoDir)
	if st, err := git(wt.Path, "status", "--porcelain"); err == nil && strings.TrimSpace(st) != "" {
		if _, err := git(wt.Path, "add", "-A"); err != nil {
			return fmt.Errorf("stage worktree changes: %v", err)
		}
		if out, err := git(wt.Path, "commit", "-m", "intellexor: checkpoint work before merge"); err != nil {
			return fmt.Errorf("commit worktree changes: %v: %s", err, strings.TrimSpace(out))
		}
	}
	if message == "" {
		message = "Merge " + wt.Branch + " (intellexor worktree)"
	}
	if out, err := git(root, "merge", "--no-ff", "-m", message, wt.Branch); err != nil {
		_, _ = git(root, "merge", "--abort") // leave no conflicted merge state
		return fmt.Errorf("merge %s: %v: %s", wt.Branch, err, strings.TrimSpace(out))
	}
	return nil
}

// Diff returns the diff of the worktree against base (including uncommitted
// changes), in `git diff` patch form.
func Diff(wtPath, base string) (string, error) {
	if base == "" {
		base = "HEAD"
	}
	return git(wtPath, "diff", base)
}

// HasChanges reports whether the worktree has uncommitted changes.
func HasChanges(wtPath string) bool {
	st, err := git(wtPath, "status", "--porcelain")
	return err == nil && strings.TrimSpace(st) != ""
}

func branchExists(root, branch string) bool {
	_, err := git(root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// excludeInternal adds the default worktree directory to .git/info/exclude
// (repo-local, never committed) so the worktrees never show up as
// untracked.
func excludeInternal(root string) {
	infoDir := filepath.Join(root, ".git", "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		return
	}
	exclude := filepath.Join(infoDir, "exclude")
	b, _ := os.ReadFile(exclude)
	line := ".intellexor/"
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == line {
			return
		}
	}
	f, err := os.OpenFile(exclude, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		_, _ = f.WriteString("\n")
	}
	_, _ = f.WriteString(line + "\n")
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
