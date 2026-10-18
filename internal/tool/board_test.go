package tool

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Intellexie/intellexor/internal/board"
	"github.com/Intellexie/intellexor/internal/store"
)

// The schedule action sets a timed start on a waiting card (via "in" or
// "startAt") and clears it with startAt 0 — the same board.SetSchedule
// mutation the HTTP API performs, broadcast to web clients.
func TestBoardToolSchedule(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	updates := 0
	bt := BoardTool{OnUpdate: func() { updates++ }}
	ctx := &Ctx{Directory: t.TempDir()}
	projectID := store.ProjectID(ctx.Directory)

	res, err := bt.Run(ctx, json.RawMessage(`{"action":"add","title":"Nightly sweep"}`))
	if err != nil {
		t.Fatalf("add failed: %v", err)
	}
	card, _ := res.Metadata["card"].(board.Card)

	// schedule via "in": a duration from now
	res, err = bt.Run(ctx, json.RawMessage(`{"action":"schedule","card":"`+card.ID+`","in":"2h"}`))
	if err != nil {
		t.Fatalf("schedule failed: %v", err)
	}
	if updates != 2 {
		t.Fatalf("schedule did not broadcast (updates=%d)", updates)
	}
	cards, _ := board.Get(projectID)
	if len(cards) != 1 || cards[0].StartAt == 0 {
		t.Fatalf("schedule not persisted: %+v", cards)
	}
	want := time.Now().Add(2 * time.Hour).UnixMilli()
	if diff := cards[0].StartAt - want; diff < -60000 || diff > 60000 {
		t.Fatalf("startAt = %d, want within a minute of %d", cards[0].StartAt, want)
	}
	if !strings.Contains(res.Output, "scheduled to start at") {
		t.Fatalf("unexpected schedule output: %s", res.Output)
	}

	// schedule via explicit startAt (unix ms)
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"schedule","card":"`+card.ID+`","startAt":1234567890}`)); err != nil {
		t.Fatalf("schedule by startAt failed: %v", err)
	}
	cards, _ = board.Get(projectID)
	if len(cards) != 1 || cards[0].StartAt != 1234567890 {
		t.Fatalf("startAt not persisted: %+v", cards)
	}

	// startAt 0 clears the schedule
	res, err = bt.Run(ctx, json.RawMessage(`{"action":"schedule","card":"`+card.ID+`","startAt":0}`))
	if err != nil {
		t.Fatalf("clearing the schedule failed: %v", err)
	}
	if !strings.Contains(res.Output, "Cleared") {
		t.Fatalf("unexpected clear output: %s", res.Output)
	}
	cards, _ = board.Get(projectID)
	if len(cards) != 1 || cards[0].StartAt != 0 {
		t.Fatalf("schedule not cleared: %+v", cards)
	}

	// bad input: unknown card, both time forms, bad duration
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"schedule","card":"nope","in":"1h"}`)); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("expected not-found error mentioning the id, got %v", err)
	}
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"schedule","card":"`+card.ID+`","startAt":5,"in":"1h"}`)); err == nil {
		t.Fatal("expected an error passing both startAt and in")
	}
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"schedule","card":"`+card.ID+`","in":"soon"}`)); err == nil || !strings.Contains(err.Error(), "soon") {
		t.Fatalf("expected invalid-duration error, got %v", err)
	}
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"schedule","in":"1h"}`)); err == nil {
		t.Fatal("expected an error scheduling without a card ID")
	}
}
