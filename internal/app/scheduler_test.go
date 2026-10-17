package app

import (
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
