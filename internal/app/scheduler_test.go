package app

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Intellexie/intellexor/internal/board"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

// The scheduler starts cards whose startAt has passed — same flow as the
// Start button (fresh session, card moved to inprogress and linked to it,
// title prompted as the task) — and consumes the schedule. Future schedules
// and already-started cards are left alone.
func TestSchedulerStartsDueCards(t *testing.T) {
	i, _ := setupAbortTest(t, textSSE("Working on it."))
	projectID := store.ProjectID(i.Directory)

	due, err := board.Add(projectID, "Due now", nil)
	if err != nil {
		t.Fatal(err)
	}
	later, err := board.Add(projectID, "Due later", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.SetSchedule(projectID, due.ID, util.Now()-time.Minute.Milliseconds()); err != nil {
		t.Fatal(err)
	}
	if _, err := board.SetSchedule(projectID, later.ID, util.Now()+time.Hour.Milliseconds()); err != nil {
		t.Fatal(err)
	}

	schedulerPass()

	cards, err := board.Get(projectID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]board.Card{}
	for _, c := range cards {
		byID[c.ID] = c
	}
	if c := byID[due.ID]; c.Column != "inprogress" || c.SessionID == "" {
		t.Fatalf("due card not started: column=%q sessionID=%q", c.Column, c.SessionID)
	}
	if c := byID[due.ID]; c.StartAt != 0 {
		t.Fatalf("started card kept its schedule: startAt=%d", c.StartAt)
	}
	if c := byID[later.ID]; c.Column != "open" || c.SessionID != "" || c.StartAt == 0 {
		t.Fatalf("future card was started: %+v", c)
	}

	// the started card's session was prompted with the card title
	sess, err := i.Mgr.Find(byID[due.ID].SessionID)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, func() bool { return watcherReplied(i, sess, "Working on it.") })
	msgs, err := i.Mgr.LoadMessages(sess)
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	for _, m := range msgs {
		if _, ok := m.Info.(session.UserMessage); ok {
			for _, p := range m.Parts {
				if p.Type == "text" {
					text += p.Text
				}
			}
		}
	}
	if text != "Due now" {
		t.Fatalf("prompted text = %q, want the card title", text)
	}
}

// A card that is started by hand while its schedule sits due must not be
// double-started by a sweep: startCard refuses cards that are no longer
// waiting in open, and the sweep drops the stale schedule.
func TestSchedulerDoesNotDoubleStart(t *testing.T) {
	i, _ := setupAbortTest(t, textSSE("Working on it."))
	projectID := store.ProjectID(i.Directory)

	card, err := board.Add(projectID, "Started by hand", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.SetSchedule(projectID, card.ID, util.Now()-time.Minute.Milliseconds()); err != nil {
		t.Fatal(err)
	}

	// manual start wins the race
	if _, err := startCard(i, projectID, card.ID); err != nil {
		t.Fatal(err)
	}
	schedulerPass()

	cards, _ := board.Get(projectID)
	if len(cards) != 1 {
		t.Fatalf("got %d cards, want 1", len(cards))
	}
	if cards[0].SessionID == "" {
		t.Fatal("card has no session")
	}
	sess, err := i.Mgr.Find(cards[0].SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// the manual start's prompt runs asynchronously — wait for it to land
	// (the reply proves the turn ran) before counting
	waitUntil(t, 5*time.Second, func() bool { return watcherReplied(i, sess, "Working on it.") })
	msgs, _ := i.Mgr.LoadMessages(sess)
	n := 0
	for _, m := range msgs {
		if _, ok := m.Info.(session.UserMessage); ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("got %d user messages, want 1 (double start)", n)
	}
}

// A scheduled start must pin the same model a manual Start click would: the
// UI-equivalent default (first connected provider + its preferred model) on
// both the session and the prompt — never the prompt-time config fallback,
// which may name a provider the user cannot use (the minimax bad-key
// incident). Here the config model deliberately points at a provider that
// does not exist; the start must still succeed on the connected default.
func TestSchedulerStartPinsDefaultModel(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	srv := httptest.NewServer(textSSE("Working on it."))
	t.Cleanup(srv.Close)

	work := t.TempDir()
	cfg := fmt.Sprintf(`{
		"model": "minimax/bad",
		"small_model": "test/m",
		"provider": {"test": {"name": "test",
			"options": {"baseURL": %q, "apiKey": "k"},
			"models": {"m": {"name": "m", "tool_call": true,
				"limit": {"context": 100000, "output": 4096}}}}}
	}`, srv.URL+"/v1")
	if err := os.WriteFile(filepath.Join(work, "intellexor.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	i := New(work)
	projectID := store.ProjectID(i.Directory)

	card, err := board.Add(projectID, "Scheduled with default model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.SetSchedule(projectID, card.ID, util.Now()-time.Minute.Milliseconds()); err != nil {
		t.Fatal(err)
	}

	schedulerPass()

	cards, err := board.Get(projectID)
	if err != nil {
		t.Fatal(err)
	}
	var started *board.Card
	for idx := range cards {
		if cards[idx].ID == card.ID {
			started = &cards[idx]
		}
	}
	if started == nil || started.Column != "inprogress" || started.SessionID == "" {
		t.Fatalf("due card not started: %+v", started)
	}

	sess, err := i.Mgr.Find(started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// the session carries the UI-equivalent default, not the config model
	if sess.Model == nil || sess.Model.ProviderID != "test" || sess.Model.ID != "m" {
		t.Fatalf("session model = %+v, want test/m", sess.Model)
	}

	// the turn ran on that model: the reply proves the prompt resolved to
	// the connected provider instead of erroring on the config's minimax
	waitUntil(t, 5*time.Second, func() bool { return watcherReplied(i, sess, "Working on it.") })
	msgs, err := i.Mgr.LoadMessages(sess)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range msgs {
		if u, ok := m.Info.(session.UserMessage); ok {
			found = true
			if u.Model.ProviderID != "test" || u.Model.ModelID != "m" {
				t.Fatalf("user message model = %+v, want test/m", u.Model)
			}
		}
	}
	if !found {
		t.Fatal("no user message recorded")
	}
}
