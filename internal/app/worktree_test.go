package app

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Intellexie/intellexor/internal/board"
	"github.com/Intellexie/intellexor/internal/store"
)

// waitIdle blocks until the session's async prompt goroutine has finished —
// startCard prompts in the background, and the test must not race its
// writes with the TempDir cleanup.
func waitIdle(t *testing.T, i *Instance, sessID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, busy := i.Statuses()[sessID]; !busy {
			// background title generation may still be writing; give it a
			// moment so the TempDir cleanup never races it
			time.Sleep(250 * time.Millisecond)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("session still busy")
}

// gitInit turns the instance directory into a git repository with one
// commit, so worktree isolation has a base to branch from.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	run("add", "-A")
	run("commit", "-m", "init")
}

// An isolated card starts in a fresh git worktree on its own branch; the
// session keeps the parent repo's project ID, and merging the card brings
// the work back and removes the worktree.
func TestStartCardWorktreeIsolation(t *testing.T) {
	i, _ := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	gitInit(t, i.Directory)
	projectID := store.ProjectID(i.Directory)

	card, err := board.Add(projectID, "Isolated task", board.AddOpts{Isolate: true})
	if err != nil {
		t.Fatal(err)
	}
	sessID, err := startCard(i, projectID, card.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, i, sessID)
	sess, err := i.Mgr.Find(sessID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Worktree == nil {
		t.Fatal("isolated card started without a worktree")
	}
	if sess.Directory != sess.Worktree.Path {
		t.Fatalf("session directory %q != worktree path %q", sess.Directory, sess.Worktree.Path)
	}
	wantBranch := "itx/card-" + card.ID
	if sess.Worktree.Branch != wantBranch {
		t.Fatalf("branch = %q, want %q", sess.Worktree.Branch, wantBranch)
	}
	if sess.Worktree.Base != "main" {
		t.Fatalf("base = %q, want main", sess.Worktree.Base)
	}
	// the worktree lives inside the repo, under the default directory
	if filepath.Dir(sess.Worktree.Path) != filepath.Join(i.Directory, ".intellexor", "worktrees") {
		t.Fatalf("worktree path %q not under the repo's worktree dir", sess.Worktree.Path)
	}
	// project grouping follows the parent repo
	if store.ProjectID(sess.Directory) != projectID {
		t.Fatal("worktree session lost the parent project ID")
	}
	// the card records the branch and worktree path
	cards, err := board.Get(projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cards {
		if c.ID == card.ID {
			if c.Branch != wantBranch || c.WorktreePath != sess.Worktree.Path {
				t.Fatalf("card worktree fields not recorded: %+v", c)
			}
		}
	}

	// the agent's work lands in the worktree; the merge brings it home
	workFile := filepath.Join(sess.Worktree.Path, "done.txt")
	if err := os.WriteFile(workFile, []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MergeCard(i, projectID, card.ID); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(i.Directory, "done.txt"))
	if err != nil {
		t.Fatalf("merged file missing from the repo: %v", err)
	}
	if string(b) != "work\n" {
		t.Fatalf("merged content = %q", b)
	}
	if _, err := os.Stat(sess.Worktree.Path); !os.IsNotExist(err) {
		t.Fatal("worktree not removed after merge")
	}
	fresh, err := i.Mgr.Find(sessID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Worktree != nil || fresh.Directory != i.Directory {
		t.Fatalf("session not returned to the main directory after merge: dir=%q wt=%+v", fresh.Directory, fresh.Worktree)
	}
}

// Default behavior is unchanged: a card without the isolate flag starts in
// the instance directory with no worktree.
func TestStartCardWithoutIsolationUnchanged(t *testing.T) {
	i, _ := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	gitInit(t, i.Directory)
	projectID := store.ProjectID(i.Directory)

	card, err := board.Add(projectID, "Plain task", board.AddOpts{Isolate: false})
	if err != nil {
		t.Fatal(err)
	}
	sessID, err := startCard(i, projectID, card.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, i, sessID)
	sess, err := i.Mgr.Find(sessID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Worktree != nil {
		t.Fatalf("non-isolated card got a worktree: %+v", sess.Worktree)
	}
	if sess.Directory != i.Directory {
		t.Fatalf("session directory = %q, want %q", sess.Directory, i.Directory)
	}
}

// The web UI's Start flow creates the session itself and links it to the
// card afterwards — EnsureCardWorktree moves that session into the
// card's worktree before its first prompt.
func TestEnsureCardWorktreeForLinkedSession(t *testing.T) {
	i, s := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	gitInit(t, i.Directory)
	projectID := store.ProjectID(i.Directory)

	card, err := board.Add(projectID, "UI-started isolated task", board.AddOpts{Isolate: true})
	if err != nil {
		t.Fatal(err)
	}
	i.EnsureCardWorktree(projectID, card.ID, s.ID)

	fresh, err := i.Mgr.Find(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Worktree == nil || fresh.Worktree.Branch != "itx/card-"+card.ID {
		t.Fatalf("linked session not isolated: %+v", fresh.Worktree)
	}
	cards, err := board.Get(projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cards {
		if c.ID == card.ID && c.Branch != fresh.Worktree.Branch {
			t.Fatalf("card branch %q != session branch %q", c.Branch, fresh.Worktree.Branch)
		}
	}
}
