package app

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Intellexie/intellexor/internal/board"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/tool"
)

// Card IDs must be plain numbers counting up from 1 — they are what users
// see and reference (links, conversations), not opaque generated strings.
func TestBoardCardIDsAreNumeric(t *testing.T) {
	i, _ := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	projectID := store.ProjectID(i.Directory)
	first, err := board.Add(projectID, "First", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := board.Add(projectID, "Second", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "1" {
		t.Fatalf("first card ID = %q, want \"1\"", first.ID)
	}
	if second.ID != "2" {
		t.Fatalf("second card ID = %q, want \"2\"", second.ID)
	}

	// IDs are max+1: removing the highest card frees its number again
	if err := board.Delete(projectID, second.ID); err != nil {
		t.Fatal(err)
	}
	third, err := board.Add(projectID, "Third", nil)
	if err != nil {
		t.Fatal(err)
	}
	if third.ID != "2" {
		t.Fatalf("card ID after delete = %q, want \"2\"", third.ID)
	}
}

// Boards created before card IDs became numeric keep working: the first
// load renumbers legacy cards into the numeric scheme and persists that.
func TestBoardLegacyIDsMigrateToNumeric(t *testing.T) {
	i, _ := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	projectID := store.ProjectID(i.Directory)
	path := filepath.Join(store.DataDir(), "storage", "board", projectID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := []board.Card{
		{ID: "card_123", Title: "Old one", Column: "open", Order: 0, Created: 1},
		{ID: "card_456", Title: "Old two", Column: "done", Order: 0, Created: 2},
	}
	b, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	cards, err := board.Get(projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 {
		t.Fatalf("want 2 cards, got %d", len(cards))
	}
	byID := map[string]string{}
	for _, c := range cards {
		byID[c.ID] = c.Title
	}
	if byID["1"] != "Old one" || byID["2"] != "Old two" {
		t.Fatalf("legacy cards not renumbered numerically: %+v", cards)
	}

	// the renumber is persisted: a fresh Add continues after the migrated IDs
	next, err := board.Add(projectID, "New", nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "3" {
		t.Fatalf("card after migration = %q, want \"3\"", next.ID)
	}
}

// Links may only target cards that are still waiting in open — not the card
// itself, not a missing one, not one already picked up.
func TestSetLinkValidation(t *testing.T) {
	i, s := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	projectID := store.ProjectID(i.Directory)
	open, err := board.Add(projectID, "Open target", nil)
	if err != nil {
		t.Fatal(err)
	}
	started := linkCard(t, i, s, "Already started", "inprogress")

	if _, err := board.SetLink(projectID, started.ID, open.ID); err != nil {
		t.Fatalf("link to an open card failed: %v", err)
	}
	cards, _ := board.Get(projectID)
	for _, c := range cards {
		if c.ID == started.ID && c.Link != open.ID {
			t.Fatalf("link not stored: %+v", c)
		}
	}

	// an empty target clears the link again
	if _, err := board.SetLink(projectID, started.ID, ""); err != nil {
		t.Fatal(err)
	}
	cards, _ = board.Get(projectID)
	for _, c := range cards {
		if c.ID == started.ID && c.Link != "" {
			t.Fatalf("link not cleared: %+v", c)
		}
	}

	if _, err := board.SetLink(projectID, started.ID, started.ID); err == nil {
		t.Fatal("self-link must be rejected")
	}
	if _, err := board.SetLink(projectID, started.ID, "404"); err == nil {
		t.Fatal("link to a missing card must be rejected")
	}
	if _, err := board.SetLink(projectID, open.ID, started.ID); err == nil {
		t.Fatal("link to a non-open card must be rejected")
	}
}

// The point of linking: when the source card is done, the target starts
// automatically — a fresh session linked to the target, the target moved to
// inprogress, prompted with its title — exactly like pressing Start.
func TestLinkedCardAutoStartsOnDone(t *testing.T) {
	i, s := setupAbortTest(t, textSSE("Following up."))
	projectID := store.ProjectID(i.Directory)
	source := linkCard(t, i, s, "Source task", "inprogress")
	target, err := board.Add(projectID, "Follow-up task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.SetLink(projectID, source.ID, target.ID); err != nil {
		t.Fatal(err)
	}

	col := "done"
	if _, err := board.Update(projectID, source.ID, nil, &col, nil, nil); err != nil {
		t.Fatal(err)
	}

	var child *session.Session
	waitUntil(t, 5*time.Second, func() bool {
		cards, _ := board.Get(projectID)
		for _, c := range cards {
			if c.ID != target.ID {
				continue
			}
			if c.Column != "inprogress" || c.SessionID == "" {
				return false
			}
			child, err = i.Mgr.Find(c.SessionID)
			return err == nil
		}
		return false
	})
	if child == nil {
		t.Fatal("linked card was not started when the source was done")
	}
	if child.ID == s.ID {
		t.Fatal("auto-start reused the moving session instead of a fresh one")
	}
	waitUntil(t, 5*time.Second, func() bool { return hasUserText(i, child, target.Title) })
	if !hasUserText(i, child, target.Title) {
		t.Fatal("auto-started session was not prompted with the target card's title")
	}
	// the turn must have run to completion before the temp dir is cleaned up
	waitUntil(t, 5*time.Second, func() bool { return watcherReplied(i, child, "Following up.") })
	if !watcherReplied(i, child, "Following up.") {
		t.Fatal("auto-started session never ran the model on the card's task")
	}
}

// The auto-start must be broadcast like every other board mutation
// (board.updated): connected web clients rely on it to refresh the board and
// give the new session a tab, since no client initiated the start.
func TestLinkedCardAutoStartBroadcastsBoardUpdate(t *testing.T) {
	i, s := setupAbortTest(t, textSSE("Following up."))
	projectID := store.ProjectID(i.Directory)
	source := linkCard(t, i, s, "Source task", "inprogress")
	target, err := board.Add(projectID, "Follow-up task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.SetLink(projectID, source.ID, target.ID); err != nil {
		t.Fatal(err)
	}

	ch := i.Hub.Subscribe()
	defer i.Hub.Unsubscribe(ch)

	col := "done"
	if _, err := board.Update(projectID, source.ID, nil, &col, nil, nil); err != nil {
		t.Fatal(err)
	}

	// session.created/updated for the fresh session arrive first; the board
	// broadcast must follow them
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type != "board.updated" {
				continue
			}
			if ev.Properties["directory"] != i.Directory {
				t.Fatalf("board.updated directory = %v, want %q", ev.Properties["directory"], i.Directory)
			}
			// the broadcast fires after the card is linked to its session;
			// let the auto-started turn finish before the temp dir is cleaned up
			var child *session.Session
			cards, _ := board.Get(projectID)
			for _, c := range cards {
				if c.ID == target.ID {
					child, err = i.Mgr.Find(c.SessionID)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			waitUntil(t, 5*time.Second, func() bool { return watcherReplied(i, child, "Following up.") })
			if !watcherReplied(i, child, "Following up.") {
				t.Fatal("auto-started session never ran the model on the card's task")
			}
			return
		case <-deadline:
			t.Fatal("no board.updated event published for the auto-started card")
		}
	}
}

// A link whose target was picked up meanwhile (no longer in open) is a
// harmless no-op: the card keeps its session instead of being re-started.
func TestLinkedCardSkippedWhenAlreadyStarted(t *testing.T) {
	i, s := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	projectID := store.ProjectID(i.Directory)
	source := linkCard(t, i, s, "Source", "inprogress")
	target, err := board.Add(projectID, "Target", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.SetLink(projectID, source.ID, target.ID); err != nil {
		t.Fatal(err)
	}
	// someone starts the target by hand before the source is done
	col := "inprogress"
	if _, err := board.Update(projectID, target.ID, nil, &col, nil, &s.ID); err != nil {
		t.Fatal(err)
	}

	col = "done"
	if _, err := board.Update(projectID, source.ID, nil, &col, nil, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	cards, _ := board.Get(projectID)
	for _, c := range cards {
		if c.ID == target.ID && c.SessionID != s.ID {
			t.Fatalf("already-started target was re-started: session %q", c.SessionID)
		}
	}
}

// Deleting a card clears links pointing at it, so no dangling triggers remain.
func TestDeleteClearsInboundLinks(t *testing.T) {
	i, s := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	projectID := store.ProjectID(i.Directory)
	source := linkCard(t, i, s, "Source", "inprogress")
	target, err := board.Add(projectID, "Target", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.SetLink(projectID, source.ID, target.ID); err != nil {
		t.Fatal(err)
	}
	if err := board.Delete(projectID, target.ID); err != nil {
		t.Fatal(err)
	}
	cards, _ := board.Get(projectID)
	for _, c := range cards {
		if c.ID == source.ID && c.Link != "" {
			t.Fatalf("link to deleted card survived: %+v", c)
		}
	}
}

// The agent can chain cards too: link stores the trigger, unlink clears it
// (resolving the session's own card when no ID is passed), and invalid
// targets are rejected with a helpful error.
func TestBoardToolLinkAction(t *testing.T) {
	i, s := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	projectID := store.ProjectID(i.Directory)
	source := linkCard(t, i, s, "Source", "inprogress")
	target, err := board.Add(projectID, "Target", nil)
	if err != nil {
		t.Fatal(err)
	}

	bt, _ := registryFor(i).Get("board")
	ctx := &tool.Ctx{SessionID: s.ID, Directory: i.Directory}

	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"link","card":"`+source.ID+`","target":"`+target.ID+`"}`)); err != nil {
		t.Fatalf("link failed: %v", err)
	}
	cards, _ := board.Get(projectID)
	for _, c := range cards {
		if c.ID == source.ID && c.Link != target.ID {
			t.Fatalf("tool link not stored: %+v", c)
		}
	}

	// unlink resolves the session's card when no ID is passed
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"unlink"}`)); err != nil {
		t.Fatalf("unlink failed: %v", err)
	}
	cards, _ = board.Get(projectID)
	for _, c := range cards {
		if c.ID == source.ID && c.Link != "" {
			t.Fatalf("unlink not stored: %+v", c)
		}
	}

	// linking the session's card to itself is rejected
	_, err = bt.Run(ctx, json.RawMessage(`{"action":"link","target":"`+source.ID+`"}`))
	if err == nil || !strings.Contains(err.Error(), "open column") {
		t.Fatalf("expected open-column rejection, got %v", err)
	}
}
