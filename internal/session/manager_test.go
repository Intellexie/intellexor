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

func saveAssistantMsg(t *testing.T, m *Manager, s *Session, id string, parts ...Part) {
	t.Helper()
	if err := m.SaveMessage(s, AssistantMessage{ID: id, SessionID: s.ID, Role: "assistant"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range parts {
		p.SessionID = s.ID
		p.MessageID = id
		if err := m.SavePart(s, p); err != nil {
			t.Fatal(err)
		}
	}
}

func msgIDs(msgs []WithParts) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, messageID(m.Info))
	}
	return out
}

// The tail window counts display units, not message records: a user prompt,
// a response text, a thinking block and a tool call are each one unit, and
// whole messages are kept — the oldest kept message may push the total past
// the limit, but a message is never split across the window edge.
func TestLoadMessagesTail(t *testing.T) {
	m, s := newTailTestManager(t)

	// units: msg_001=1, msg_002=3, msg_003=2, msg_004=1, msg_005=5 (12 total)
	saveUserMsg(t, m, s, "msg_001")
	saveAssistantMsg(t, m, s, "msg_002",
		Part{ID: "prt_001", Type: "tool"},
		Part{ID: "prt_002", Type: "tool"},
		Part{ID: "prt_003", Type: "tool"},
	)
	saveAssistantMsg(t, m, s, "msg_003",
		Part{ID: "prt_004", Type: "reasoning"},
		Part{ID: "prt_005", Type: "text", Text: "done"},
	)
	saveUserMsg(t, m, s, "msg_004")
	saveAssistantMsg(t, m, s, "msg_005",
		Part{ID: "prt_006", Type: "tool"},
		Part{ID: "prt_007", Type: "tool"},
		Part{ID: "prt_008", Type: "tool"},
		Part{ID: "prt_009", Type: "tool"},
		Part{ID: "prt_010", Type: "tool"},
	)

	// a single message whose units exceed the limit is kept whole
	got, err := m.LoadMessagesTail(s, 4)
	if err != nil {
		t.Fatal(err)
	}
	if ids := msgIDs(got); len(ids) != 1 || ids[0] != "msg_005" {
		t.Fatalf("expected [msg_005] (5 units, never split), got %v", ids)
	}
	if len(got[0].Parts) != 5 || got[0].Parts[0].ID != "prt_006" || got[0].Parts[4].ID != "prt_010" {
		t.Fatalf("expected msg_005's 5 tool parts sorted, got %d parts", len(got[0].Parts))
	}

	// msg_005 (5) + msg_004 (1) = 6 < 7, so msg_003 (2) crosses the budget
	// and is kept whole: 8 units across 3 messages
	got, err = m.LoadMessagesTail(s, 7)
	if err != nil {
		t.Fatal(err)
	}
	if ids := msgIDs(got); len(ids) != 3 || ids[0] != "msg_003" || ids[2] != "msg_005" {
		t.Fatalf("expected [msg_003 msg_004 msg_005], got %v", ids)
	}

	// limit covering the whole history, and limit <= 0, load everything
	for _, limit := range []int{0, -1, 12, 100} {
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
	got, err = m.LoadMessagesTail(s, 2)
	if err != nil {
		t.Fatal(err)
	}
	// msg_002 alone is 3 units, above the budget of 2, and is kept whole
	if ids := msgIDs(got); len(ids) != 1 || ids[0] != "msg_002" {
		t.Fatalf("expected [msg_002] only (revert hides msg_003 onward), got %v", ids)
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

func TestDisplayUnits(t *testing.T) {
	// a user prompt is one unit; a synthetic-only prompt renders nothing and is free
	if n := displayUnits(WithParts{Info: UserMessage{Role: "user"}, Parts: []Part{{ID: "p1", Type: "text", Text: "hi"}}}); n != 1 {
		t.Fatalf("user prompt: expected 1 unit, got %d", n)
	}
	if n := displayUnits(WithParts{Info: UserMessage{Role: "user"}, Parts: []Part{{ID: "p1", Type: "text", Synthetic: true}}}); n != 0 {
		t.Fatalf("synthetic-only user prompt: expected 0 units, got %d", n)
	}
	// response text, thinking block and tool call are each one unit
	n := displayUnits(WithParts{Info: AssistantMessage{Role: "assistant"}, Parts: []Part{
		{ID: "p1", Type: "reasoning"},
		{ID: "p2", Type: "text", Text: "answer"},
		{ID: "p3", Type: "tool"},
	}})
	if n != 3 {
		t.Fatalf("assistant text+reasoning+tool: expected 3 units, got %d", n)
	}
	// auxiliary parts and invisible text are free
	n = displayUnits(WithParts{Info: AssistantMessage{Role: "assistant"}, Parts: []Part{
		{ID: "p1", Type: "step-start"},
		{ID: "p2", Type: "text", Synthetic: true},
		{ID: "p3", Type: "text", Ignored: true},
		{ID: "p4", Type: "patch"},
	}})
	if n != 0 {
		t.Fatalf("auxiliary-only assistant message: expected 0 units, got %d", n)
	}
}
