package app

import (
	"os"
	"testing"

	"github.com/Intellexie/intellexor/internal/alarm"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

func TestForkCopiesMessagesPartsAndKV(t *testing.T) {
	// isolated data dir; must be set before any store.DataDir() call
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	work := t.TempDir()
	i := New(work)

	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "original", Agent: "build"}
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
	part := session.Part{ID: "prt_1", SessionID: s.ID, MessageID: asst.ID, Type: "text", Text: "hello"}
	if err := i.Mgr.SavePart(s, part); err != nil {
		t.Fatal(err)
	}
	todos := []map[string]any{{"content": "task one", "status": "pending"}}
	if err := i.Mgr.WriteKV("todo", s.ID, todos); err != nil {
		t.Fatal(err)
	}

	dup, err := i.Fork(s, "")
	if err != nil {
		t.Fatal(err)
	}
	if dup.ID == "" || dup.ID == s.ID {
		t.Fatalf("fork must get a fresh id, got %q", dup.ID)
	}
	if dup.Title != "original (forked)" {
		t.Fatalf("unexpected fork title %q", dup.Title)
	}
	if dup.Revert != nil {
		t.Fatal("fork must not carry revert state")
	}

	msgs, err := i.Mgr.LoadMessages(dup)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages in fork, got %d", len(msgs))
	}
	u, ok := msgs[0].Info.(session.UserMessage)
	if !ok || u.ID != user.ID || u.SessionID != dup.ID {
		t.Fatalf("copied user message wrong: %+v", msgs[0].Info)
	}
	a, ok := msgs[1].Info.(session.AssistantMessage)
	if !ok || a.ID != asst.ID || a.SessionID != dup.ID {
		t.Fatalf("copied assistant message wrong: %+v", msgs[1].Info)
	}
	if len(msgs[1].Parts) != 1 {
		t.Fatalf("expected 1 part in fork, got %d", len(msgs[1].Parts))
	}
	if p := msgs[1].Parts[0]; p.SessionID != dup.ID || p.MessageID != asst.ID || p.Text != "hello" {
		t.Fatalf("copied part wrong: %+v", p)
	}

	var got []map[string]any
	if err := i.Mgr.ReadKV("todo", dup.ID, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["content"] != "task one" {
		t.Fatalf("todos not copied to fork: %v", got)
	}

	// the original session is untouched
	orig, err := i.Mgr.LoadMessages(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(orig) != 2 {
		t.Fatalf("expected 2 messages in original, got %d", len(orig))
	}
	if o, ok := orig[0].Info.(session.UserMessage); !ok || o.SessionID != s.ID {
		t.Fatalf("original message sessionID changed: %+v", orig[0].Info)
	}
}

func TestForkAtMessageTruncatesHistory(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	work := t.TempDir()
	i := New(work)
	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "t"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	for idx, id := range []string{"msg_1000000000000AAAAAA", "msg_1000000000001BBBBBB", "msg_1000000000002CCCCCC"} {
		var msg any
		if idx%2 == 0 {
			msg = session.UserMessage{ID: id, SessionID: s.ID, Role: "user"}
		} else {
			msg = session.AssistantMessage{ID: id, SessionID: s.ID, Role: "assistant"}
		}
		if err := i.Mgr.SaveMessage(s, msg); err != nil {
			t.Fatal(err)
		}
	}

	// fork up to and including the second message
	dup, err := i.Fork(s, "msg_1000000000001BBBBBB")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := i.Mgr.LoadMessages(dup)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages in fork, got %d", len(msgs))
	}
	if m := messageIDOf(msgs[1].Info); m != "msg_1000000000001BBBBBB" {
		t.Fatalf("fork should end at the given message, got %q", m)
	}

	orig, _ := i.Mgr.LoadMessages(s)
	if len(orig) != 3 {
		t.Fatalf("original must keep all messages, got %d", len(orig))
	}
}

func TestForkRevertedSourceCopiesVisibleOnly(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	work := t.TempDir()
	i := New(work)
	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "t"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	msgs := []any{
		session.UserMessage{ID: "msg_1000000000000AAAAAA", SessionID: s.ID, Role: "user"},
		session.AssistantMessage{ID: "msg_1000000000001BBBBBB", SessionID: s.ID, Role: "assistant"},
		session.UserMessage{ID: "msg_1000000000002CCCCCC", SessionID: s.ID, Role: "user"},
		session.AssistantMessage{ID: "msg_1000000000003DDDDDD", SessionID: s.ID, Role: "assistant"},
	}
	for _, m := range msgs {
		if err := i.Mgr.SaveMessage(s, m); err != nil {
			t.Fatal(err)
		}
	}

	// revert to the second user message: last two turns hidden
	if _, err := i.Revert(s, "msg_1000000000002CCCCCC"); err != nil {
		t.Fatal(err)
	}

	dup, err := i.Fork(s, "")
	if err != nil {
		t.Fatal(err)
	}
	if dup.Revert != nil {
		t.Fatal("fork of a reverted session must start clean")
	}
	forked, err := i.Mgr.LoadMessages(dup)
	if err != nil {
		t.Fatal(err)
	}
	if len(forked) != 2 {
		t.Fatalf("fork should only copy visible messages, got %d", len(forked))
	}

	// the source session stays reverted with all messages on disk
	if s.Revert == nil {
		t.Fatal("source session must keep its revert state")
	}
	all, err := i.Mgr.LoadMessagesAll(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("source must keep all stored messages, got %d", len(all))
	}
}

func TestForkUnknownMessage(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	work := t.TempDir()
	i := New(work)
	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "t"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Fork(s, "msg_does_not_exist"); err == nil {
		t.Fatal("expected error for unknown messageID")
	}
}

// A fork clones the source's pending wake-ups under the new session id:
// future ones stay armed (and will fire into the fork, not the original),
// past-due untriggered ones are neutralized, and fired ones are history
// that stays with the original.
func TestForkClonesPendingAlarms(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	work := t.TempDir()
	i := New(work)
	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "t"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	pid := store.ProjectID(work)
	now := util.Now() / 1000
	if _, err := alarm.Add(pid, s.ID, now+3600, "future"); err != nil {
		t.Fatal(err)
	}
	if _, err := alarm.Add(pid, s.ID, now-60, "stale"); err != nil {
		t.Fatal(err)
	}
	if _, err := alarm.Add(pid, s.ID, now+120, "already rang"); err != nil {
		t.Fatal(err)
	}
	// mark one alarm fired: history, not cloned
	if _, err := alarm.Claim(pid, mustAlarmID(t, pid, s.ID, "already rang")); err != nil {
		t.Fatal(err)
	}

	dup, err := i.Fork(s, "")
	if err != nil {
		t.Fatal(err)
	}

	// defuse the source's stale alarm right away: the global background
	// sweep must never claim it after this test's data dir is gone (a
	// late fire would write into whatever INTELLEXOR_DATA is current).
	// Claim is atomic — if the sweep got there first, this is a no-op.
	_, _ = alarm.Claim(pid, mustAlarmID(t, pid, s.ID, "stale"))

	forked, err := alarm.List(pid, dup.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	// the background scheduler may claim the source's stale alarm before
	// the fork runs, so don't pin the cloned count — assert instead that
	// the future alarm is cloned armed and no past-due clone is ever
	// left unfired (the neutralization path itself is covered in the
	// alarm package tests)
	var future *alarm.Alarm
	for idx := range forked {
		a := forked[idx]
		if a.Note == "future" {
			future = &forked[idx]
		}
		if a.TriggerAt <= now && !a.Fired {
			t.Fatalf("past-due cloned alarm not neutralized: %+v", a)
		}
	}
	if future == nil || future.Fired || future.TriggerAt != now+3600 {
		t.Fatalf("future alarm must be cloned armed: %+v", future)
	}

	// the original keeps all three, none re-pointed
	orig, err := alarm.List(pid, s.ID, false)
	if err != nil || len(orig) != 3 {
		t.Fatalf("source alarms must be untouched, got %d (%v)", len(orig), err)
	}

	// the sweep right after the fork must fire nothing into the fork
	// (the source's own stale alarm firing is expected and fine)
	due, err := alarm.ClaimDue(pid, util.Now()/1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range due {
		if a.SessionID == dup.ID {
			t.Fatalf("sweep fired a cloned alarm into the fork: %+v", a)
		}
	}
}

// mustAlarmID finds a session's alarm by note (test helper).
func mustAlarmID(t *testing.T, pid, sessionID, note string) string {
	t.Helper()
	alarms, err := alarm.List(pid, sessionID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range alarms {
		if a.Note == note {
			return a.ID
		}
	}
	t.Fatalf("alarm with note %q not found", note)
	return ""
}
