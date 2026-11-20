package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Intellexie/intellexor/internal/store"
)

// POST /board/card/{cardID}/start is the one shared start path (web UI
// Start button, scheduler, board tool, done-links): the session it creates
// must be prompted with the card's full task — title, description and
// acceptance criteria — not just the title, and the card must move to
// In Progress linked to the new session.
func TestStartCardRoutePromptsFullTask(t *testing.T) {
	t.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	// fake provider: record the chat completion request so the prompt text
	// can be asserted, and answer with a minimal valid SSE completion (the
	// async title-generation call hits the same server)
	var gotBody atomic.Value
	prov := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody.Store(string(b))
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		final := map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}}
		out, _ := json.Marshal(final)
		fmt.Fprintf(w, "data: %s\n\n", out)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(prov.Close)

	work := t.TempDir()
	cfg := fmt.Sprintf(`{
		"model": "test/m",
		"small_model": "test/m",
		"board": {"auto_review": false},
		"provider": {"test": {"name": "test",
			"options": {"baseURL": %q, "apiKey": "k"},
			"models": {"m": {"name": "m", "tool_call": true,
				"limit": {"context": 100000, "output": 4096}}}}}
	}`, prov.URL+"/v1")
	if err := os.WriteFile(filepath.Join(work, "intellexor.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(New(work, "test").Handler())
	t.Cleanup(ts.Close)

	// a card carrying the full spec
	code, body := doReq(t, "POST", ts.URL+"/board/card",
		`{"title":"Build the parser","description":"Parse the config format.","criteria":"All tests pass."}`)
	if code != 200 {
		t.Fatalf("POST /board/card: status %d: %s", code, body)
	}
	var card struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatal(err)
	}

	code, body = doReq(t, "POST", ts.URL+"/board/card/"+card.ID+"/start", "")
	if code != 200 {
		t.Fatalf("POST start: status %d: %s", code, body)
	}
	var started struct {
		SessionID string `json:"sessionID"`
	}
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatal(err)
	}
	if started.SessionID == "" {
		t.Fatalf("start returned no sessionID: %s", body)
	}

	// the card moved to In Progress, linked to the new session
	code, body = doReq(t, "GET", ts.URL+"/board", "")
	if code != 200 {
		t.Fatalf("GET /board: status %d: %s", code, body)
	}
	var cards []struct {
		ID        string `json:"id"`
		Column    string `json:"column"`
		SessionID string `json:"sessionID"`
	}
	if err := json.Unmarshal(body, &cards); err != nil {
		t.Fatal(err)
	}
	var started0 bool
	for _, c := range cards {
		if c.ID == card.ID {
			started0 = true
			if c.Column != "inprogress" || c.SessionID != started.SessionID {
				t.Fatalf("card = column %q session %q, want inprogress/%s", c.Column, c.SessionID, started.SessionID)
			}
		}
	}
	if !started0 {
		t.Fatalf("card %s missing from board", card.ID)
	}

	// the background prompt carries the full task, not just the title
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := gotBody.Load().(string)
		if strings.Contains(b, "Build the parser") &&
			strings.Contains(b, "Parse the config format.") &&
			strings.Contains(b, "All tests pass.") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, _ := gotBody.Load().(string)
	for _, want := range []string{"Build the parser", "Parse the config format.", "All tests pass."} {
		if !strings.Contains(b, want) {
			t.Fatalf("prompt missing %q: %s", want, b)
		}
	}

	// starting again (the card is no longer waiting) is rejected
	code, _ = doReq(t, "POST", ts.URL+"/board/card/"+card.ID+"/start", "")
	if code != 400 {
		t.Errorf("re-start: status %d, want 400", code)
	}
	// unknown card
	code, _ = doReq(t, "POST", ts.URL+"/board/card/9999/start", "")
	if code != 400 {
		t.Errorf("unknown card: status %d, want 400", code)
	}
}
