package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Intellexie/intellexor/internal/board"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/tool"
)

// linkCard creates a card and links it to the session (as the web UI does
// when a card is started from the board). Cards are always created in open,
// so a non-open column is reached with a move.
func linkCard(t *testing.T, i *Instance, s *session.Session, title, column string) board.Card {
	t.Helper()
	card, err := board.Add(store.ProjectID(i.Directory), title, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.Update(store.ProjectID(i.Directory), card.ID, nil, &column, nil, &s.ID); err != nil {
		t.Fatal(err)
	}
	return card
}

// The agent must be able to move its card to done without knowing the card
// ID: the tool resolves the card linked to the session.
func TestBoardToolMovesLinkedCardToDone(t *testing.T) {
	i, s := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	card := linkCard(t, i, s, "Implement the thing", "inprogress")

	ch := i.Hub.Subscribe()
	defer i.Hub.Unsubscribe(ch)

	bt, ok := registryFor(i).Get("board")
	if !ok {
		t.Fatal("board tool not registered")
	}
	res, err := bt.Run(&tool.Ctx{SessionID: s.ID, Directory: i.Directory},
		json.RawMessage(`{"action":"move","column":"done"}`))
	if err != nil {
		t.Fatalf("board move failed: %v", err)
	}
	if res.Metadata["card"] == nil {
		t.Fatal("move result carries no card metadata")
	}

	cards, err := board.Get(store.ProjectID(i.Directory))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cards {
		if c.ID == card.ID && c.Column != "done" {
			t.Fatalf("card %s not moved to done, column=%s", card.ID, c.Column)
		}
	}

	// the mutation must be broadcast so other clients refresh
	select {
	case ev := <-ch:
		if ev.Type != "board.updated" {
			t.Fatalf("expected board.updated event, got %q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no board.updated event published")
	}
}

// An explicit card ID moves that card (not just the session-linked one), and
// list exposes every card with its ID.
func TestBoardToolListAndExplicitCard(t *testing.T) {
	i, s := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	linked := linkCard(t, i, s, "Linked task", "inprogress")
	other, err := board.Add(store.ProjectID(i.Directory), "Other task", nil)
	if err != nil {
		t.Fatal(err)
	}

	bt, _ := registryFor(i).Get("board")

	// list shows both cards with their IDs
	res, err := bt.Run(&tool.Ctx{SessionID: s.ID, Directory: i.Directory}, json.RawMessage(`{"action":"list"}`))
	if err != nil {
		t.Fatalf("board list failed: %v", err)
	}
	for _, id := range []string{linked.ID, other.ID} {
		if !strings.Contains(res.Output, id) {
			t.Fatalf("list output missing card id %s:\n%s", id, res.Output)
		}
	}

	// move by explicit ID
	if _, err := bt.Run(&tool.Ctx{SessionID: s.ID, Directory: i.Directory},
		json.RawMessage(`{"action":"move","card":"`+other.ID+`","column":"done"}`)); err != nil {
		t.Fatalf("explicit move failed: %v", err)
	}
	cards, _ := board.Get(store.ProjectID(i.Directory))
	for _, c := range cards {
		switch c.ID {
		case other.ID:
			if c.Column != "done" {
				t.Fatalf("card %s not moved to done, column=%s", other.ID, c.Column)
			}
		case linked.ID:
			if c.Column != "inprogress" {
				t.Fatalf("unrelated card %s was moved, column=%s", linked.ID, c.Column)
			}
		}
	}

	// unknown IDs produce a helpful error, not a silent success
	_, err = bt.Run(&tool.Ctx{SessionID: s.ID, Directory: i.Directory},
		json.RawMessage(`{"action":"move","card":"card_nope","column":"done"}`))
	if err == nil || !strings.Contains(err.Error(), "card_nope") {
		t.Fatalf("expected card-not-found error mentioning the id, got %v", err)
	}
}

// The system prompt must tell the agent which card it is working on — with
// its ID — so it can move the card to done when finished, plus how to work
// the card (the card's task itself arrives as the plain user message).
func TestBoardContextIncludesCardID(t *testing.T) {
	i, s := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	if bc := boardContext(i, s, "build"); bc != "" {
		t.Fatalf("expected no board context without a linked card, got %q", bc)
	}

	card := linkCard(t, i, s, "Write the parser", "inprogress")
	bc := boardContext(i, s, "build")
	if !strings.Contains(bc, card.ID) {
		t.Fatalf("board context missing card id %s:\n%s", card.ID, bc)
	}
	if !strings.Contains(bc, "done") {
		t.Fatalf("board context missing done-column guidance:\n%s", bc)
	}
	if !strings.Contains(bc, "user's message") {
		t.Fatalf("board context missing task-message guidance:\n%s", bc)
	}
}

// End to end: a session started from a board card, whose model calls the
// board tool with {"action":"move","column":"done"}, must end with the card
// in the done column.
func TestAgentMovesCardToDoneEndToEnd(t *testing.T) {
	var apiCalls int32
	h := func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "title generator") {
			// async title-generation call: answer immediately
			respondEmptyStop(w)
			return
		}
		switch atomic.AddInt32(&apiCalls, 1) {
		case 1:
			chunk := map[string]any{
				"choices": []map[string]any{{
					"delta": map[string]any{"tool_calls": []map[string]any{{
						"index": 0, "id": "call_1", "type": "function",
						"function": map[string]any{"name": "board", "arguments": `{"action":"move","column":"done"}`},
					}}},
				}},
			}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			final := map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "tool_calls"}}}
			b, _ = json.Marshal(final)
			fmt.Fprintf(w, "data: %s\n\n", b)
		default:
			txt := map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "Task complete."}, "finish_reason": "stop"}}}
			b, _ := json.Marshal(txt)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}

	i, s := setupAbortTest(t, h)
	card := linkCard(t, i, s, "Ship the feature", "inprogress")

	if _, err := i.Prompt(s, textInput("finish the card")); err != nil {
		t.Fatalf("prompt failed: %v", err)
	}

	cards, err := board.Get(store.ProjectID(i.Directory))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cards {
		if c.ID == card.ID && c.Column != "done" {
			t.Fatalf("agent did not move card %s to done, column=%s", card.ID, c.Column)
		}
	}

	// the board tool part must be recorded on the assistant message
	msgs, _ := i.Mgr.LoadMessages(s)
	found := false
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == "tool" && p.Tool == "board" && p.State != nil && p.State.Status == "completed" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("no completed board tool part stored")
	}
}

// The board tool must be offered to the model (registry defs include it).
func TestBoardToolIsAdvertised(t *testing.T) {
	i, _ := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defs := registryFor(i).Defs(activeToolNames(i.Directory, i.Agent("build")))
	found := false
	for _, d := range defs {
		if d.Name == "board" {
			found = true
		}
	}
	if !found {
		t.Fatal("board tool missing from tool defs")
	}
}

// Cards can only be added to the open column — the tool rejects other
// columns and lands cards in open otherwise.
func TestBoardToolAddOnlyOpen(t *testing.T) {
	i, s := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	bt, ok := registryFor(i).Get("board")
	if !ok {
		t.Fatal("board tool not registered")
	}
	ctx := &tool.Ctx{SessionID: s.ID, Directory: i.Directory}

	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"add","title":"Straight to done","column":"done"}`)); err == nil {
		t.Fatal("expected an error adding a card to the done column")
	}
	res, err := bt.Run(ctx, json.RawMessage(`{"action":"add","title":"Follow-up"}`))
	if err != nil {
		t.Fatalf("add without column failed: %v", err)
	}
	cards, err := board.Get(store.ProjectID(i.Directory))
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 || cards[0].Column != "open" || cards[0].Title != "Follow-up" {
		t.Fatalf("cards = %+v, want one open card %q", cards, res.Title)
	}
}

// Starting a card prompts the fresh session with the card's title as the
// user message and its attachments as file parts (the composer's shape).
func TestStartCardPromptsAttachments(t *testing.T) {
	i, _ := setupAbortTest(t, textSSE("Working on it."))
	projectID := store.ProjectID(i.Directory)
	atts := []board.Attachment{
		{Type: "file", MIME: "image/png", URL: "data:image/png;base64,AAAA", Filename: "shot.png"},
	}
	card, err := board.Add(projectID, "Fix the login page", atts)
	if err != nil {
		t.Fatal(err)
	}

	sessID, err := startCard(i, projectID, card.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := i.Mgr.Find(sessID)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, func() bool { return watcherReplied(i, sess, "Working on it.") })

	msgs, err := i.Mgr.LoadMessages(sess)
	if err != nil {
		t.Fatal(err)
	}
	text, file := "", false
	for _, m := range msgs {
		if _, ok := m.Info.(session.UserMessage); !ok {
			continue
		}
		for _, p := range m.Parts {
			switch p.Type {
			case "text":
				text += p.Text
			case "file":
				file = p.Mime == "image/png" && p.URL == "data:image/png;base64,AAAA" && p.Filename == "shot.png"
			}
		}
	}
	if text != "Fix the login page" {
		t.Fatalf("user text = %q, want the card title", text)
	}
	if !file {
		t.Fatal("card attachment was not prompted as a file part")
	}
}
