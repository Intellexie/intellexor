package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Intellexie/intellexor/internal/board"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/tool"
)

// textSSE answers every completion with a fixed text (title-generation calls
// get the empty stop so they never take a slot).
func textSSE(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "title generator") {
			respondEmptyStop(w)
			return
		}
		chunk := map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": text}, "finish_reason": "stop"}}}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}
}

// The watch/unwatch actions must maintain the card's watcher list: watching
// by explicit ID, by the session-linked card, deduplicated, and removable.
func TestBoardToolWatchUnwatch(t *testing.T) {
	i, s := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	projectID := store.ProjectID(i.Directory)
	dep, err := board.Add(projectID, "Dependency", nil)
	if err != nil {
		t.Fatal(err)
	}
	linked := linkCard(t, i, s, "Linked task", "inprogress")

	bt, _ := registryFor(i).Get("board")
	ctx := &tool.Ctx{SessionID: s.ID, Directory: i.Directory}

	// watch by explicit ID
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"watch","card":"`+dep.ID+`"}`)); err != nil {
		t.Fatalf("watch failed: %v", err)
	}

	// the session's own (linked) card cannot be watched — the agent is
	// already working on it — neither by omitting the ID…
	_, err = bt.Run(ctx, json.RawMessage(`{"action":"watch"}`))
	if err == nil || !strings.Contains(err.Error(), "already working on it") {
		t.Fatalf("expected own-card watch to be rejected, got %v", err)
	}
	// …nor by passing its ID explicitly
	_, err = bt.Run(ctx, json.RawMessage(`{"action":"watch","card":"`+linked.ID+`"}`))
	if err == nil || !strings.Contains(err.Error(), "already working on it") {
		t.Fatalf("expected own-card watch to be rejected, got %v", err)
	}
	// unwatching it is a harmless no-op (e.g. legacy data)
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"unwatch"}`)); err != nil {
		t.Fatalf("unwatch of the linked card failed: %v", err)
	}

	cards, _ := board.Get(projectID)
	byID := map[string]board.Card{}
	for _, c := range cards {
		byID[c.ID] = c
	}
	if w := byID[dep.ID].Watchers; len(w) != 1 || w[0] != s.ID {
		t.Fatalf("dependency card watchers = %v, want [%s]", w, s.ID)
	}
	if w := byID[linked.ID].Watchers; len(w) != 0 {
		t.Fatalf("linked card should have no watchers, got %v", w)
	}

	// watching twice must not duplicate the watcher
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"watch","card":"`+dep.ID+`"}`)); err != nil {
		t.Fatalf("re-watch failed: %v", err)
	}
	cards, _ = board.Get(projectID)
	for _, c := range cards {
		if c.ID == dep.ID && len(c.Watchers) != 1 {
			t.Fatalf("duplicate watcher: %v", c.Watchers)
		}
	}

	// unwatch removes the session again
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"unwatch","card":"`+dep.ID+`"}`)); err != nil {
		t.Fatalf("unwatch failed: %v", err)
	}
	cards, _ = board.Get(projectID)
	for _, c := range cards {
		if c.ID == dep.ID && len(c.Watchers) != 0 {
			t.Fatalf("watcher not removed: %v", c.Watchers)
		}
	}

	// unknown IDs produce a helpful error
	_, err = bt.Run(ctx, json.RawMessage(`{"action":"watch","card":"card_nope"}`))
	if err == nil || !strings.Contains(err.Error(), "card_nope") {
		t.Fatalf("expected card-not-found error mentioning the id, got %v", err)
	}
}

// End to end: a session watching a card must be prompted on behalf of the
// user when the card is moved to done — no matter who moves it (here: a
// direct board.Update, as the web UI's drag or another agent would).
func TestWatchedCardNotifiesWatcherOnDone(t *testing.T) {
	i, s := setupAbortTest(t, textSSE("Noted — the dependency is done."))
	projectID := store.ProjectID(i.Directory)
	card, err := board.Add(projectID, "Dependency", nil)
	if err != nil {
		t.Fatal(err)
	}

	bt, _ := registryFor(i).Get("board")
	if _, err := bt.Run(&tool.Ctx{SessionID: s.ID, Directory: i.Directory},
		json.RawMessage(`{"action":"watch","card":"`+card.ID+`"}`)); err != nil {
		t.Fatalf("watch failed: %v", err)
	}

	col := "done"
	if _, err := board.Update(projectID, card.ID, nil, &col, nil, nil); err != nil {
		t.Fatal(err)
	}

	// the watcher receives the notification as a user message…
	waitUntil(t, 5*time.Second, func() bool {
		return hasUserText(i, s, "has been marked done", card.ID)
	})
	if !hasUserText(i, s, "has been marked done", card.ID) {
		msgs, _ := i.Mgr.LoadMessages(s)
		t.Fatalf("watcher was not notified, messages: %+v", msgs)
	}

	// …and the model answers it (the prompt actually ran)
	waitUntil(t, 5*time.Second, func() bool {
		msgs, _ := i.Mgr.LoadMessages(s)
		for j := len(msgs) - 1; j >= 0; j-- {
			if _, ok := msgs[j].Info.(session.AssistantMessage); ok {
				for _, p := range msgs[j].Parts {
					if p.Type == "text" && strings.Contains(p.Text, "dependency is done") {
						return true
					}
				}
			}
		}
		return false
	})
	if !watcherReplied(i, s, "dependency is done") {
		t.Fatal("watcher session never ran the model on the notification")
	}
}

// A card that is not watched (or no longer watched) must not trigger anything.
func TestUnwatchedCardDoesNotNotify(t *testing.T) {
	i, s := setupAbortTest(t, textSSE("unused"))
	projectID := store.ProjectID(i.Directory)
	card, err := board.Add(projectID, "Nobody watches me", nil)
	if err != nil {
		t.Fatal(err)
	}

	bt, _ := registryFor(i).Get("board")
	ctx := &tool.Ctx{SessionID: s.ID, Directory: i.Directory}
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"watch","card":"`+card.ID+`"}`)); err != nil {
		t.Fatalf("watch failed: %v", err)
	}
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"unwatch","card":"`+card.ID+`"}`)); err != nil {
		t.Fatalf("unwatch failed: %v", err)
	}

	col := "done"
	if _, err := board.Update(projectID, card.ID, nil, &col, nil, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	msgs, _ := i.Mgr.LoadMessages(s)
	if len(msgs) != 0 {
		t.Fatalf("unwatched session was prompted: %d messages", len(msgs))
	}
}

// The start action must mirror the web UI's Start button: a fresh session
// linked to the card, the card moved to inprogress, and the card's title
// sent as the new session's first user message.
func TestBoardToolStartsCard(t *testing.T) {
	i, s := setupAbortTest(t, textSSE("Working on it."))
	projectID := store.ProjectID(i.Directory)
	card, err := board.Add(projectID, "Implement the parser", nil)
	if err != nil {
		t.Fatal(err)
	}

	bt, _ := registryFor(i).Get("board")
	res, err := bt.Run(&tool.Ctx{SessionID: s.ID, Directory: i.Directory},
		json.RawMessage(`{"action":"start","card":"`+card.ID+`"}`))
	if err != nil {
		t.Fatalf("start failed: %v", err)
	}
	sessID, _ := res.Metadata["sessionID"].(string)
	if sessID == "" {
		t.Fatal("start result carries no sessionID")
	}
	if sessID == s.ID {
		t.Fatal("start reused the calling session")
	}

	// the card is linked to the new session and moved to inprogress
	cards, _ := board.Get(projectID)
	for _, c := range cards {
		if c.ID != card.ID {
			continue
		}
		if c.Column != "inprogress" {
			t.Fatalf("card column = %s, want inprogress", c.Column)
		}
		if c.SessionID != sessID {
			t.Fatalf("card linked to %q, want %q", c.SessionID, sessID)
		}
	}

	// the new session exists and is prompted with the card's title
	child, err := i.Mgr.Find(sessID)
	if err != nil {
		t.Fatalf("started session not found: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		return hasUserText(i, child, card.Title)
	})
	if !hasUserText(i, child, card.Title) {
		t.Fatal("started session was not prompted with the card title")
	}
	// wait for the reply to finish streaming too: the prompt runs
	// asynchronously and the temp dir cleanup races with part files still
	// being written when the test ends (flaky "directory not empty")
	waitUntil(t, 5*time.Second, func() bool { return watcherReplied(i, child, "Working on it.") })
}

// Starting a card without an ID (or with an unknown one) must fail loudly.
func TestBoardToolStartValidation(t *testing.T) {
	i, s := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	bt, _ := registryFor(i).Get("board")
	ctx := &tool.Ctx{SessionID: s.ID, Directory: i.Directory}

	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"start"}`)); err == nil {
		t.Fatal("start without a card ID must fail")
	}
	_, err := bt.Run(ctx, json.RawMessage(`{"action":"start","card":"card_nope"}`))
	if err == nil || !strings.Contains(err.Error(), "card_nope") {
		t.Fatalf("expected card-not-found error mentioning the id, got %v", err)
	}
}

// hasUserText reports whether the session has a user message whose text
// contains all given substrings.
func hasUserText(i *Instance, s *session.Session, want ...string) bool {
	msgs, _ := i.Mgr.LoadMessages(s)
	for _, m := range msgs {
		if _, ok := m.Info.(session.UserMessage); !ok {
			continue
		}
		for _, p := range m.Parts {
			if p.Type != "text" {
				continue
			}
			ok := true
			for _, w := range want {
				if !strings.Contains(p.Text, w) {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
	}
	return false
}

func watcherReplied(i *Instance, s *session.Session, substr string) bool {
	msgs, _ := i.Mgr.LoadMessages(s)
	for j := len(msgs) - 1; j >= 0; j-- {
		if _, ok := msgs[j].Info.(session.AssistantMessage); !ok {
			continue
		}
		for _, p := range msgs[j].Parts {
			if p.Type == "text" && strings.Contains(p.Text, substr) {
				return true
			}
		}
	}
	return false
}
