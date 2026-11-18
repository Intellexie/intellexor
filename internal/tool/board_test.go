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

// The edit action changes fields on a card that is still waiting in
// planning or open; started or finished cards are rejected.
func TestBoardToolEdit(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	updates := 0
	bt := BoardTool{OnUpdate: func() { updates++ }}
	ctx := &Ctx{Directory: t.TempDir()}
	projectID := store.ProjectID(ctx.Directory)

	// a blocker card so blockedBy can be exercised
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"add","title":"Blocker"}`)); err != nil {
		t.Fatalf("add blocker failed: %v", err)
	}
	res, err := bt.Run(ctx, json.RawMessage(`{"action":"add","title":"Draft task"}`))
	if err != nil {
		t.Fatalf("add failed: %v", err)
	}
	card, _ := res.Metadata["card"].(board.Card)

	// edit several fields at once; only passed fields change
	res, err = bt.Run(ctx, json.RawMessage(`{"action":"edit","card":"`+card.ID+`","title":"Refined task","description":"More context","criteria":"Tests pass","agent":"build","model":"anthropic/claude-sonnet-4","isolate":true,"blockedBy":["1"]}`))
	if err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	if updates != 3 {
		t.Fatalf("edit did not broadcast (updates=%d)", updates)
	}
	cards, _ := board.Get(projectID)
	edited := cards[1]
	if edited.Title != "Refined task" || edited.Description != "More context" || edited.Criteria != "Tests pass" ||
		edited.Agent != "build" || edited.Model != "anthropic/claude-sonnet-4" || !edited.Isolate {
		t.Fatalf("edit not persisted: %+v", edited)
	}
	if len(edited.BlockedBy) != 1 || edited.BlockedBy[0] != "1" {
		t.Fatalf("blockedBy not persisted: %+v", edited.BlockedBy)
	}
	// the untouched card is unchanged
	if cards[0].Title != "Blocker" || cards[0].Isolate {
		t.Fatalf("sibling card was modified: %+v", cards[0])
	}
	if !strings.Contains(res.Output, "updated") {
		t.Fatalf("unexpected edit output: %s", res.Output)
	}

	// an empty blockedBy array clears the dependencies; self-references
	// and duplicates are dropped
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"edit","card":"`+card.ID+`","blockedBy":["`+card.ID+`","1","1"]}`)); err != nil {
		t.Fatalf("edit blockedBy failed: %v", err)
	}
	cards, _ = board.Get(projectID)
	if len(cards[1].BlockedBy) != 1 || cards[1].BlockedBy[0] != "1" {
		t.Fatalf("blockedBy dedupe failed: %+v", cards[1].BlockedBy)
	}
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"edit","card":"`+card.ID+`","blockedBy":[]}`)); err != nil {
		t.Fatalf("clearing blockedBy failed: %v", err)
	}
	cards, _ = board.Get(projectID)
	if len(cards[1].BlockedBy) != 0 {
		t.Fatalf("blockedBy not cleared: %+v", cards[1].BlockedBy)
	}

	// bad input: unknown card, no fields, empty title, unknown blocker
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"edit","card":"nope","title":"X"}`)); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("expected not-found error mentioning the id, got %v", err)
	}
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"edit","card":"`+card.ID+`"}`)); err == nil || !strings.Contains(err.Error(), "at least one field") {
		t.Fatalf("expected no-fields error, got %v", err)
	}
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"edit","card":"`+card.ID+`","title":"   "}`)); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-title error, got %v", err)
	}
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"edit","card":"`+card.ID+`","blockedBy":["404"]}`)); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected unknown-blocker error, got %v", err)
	}
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"edit","title":"X"}`)); err == nil {
		t.Fatal("expected an error editing without a card ID")
	}

	// a started card (no longer waiting) cannot be edited
	col := "inprogress"
	if _, err := board.Update(projectID, card.ID, nil, &col, nil, nil); err != nil {
		t.Fatalf("move failed: %v", err)
	}
	if _, err := bt.Run(ctx, json.RawMessage(`{"action":"edit","card":"`+card.ID+`","title":"Too late"}`)); err == nil || !strings.Contains(err.Error(), "planning or open") {
		t.Fatalf("expected not-waiting error, got %v", err)
	}
	cards, _ = board.Get(projectID)
	if cards[1].Title != "Refined task" {
		t.Fatalf("non-waiting card was edited: %+v", cards[1])
	}
}
