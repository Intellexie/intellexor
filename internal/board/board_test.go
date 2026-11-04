package board

import (
	"os"
	"testing"

	"github.com/Intellexie/intellexor/internal/store"
)

// setup points the data dir at a fresh temp dir (the data dir is cached at
// package level, so every test needs its own).
func setup(t *testing.T) string {
	t.Helper()
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()
	return store.ProjectID(t.TempDir())
}

// Cards can only be created in the open column: Add has no column parameter
// and always lands the card there, in creation order.
func TestAddOnlyCreatesInOpen(t *testing.T) {
	projectID := setup(t)

	first, err := Add(projectID, "First", AddOpts{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Add(projectID, "Second", AddOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Column != "open" || second.Column != "open" {
		t.Fatalf("cards created in %q / %q, want open", first.Column, second.Column)
	}
	if second.Order <= first.Order {
		t.Fatalf("second card order %d should come after first %d", second.Order, first.Order)
	}

	cards, err := Get(projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 {
		t.Fatalf("got %d cards, want 2", len(cards))
	}
}

// Attachments given at creation ride along on the card and survive a
// reload — they are prompted to the session when the card is started.
func TestAddPersistsAttachments(t *testing.T) {
	projectID := setup(t)

	atts := []Attachment{
		{Type: "file", MIME: "image/png", URL: "data:image/png;base64,AAAA", Filename: "shot.png"},
		{Type: "file", MIME: "text/plain", URL: "data:text/plain;base64,aGk=", Filename: "notes.txt"},
	}
	card, err := Add(projectID, "With attachments", AddOpts{Attachments: atts})
	if err != nil {
		t.Fatal(err)
	}
	if len(card.Attachments) != 2 {
		t.Fatalf("got %d attachments on the returned card, want 2", len(card.Attachments))
	}

	// Get always loads from disk, so this proves the attachments persisted
	cards, err := Get(projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 || cards[0].ID != card.ID {
		t.Fatalf("reloaded cards = %+v, want the created card", cards)
	}
	if len(cards[0].Attachments) != 2 ||
		cards[0].Attachments[0].Filename != "shot.png" ||
		cards[0].Attachments[0].MIME != "image/png" ||
		cards[0].Attachments[1].Filename != "notes.txt" {
		t.Fatalf("attachments did not survive the reload: %+v", cards[0].Attachments)
	}
}

// A session can only be linked to a card that is still waiting in open and
// not owned by another session — the same precondition startCard enforces.
// Clearing a sessionID (empty string) is always allowed.
func TestUpdateRejectsSessionLinkOnNonOpenOrOwnedCard(t *testing.T) {
	projectID := setup(t)

	card, err := Add(projectID, "Guarded", AddOpts{})
	if err != nil {
		t.Fatal(err)
	}
	// link session A while the card is open (the start flow) — allowed
	col := "inprogress"
	sessA, sessB := "sess-a", "sess-b"
	if _, err := Update(projectID, card.ID, nil, &col, nil, &sessA); err != nil {
		t.Fatal(err)
	}
	// another session cannot take the card while it is linked to A
	if _, err := Update(projectID, card.ID, nil, nil, nil, &sessB); err != ErrBadSession {
		t.Fatalf("re-linking an owned card: err = %v, want ErrBadSession", err)
	}
	// no session can be linked to a card that already left open
	if _, err := Update(projectID, card.ID, nil, nil, nil, &sessA); err != ErrBadSession {
		t.Fatalf("re-linking an in-progress card: err = %v, want ErrBadSession", err)
	}
	// clearing the link is always allowed (the card goes back to open,
	// released from its session)
	open := "open"
	empty := ""
	if _, err := Update(projectID, card.ID, nil, &open, nil, &empty); err != nil {
		t.Fatal(err)
	}
	// and now B can take it
	if _, err := Update(projectID, card.ID, nil, &col, nil, &sessB); err != nil {
		t.Fatal(err)
	}
}

// Moving a card from inprogress back to open is the "stop" flow: the stale
// session link is cleared (so a fresh Start can link a new session) and
// OnStopped fires with the released session ID so the app layer can abort
// the agent. Moving to done keeps the link (View still works there).
func TestStopReturnsCardToOpenAndFiresOnStopped(t *testing.T) {
	projectID := setup(t)

	card, err := Add(projectID, "Stoppable", AddOpts{})
	if err != nil {
		t.Fatal(err)
	}
	col := "inprogress"
	sess := "sess-a"
	if _, err := Update(projectID, card.ID, nil, &col, nil, &sess); err != nil {
		t.Fatal(err)
	}

	stopped := ""
	prev := OnStopped
	OnStopped = func(pid, sid string) {
		if pid != projectID {
			t.Errorf("OnStopped projectID = %q, want %q", pid, projectID)
		}
		stopped = sid
	}
	defer func() { OnStopped = prev }()

	// drag back to open: no explicit sessionID — the server clears it
	open := "open"
	updated, err := Update(projectID, card.ID, nil, &open, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SessionID != "" {
		t.Fatalf("session link survived the stop: %q", updated.SessionID)
	}
	if stopped != sess {
		t.Fatalf("OnStopped sessionID = %q, want %q", stopped, sess)
	}

	// the card is waiting and unowned again: a fresh session can take it
	sessB := "sess-b"
	if _, err := Update(projectID, card.ID, nil, &col, nil, &sessB); err != nil {
		t.Fatalf("re-starting a stopped card: %v", err)
	}

	// moving to done does NOT clear the link or fire OnStopped
	stopped = ""
	done := "done"
	if _, err := Update(projectID, card.ID, nil, &done, nil, nil); err != nil {
		t.Fatal(err)
	}
	cards, err := Get(projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cards {
		if c.ID == card.ID && c.SessionID != sessB {
			t.Fatalf("moving to done cleared the session link: %+v", c)
		}
	}
	if stopped != "" {
		t.Fatalf("OnStopped fired for a move to done (session %q)", stopped)
	}
}
