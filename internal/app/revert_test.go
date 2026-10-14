package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

func TestRevertRestoreUnrevert(t *testing.T) {
	// isolated data dir; must be set before any store.DataDir() call
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	work := t.TempDir()
	i := New(work)

	// workspace files: one that exists and gets edited, one created by the agent
	existing := filepath.Join(work, "hello.txt")
	created := filepath.Join(work, "generated.txt")
	if err := os.WriteFile(existing, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "revert test"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}

	// two messages: user prompt, assistant reply that "edited" the files
	user := session.UserMessage{ID: "msg_1000000000000AAAAAA", SessionID: s.ID, Role: "user"}
	asst := session.AssistantMessage{ID: "msg_1000000000001BBBBBB", SessionID: s.ID, Role: "assistant"}
	if err := i.Mgr.SaveMessage(s, user); err != nil {
		t.Fatal(err)
	}
	if err := i.Mgr.SaveMessage(s, asst); err != nil {
		t.Fatal(err)
	}

	// simulate the assistant's edits: backups taken, then files written
	if err := i.Mgr.BackupFile(s, asst.ID, existing); err != nil {
		t.Fatal(err)
	}
	if err := i.Mgr.BackupFile(s, asst.ID, created); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(created, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// revert to the user message: both messages hidden, files restored
	if _, err := i.Revert(s, user.ID); err != nil {
		t.Fatal(err)
	}
	if s.Revert == nil || s.Revert.MessageID != user.ID {
		t.Fatalf("session revert state not set: %+v", s.Revert)
	}

	got, err := i.Mgr.LoadMessages(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 visible messages after revert, got %d", len(got))
	}
	all, err := i.Mgr.LoadMessagesAll(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 stored messages after revert, got %d", len(all))
	}

	b, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "original\n" {
		t.Fatalf("edit not reverted: %q", string(b))
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("created file should have been deleted on revert, err=%v", err)
	}

	// unrevert: messages visible again, file changes re-applied
	if _, err := i.Unrevert(s); err != nil {
		t.Fatal(err)
	}
	if s.Revert != nil {
		t.Fatal("revert state should be cleared")
	}
	got, err = i.Mgr.LoadMessages(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 visible messages after unrevert, got %d", len(got))
	}
	b, err = os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "edited\n" {
		t.Fatalf("unrevert did not re-apply edit: %q", string(b))
	}
	if b, err := os.ReadFile(created); err != nil || string(b) != "new\n" {
		t.Fatalf("unrevert did not restore created file: %v %q", err, string(b))
	}

	// continuing the session purges the reverted turns permanently
	if _, err := i.Revert(s, user.ID); err != nil {
		t.Fatal(err)
	}
	i.purgeReverted(s)
	if s.Revert != nil {
		t.Fatal("revert state should be cleared after purge")
	}
	all, err = i.Mgr.LoadMessagesAll(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("expected 0 stored messages after purge, got %d", len(all))
	}
}

func TestRevertUnknownMessage(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	work := t.TempDir()
	i := New(work)
	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "t"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	if err := i.Mgr.SaveMessage(s, session.UserMessage{ID: util.ID("msg"), SessionID: s.ID, Role: "user"}); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Revert(s, "msg_does_not_exist"); err == nil {
		t.Fatal("expected error for unknown messageID")
	}
	if s.Revert != nil {
		t.Fatal("revert state must not be set on failure")
	}
}

func TestRevertNoFileChanges(t *testing.T) {
	// reverting turns that touched no files must still work (no snapshot)
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	work := t.TempDir()
	i := New(work)
	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "t"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	user := session.UserMessage{ID: "msg_1000000000000AAAAAA", SessionID: s.ID, Role: "user"}
	asst := session.AssistantMessage{ID: "msg_1000000000001BBBBBB", SessionID: s.ID, Role: "assistant"}
	if err := i.Mgr.SaveMessage(s, user); err != nil {
		t.Fatal(err)
	}
	if err := i.Mgr.SaveMessage(s, asst); err != nil {
		t.Fatal(err)
	}

	if _, err := i.Revert(s, user.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := i.Mgr.LoadMessages(s)
	if len(got) != 0 {
		t.Fatalf("expected 0 visible messages after revert, got %d", len(got))
	}
	if _, err := i.Unrevert(s); err != nil {
		t.Fatal(err)
	}
	got, _ = i.Mgr.LoadMessages(s)
	if len(got) != 2 {
		t.Fatalf("expected 2 visible messages after unrevert, got %d", len(got))
	}
}
