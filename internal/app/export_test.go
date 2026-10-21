package app

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/Intellexie/intellexor/internal/alarm"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

// roundTrip marshals and unmarshals the payload, simulating the JSON file
// the browser downloads and later uploads.
func roundTrip(t *testing.T, p *ExportPayload) *ExportPayload {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var out ExportPayload
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func TestExportImportRoundTrip(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	work := t.TempDir()
	i := New(work)

	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "exported", Agent: "build"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	user := session.UserMessage{ID: "msg_1000000000000AAAAAA", SessionID: s.ID, Role: "user"}
	asst := session.AssistantMessage{ID: "msg_1000000000001BBBBBB", SessionID: s.ID, Role: "assistant", ParentID: user.ID}
	if err := i.Mgr.SaveMessage(s, user); err != nil {
		t.Fatal(err)
	}
	if err := i.Mgr.SaveMessage(s, asst); err != nil {
		t.Fatal(err)
	}
	text := session.Part{ID: "prt_1", SessionID: s.ID, MessageID: asst.ID, Type: "text", Text: "hello"}
	tool := session.Part{
		ID: "prt_2", SessionID: s.ID, MessageID: asst.ID, Type: "tool", Tool: "bash", CallID: "call_1",
		State: &session.ToolState{Status: "completed", Input: map[string]any{"command": "ls"}, Output: "file.txt"},
	}
	if err := i.Mgr.SavePart(s, text); err != nil {
		t.Fatal(err)
	}
	if err := i.Mgr.SavePart(s, tool); err != nil {
		t.Fatal(err)
	}
	todos := []map[string]any{{"content": "task one", "status": "pending"}}
	if err := i.Mgr.WriteKV("todo", s.ID, todos); err != nil {
		t.Fatal(err)
	}

	payload, err := i.ExportSession(s)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Session == nil || payload.Session.ID != s.ID {
		t.Fatalf("export must carry the session info, got %+v", payload.Session)
	}
	if len(payload.Messages) != 2 {
		t.Fatalf("expected 2 exported messages, got %d", len(payload.Messages))
	}

	// import into a different project directory: a completely new session
	other := t.TempDir()
	j := New(other)
	dup, err := j.ImportSession(roundTrip(t, payload))
	if err != nil {
		t.Fatal(err)
	}
	if dup.ID == "" || dup.ID == s.ID {
		t.Fatalf("import must mint a fresh id, got %q", dup.ID)
	}
	if dup.Title != "exported" {
		t.Fatalf("import must keep the title, got %q", dup.Title)
	}
	if dup.Directory != other || dup.ProjectID != store.ProjectID(other) {
		t.Fatalf("import must land in the importing directory: %q %q", dup.Directory, dup.ProjectID)
	}
	if dup.Revert != nil || dup.ParentID != "" {
		t.Fatalf("import must start clean, got %+v", dup)
	}

	msgs, err := j.Mgr.LoadMessages(dup)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 imported messages, got %d", len(msgs))
	}
	u, ok := msgs[0].Info.(session.UserMessage)
	if !ok || u.ID != user.ID || u.SessionID != dup.ID {
		t.Fatalf("imported user message wrong: %+v", msgs[0].Info)
	}
	a, ok := msgs[1].Info.(session.AssistantMessage)
	if !ok || a.ID != asst.ID || a.SessionID != dup.ID || a.ParentID != user.ID {
		t.Fatalf("imported assistant message wrong: %+v", msgs[1].Info)
	}
	if len(msgs[1].Parts) != 2 {
		t.Fatalf("expected 2 imported parts, got %d", len(msgs[1].Parts))
	}
	if p := msgs[1].Parts[0]; p.ID != "prt_1" || p.SessionID != dup.ID || p.Text != "hello" {
		t.Fatalf("imported text part wrong: %+v", p)
	}
	tp := msgs[1].Parts[1]
	if tp.ID != "prt_2" || tp.Tool != "bash" || tp.SessionID != dup.ID {
		t.Fatalf("imported tool part wrong: %+v", tp)
	}
	if tp.State == nil || tp.State.Output != "file.txt" || tp.State.Input["command"] != "ls" {
		t.Fatalf("imported tool state wrong: %+v", tp.State)
	}

	var got []map[string]any
	if err := j.Mgr.ReadKV("todo", dup.ID, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["content"] != "task one" {
		t.Fatalf("todos not restored on import: %v", got)
	}

	// the source session is untouched
	orig, err := i.Mgr.LoadMessages(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(orig) != 2 {
		t.Fatalf("source must keep its messages, got %d", len(orig))
	}
	if o, ok := orig[0].Info.(session.UserMessage); !ok || o.SessionID != s.ID {
		t.Fatalf("source message sessionID changed: %+v", orig[0].Info)
	}
}

func TestImportRejectsBadPayloads(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	work := t.TempDir()
	i := New(work)

	if _, err := i.ImportSession(nil); err == nil {
		t.Fatal("expected error for nil payload")
	}
	if _, err := i.ImportSession(&ExportPayload{}); err == nil {
		t.Fatal("expected error for missing session")
	}
	if _, err := i.ImportSession(&ExportPayload{Format: "other.thing", Session: &session.Session{}}); err == nil {
		t.Fatal("expected error for foreign format")
	}
	// a message that decodes to neither role must abort the import
	bad := &ExportPayload{
		Format:   ExportFormat,
		Session:  &session.Session{Title: "x"},
		Messages: []ExportMessage{{Info: json.RawMessage(`{"role":"system"}`)}},
	}
	if _, err := i.ImportSession(bad); err == nil {
		t.Fatal("expected error for unknown role")
	}
}

func TestExportExcludesRevertedMessages(t *testing.T) {
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
	if _, err := i.Revert(s, "msg_1000000000002CCCCCC"); err != nil {
		t.Fatal(err)
	}

	payload, err := i.ExportSession(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.Messages) != 2 {
		t.Fatalf("export must contain only visible messages, got %d", len(payload.Messages))
	}
	// the import of that export starts clean: no revert state, visible history
	dup, err := i.ImportSession(roundTrip(t, payload))
	if err != nil {
		t.Fatal(err)
	}
	if dup.Revert != nil {
		t.Fatal("imported session must not carry revert state")
	}
	got, _ := i.Mgr.LoadMessages(dup)
	if len(got) != 2 {
		t.Fatalf("imported session must show the exported history, got %d", len(got))
	}
}

// Wake-ups ride along with an export: future alarms are re-armed under
// the new session id, but past-due untriggered ones are neutralized — an
// import must never replay stale wake-ups (the sweep would fire them
// within one tick otherwise).
func TestExportImportCarriesAlarms(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	work := t.TempDir()
	i := New(work)
	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "alarmed"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	now := util.Now() / 1000
	pid := store.ProjectID(work)
	if _, err := alarm.Add(pid, s.ID, now+3600, "still armed"); err != nil {
		t.Fatal(err)
	}
	if _, err := alarm.Add(pid, s.ID, now-60, "stale"); err != nil {
		t.Fatal(err)
	}

	payload, err := i.ExportSession(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.Alarms) != 2 {
		t.Fatalf("export must carry the session's alarms, got %d", len(payload.Alarms))
	}

	other := t.TempDir()
	j := New(other)
	dup, err := j.ImportSession(roundTrip(t, payload))
	if err != nil {
		t.Fatal(err)
	}

	imported, err := alarm.List(store.ProjectID(other), dup.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(imported) != 2 {
		t.Fatalf("expected 2 imported alarms, got %d", len(imported))
	}
	byNote := map[string]alarm.Alarm{}
	for _, a := range imported {
		byNote[a.Note] = a
	}
	if a := byNote["still armed"]; a.Fired || a.TriggerAt != now+3600 {
		t.Fatalf("future alarm must stay armed under the new session: %+v", a)
	}
	if a := byNote["stale"]; !a.Fired {
		t.Fatalf("past-due alarm must be neutralized on import: %+v", a)
	}

	// the sweep right after the import must fire nothing
	if due, err := alarm.ClaimDue(store.ProjectID(other), util.Now()/1000); err != nil || len(due) != 0 {
		t.Fatalf("sweep fired imported alarms: %+v (%v)", due, err)
	}

	// the source project keeps its own alarms untouched
	src, err := alarm.List(pid, s.ID, false)
	if err != nil || len(src) != 2 {
		t.Fatalf("source alarms must be untouched, got %d (%v)", len(src), err)
	}

	// defuse the source's stale alarm: the global background sweep must
	// never claim it after this test's data dir is gone. Claim is atomic
	// — if the sweep got there first, this is a no-op.
	_, _ = alarm.Claim(pid, mustAlarmID(t, pid, s.ID, "stale"))
}
