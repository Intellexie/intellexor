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

	first, err := Add(projectID, "First", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Add(projectID, "Second", nil)
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
	card, err := Add(projectID, "With attachments", atts)
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
