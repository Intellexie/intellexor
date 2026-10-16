package session

import (
	"testing"

	"github.com/Intellexie/intellexor/internal/store"
)

func newTailTestManager(t *testing.T) (*Manager, *Session) {
	t.Helper()
	t.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()
	m := NewManager()
	s := &Session{ProjectID: "tailtest", Directory: t.TempDir(), Title: "tail test"}
	if err := m.Create(s); err != nil {
		t.Fatal(err)
	}
	return m, s
}

func saveUserMsg(t *testing.T, m *Manager, s *Session, id string) {
	t.Helper()
	if err := m.SaveMessage(s, UserMessage{ID: id, SessionID: s.ID, Role: "user"}); err != nil {
		t.Fatal(err)
	}
}

func msgIDs(msgs []WithParts) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, messageID(m.Info))
	}
	return out
}

func TestLoadMessagesTail(t *testing.T) {
	m, s := newTailTestManager(t)
	for _, id := range []string{"msg_001", "msg_002", "msg_003", "msg_004", "msg_005"} {
		saveUserMsg(t, m, s, id)
	}

	// the tail keeps the newest messages, in chronological order
	got, err := m.LoadMessagesTail(s, 2)
	if err != nil {
		t.Fatal(err)
	}
	if ids := msgIDs(got); len(ids) != 2 || ids[0] != "msg_004" || ids[1] != "msg_005" {
		t.Fatalf("expected last 2 messages, got %v", ids)
	}

	// limit larger than the history, and limit <= 0, load everything
	for _, limit := range []int{0, -1, 10} {
		got, err := m.LoadMessagesTail(s, limit)
		if err != nil {
			t.Fatal(err)
		}
		if ids := msgIDs(got); len(ids) != 5 {
			t.Fatalf("limit %d: expected all 5 messages, got %v", limit, ids)
		}
	}

	// the tail is relative to visible messages: a revert hides the
	// reverted message and everything after it before the limit applies
	s.Revert = &RevertState{MessageID: "msg_003"}
	got, err = m.LoadMessagesTail(s, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ids := msgIDs(got); len(ids) != 1 || ids[0] != "msg_002" {
		t.Fatalf("expected msg_002 only (revert hides msg_003 onward), got %v", ids)
	}

	// LoadMessages is unchanged: everything visible, no limit
	got, err = m.LoadMessages(s)
	if err != nil {
		t.Fatal(err)
	}
	if ids := msgIDs(got); len(ids) != 2 {
		t.Fatalf("expected 2 visible messages after revert, got %v", ids)
	}
}
