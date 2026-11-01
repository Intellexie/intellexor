package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
)

// GET /session/{id}/message must support lazy-history paging: ?before=<id>
// returns the page of display units strictly older than the cursor, so the
// web UI can load earlier messages on scroll-to-top.
func TestSessionMessagesBeforeCursor(t *testing.T) {
	t.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()
	s := New(t.TempDir(), "test")
	in := s.Instance("")

	sess := &session.Session{ProjectID: projectIDOf(s.DefaultDir), Directory: s.DefaultDir, Title: "paging"}
	if err := in.Mgr.Create(sess); err != nil {
		t.Fatal(err)
	}
	// five one-unit messages: msg_1..msg_5 (a text part makes each a
	// visible display unit)
	for i := 1; i <= 5; i++ {
		id := "msg_" + string(rune('0'+i))
		if err := in.Mgr.SaveMessage(sess, session.UserMessage{ID: id, SessionID: sess.ID, Role: "user"}); err != nil {
			t.Fatal(err)
		}
		if err := in.Mgr.SavePart(sess, session.Part{ID: "prt_" + id, SessionID: sess.ID, MessageID: id, Type: "text", Text: "hi"}); err != nil {
			t.Fatal(err)
		}
	}

	h := s.Handler()
	get := func(q string) ([]string, int) {
		req := httptest.NewRequest("GET", "/session/"+sess.ID+"/message"+q, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Logf("non-200 body: %s", rec.Body.String())
			return nil, rec.Code
		}
		var msgs []struct {
			Info struct {
				ID   string `json:"id"`
				Role string `json:"role"`
			} `json:"info"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &msgs); err != nil {
			t.Fatalf("body not a message list: %v (%q)", err, rec.Body.String())
		}
		var ids []string
		for _, m := range msgs {
			ids = append(ids, m.Info.ID)
		}
		return ids, rec.Code
	}

	// tail window: the last two messages
	ids, code := get("?limit=2")
	if code != 200 {
		t.Fatalf("tail code = %d", code)
	}
	if len(ids) != 2 || ids[0] != "msg_4" || ids[1] != "msg_5" {
		t.Fatalf("tail: got %v (code %d)", ids, code)
	}

	// page before msg_4: everything older, oldest first
	ids, code = get("?limit=10&before=msg_4")
	if code != 200 || len(ids) != 3 || ids[0] != "msg_1" || ids[2] != "msg_3" {
		t.Fatalf("before page: got %v (code %d)", ids, code)
	}

	// the cursor itself is exclusive
	ids, _ = get("?limit=10&before=msg_3")
	if len(ids) != 2 || ids[1] == "msg_3" {
		t.Fatalf("cursor leaked into page: %v", ids)
	}

	// an unknown cursor ends pagination with an empty page
	ids, _ = get("?limit=10&before=msg_nope")
	if len(ids) != 0 {
		t.Fatalf("unknown cursor: got %v", ids)
	}

	// a bad limit is a 400, not a silent full load
	_, code = get("?limit=0&before=msg_3")
	if code != 400 {
		t.Fatalf("invalid limit status = %d, want 400", code)
	}
}
