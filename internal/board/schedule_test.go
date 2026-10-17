package board

import (
	"errors"
	"testing"
)

// SetSchedule stores a start time on an open card and it survives a reload;
// a card that already started cannot be scheduled, but its schedule can
// always be cleared (a start consumes it on any column).
func TestSetSchedule(t *testing.T) {
	projectID := setup(t)

	card, err := Add(projectID, "Scheduled task", nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := SetSchedule(projectID, card.ID, 1234567890000)
	if err != nil {
		t.Fatal(err)
	}
	if got.StartAt != 1234567890000 {
		t.Fatalf("StartAt = %d, want 1234567890000", got.StartAt)
	}
	cards, err := Get(projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 || cards[0].StartAt != 1234567890000 {
		t.Fatalf("schedule not persisted: %+v", cards)
	}

	// clearing always works…
	if _, err := SetSchedule(projectID, card.ID, 0); err != nil {
		t.Fatal(err)
	}
	cards, _ = Get(projectID)
	if cards[0].StartAt != 0 {
		t.Fatalf("schedule not cleared: %+v", cards[0])
	}

	// …but setting one is only allowed while the card waits in open
	col := "inprogress"
	if _, err := Update(projectID, card.ID, nil, &col, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := SetSchedule(projectID, card.ID, 1234567890000); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("expected ErrNotOpen for an inprogress card, got %v", err)
	}
	// clearing a started card's leftover schedule is fine
	if _, err := SetSchedule(projectID, card.ID, 0); err != nil {
		t.Fatalf("clearing on a non-open card failed: %v", err)
	}

	if _, err := SetSchedule(projectID, "404", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// Moving a card out of open consumes its schedule — a start (by hand, by the
// scheduler, or by a done-link trigger) goes through Update, so the schedule
// is cleared no matter which path started the card.
func TestMoveOutOfOpenClearsSchedule(t *testing.T) {
	projectID := setup(t)

	card, err := Add(projectID, "Will be dragged away", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SetSchedule(projectID, card.ID, 1234567890000); err != nil {
		t.Fatal(err)
	}

	col := "inprogress"
	if _, err := Update(projectID, card.ID, nil, &col, nil, nil); err != nil {
		t.Fatal(err)
	}
	cards, err := Get(projectID)
	if err != nil {
		t.Fatal(err)
	}
	if cards[0].StartAt != 0 {
		t.Fatalf("schedule survived the move out of open: %+v", cards[0])
	}
}
