package board

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Intellexie/intellexor/internal/store"
)

// Boards created before dependencies existed store a single link field: the
// first load migrates it into blockedBy (keeping link too, deprecated).
func TestLegacyLinkMigratesToBlockedBy(t *testing.T) {
	projectID := setup(t)
	path := filepath.Join(store.DataDir(), "storage", "board", projectID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := []Card{
		{ID: "1", Title: "Source", Column: "done", Order: 0, Created: 1, Link: "2"},
		{ID: "2", Title: "Target", Column: "open", Order: 0, Created: 2},
	}
	b, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	cards, err := Get(projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cards {
		if c.ID != "2" {
			continue
		}
		if len(c.BlockedBy) != 1 || c.BlockedBy[0] != "1" {
			t.Fatalf("legacy link not migrated to blockedBy: %+v", c)
		}
	}

	// the migration is persisted: reloading keeps the dependency
	cards, err = Get(projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cards {
		if c.ID == "2" && (len(c.BlockedBy) != 1 || c.BlockedBy[0] != "1") {
			t.Fatalf("migration not persisted: %+v", c)
		}
	}
}

// SetBlockedBy validates its targets: existing cards only, never the card
// itself; duplicates are dropped and an empty list clears.
func TestSetBlockedByValidation(t *testing.T) {
	projectID := setup(t)
	a, err := Add(projectID, "A", AddOpts{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Add(projectID, "B", AddOpts{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := SetBlockedBy(projectID, b.ID, []string{a.ID, a.ID, b.ID}); err != nil {
		t.Fatalf("valid blockers rejected: %v", err)
	}
	cards, _ := Get(projectID)
	for _, c := range cards {
		if c.ID == b.ID && (len(c.BlockedBy) != 1 || c.BlockedBy[0] != a.ID) {
			t.Fatalf("blockers not stored deduplicated: %+v", c)
		}
	}

	if _, err := SetBlockedBy(projectID, b.ID, []string{"404"}); err == nil {
		t.Fatal("missing blocker must be rejected")
	}

	// deleting a blocker unblocks the dependent card
	if _, err := SetBlockedBy(projectID, b.ID, []string{a.ID}); err != nil {
		t.Fatal(err)
	}
	if err := Delete(projectID, a.ID); err != nil {
		t.Fatal(err)
	}
	cards, _ = Get(projectID)
	for _, c := range cards {
		if c.ID == b.ID && len(c.BlockedBy) != 0 {
			t.Fatalf("deleted blocker not cleared: %+v", c)
		}
	}
}

// The review column is a first-class column: cards can be moved into it and
// display-ordered between inprogress and done.
func TestReviewColumn(t *testing.T) {
	projectID := setup(t)
	card, err := Add(projectID, "Under review", AddOpts{})
	if err != nil {
		t.Fatal(err)
	}
	col := "review"
	if _, err := Update(projectID, card.ID, nil, &col, nil, nil); err != nil {
		t.Fatalf("move to review rejected: %v", err)
	}
	cards, _ := Get(projectID)
	if cards[0].Column != "review" {
		t.Fatalf("card not in review: %+v", cards[0])
	}
	if _, err := Update(projectID, card.ID, nil, strPtr("bogus"), nil, nil); err == nil {
		t.Fatal("bogus column must be rejected")
	}
}

func strPtr(s string) *string { return &s }

// SetRuntime stores the live status snapshot and reports whether anything
// changed, so callers can skip redundant broadcasts.
func TestSetRuntimeChanged(t *testing.T) {
	projectID := setup(t)
	card, err := Add(projectID, "Runtime", AddOpts{})
	if err != nil {
		t.Fatal(err)
	}
	_, changed, err := SetRuntime(projectID, card.ID, Runtime{Status: "running", StartedAt: 42, Cost: 0.5, Tokens: 100})
	if err != nil || !changed {
		t.Fatalf("first runtime write: changed=%v err=%v", changed, err)
	}
	_, changed, err = SetRuntime(projectID, card.ID, Runtime{Status: "running", StartedAt: 42, Cost: 0.5, Tokens: 100})
	if err != nil || changed {
		t.Fatalf("identical runtime write reported change: changed=%v err=%v", changed, err)
	}
	cards, _ := Get(projectID)
	if cards[0].Status != "running" || cards[0].StartedAt != 42 || cards[0].Cost != 0.5 || cards[0].Tokens != 100 {
		t.Fatalf("runtime not stored: %+v", cards[0])
	}
}
