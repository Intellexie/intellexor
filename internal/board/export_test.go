package board

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/Intellexie/intellexor/internal/store"
)

// roundTrip marshals and unmarshals the payload, simulating the JSON file
// the browser downloads and later uploads.
func roundTripBoard(t *testing.T, p *ExportPayload) *ExportPayload {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var out ExportPayload
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

// buildExportBoard creates a board worth exporting: three cards across all
// columns, a done-link, a session link, a watcher and a schedule.
func buildExportBoard(t *testing.T, projectID string) {
	t.Helper()
	first, err := Add(projectID, "First", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Add(projectID, "Second", nil)
	if err != nil {
		t.Fatal(err)
	}
	third, err := Add(projectID, "Third", nil)
	if err != nil {
		t.Fatal(err)
	}
	// first done -> second auto-starts; second is being worked on
	if _, err := SetLink(projectID, first.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	col := "inprogress"
	sess := "sess_abcdef"
	if _, err := Update(projectID, second.ID, nil, &col, nil, &sess); err != nil {
		t.Fatal(err)
	}
	if _, err := Watch(projectID, third.ID, "sess_watcher", true); err != nil {
		t.Fatal(err)
	}
	done := "done"
	if _, err := Update(projectID, third.ID, nil, &done, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestBoardExportCarriesWholeBoard(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()
	projectID := setup(t)
	buildExportBoard(t, projectID)

	payload, err := Export(projectID)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Format != BoardExportFormat || payload.Version != 1 {
		t.Fatalf("unexpected payload header: %+v", payload)
	}
	if len(payload.Cards) != 3 {
		t.Fatalf("expected 3 cards, got %d", len(payload.Cards))
	}
	byTitle := map[string]Card{}
	for _, c := range payload.Cards {
		byTitle[c.Title] = c
	}
	if c := byTitle["First"]; c.Link == "" {
		t.Fatal("export must carry the done-link")
	}
	if c := byTitle["Second"]; c.Column != "inprogress" || c.SessionID == "" {
		t.Fatalf("started card exported wrong: %+v", c)
	}
	if c := byTitle["Third"]; c.Column != "done" || len(c.Watchers) != 1 {
		t.Fatalf("done card exported wrong: %+v", c)
	}
}

// Replace makes the board exactly the import: current cards are gone,
// exported ids, columns, orders and links are kept, and session references
// are dropped (they point at the exporting project's sessions).
func TestBoardImportReplace(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()
	src := setup(t)
	buildExportBoard(t, src)
	payload, err := Export(src)
	if err != nil {
		t.Fatal(err)
	}

	dst := store.ProjectID(t.TempDir()) // second project in the same data dir
	if _, err := Add(dst, "Existing", nil); err != nil {
		t.Fatal(err)
	}

	n, err := Import(dst, roundTripBoard(t, payload), ImportReplace)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("imported %d cards, want 3", n)
	}
	cards, err := Get(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 3 {
		t.Fatalf("replace must discard the previous board, got %+v", cards)
	}
	byTitle := map[string]Card{}
	for _, c := range cards {
		byTitle[c.Title] = c
	}
	first, second, third := byTitle["First"], byTitle["Second"], byTitle["Third"]
	if first.Link == "" || first.Link != second.ID {
		t.Fatalf("done-link must survive replace: first=%+v second=%+v", first, second)
	}
	if second.Column != "inprogress" {
		t.Fatalf("column must be kept, got %q", second.Column)
	}
	if second.SessionID != "" || len(third.Watchers) != 0 {
		t.Fatalf("session references must be cleared: %+v %+v", second, third)
	}
	if third.Column != "done" {
		t.Fatalf("done column must be kept, got %q", third.Column)
	}
	// the source board is untouched
	srcCards, err := Get(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcCards) != 3 {
		t.Fatalf("source board changed: %d cards", len(srcCards))
	}
}

// Merge keeps the current board and appends the imported cards with fresh
// ids at the end of their columns; done-links are remapped to the new ids.
func TestBoardImportMerge(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()
	src := setup(t)
	buildExportBoard(t, src)
	payload, err := Export(src)
	if err != nil {
		t.Fatal(err)
	}

	dst := store.ProjectID(t.TempDir()) // second project in the same data dir
	existing, err := Add(dst, "Existing", nil)
	if err != nil {
		t.Fatal(err)
	}

	n, err := Import(dst, roundTripBoard(t, payload), ImportMerge)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("imported %d cards, want 3", n)
	}
	cards, err := Get(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 4 {
		t.Fatalf("merge must keep the existing cards, got %+v", cards)
	}
	byTitle := map[string]Card{}
	for _, c := range cards {
		byTitle[c.Title] = c
	}
	if byTitle["Existing"].ID != existing.ID {
		t.Fatalf("existing card changed: %+v", byTitle["Existing"])
	}
	first, second := byTitle["First"], byTitle["Second"]
	if first.Link != second.ID {
		t.Fatalf("done-link must be remapped to the fresh id: first=%+v second=%+v", first, second)
	}
	if second.ID == existing.ID || first.ID == existing.ID {
		t.Fatal("merged cards must get fresh ids")
	}
	// merged cards land at the end of their columns, after the existing one
	if byTitle["Existing"].Column == second.Column && byTitle["Existing"].Order >= second.Order {
		t.Fatalf("merged card must come after the existing one: %+v vs %+v", byTitle["Existing"], second)
	}
}

// An import without a mode replaces (the natural reading of "import this
// board"); foreign formats and unknown modes are rejected.
func TestBoardImportValidation(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()
	projectID := setup(t)

	if _, err := Add(projectID, "Keep", nil); err != nil {
		t.Fatal(err)
	}

	if _, err := Import(projectID, nil, ImportReplace); err == nil {
		t.Fatal("expected error for nil payload")
	}
	if _, err := Import(projectID, &ExportPayload{Format: "other.thing"}, ImportReplace); err == nil {
		t.Fatal("expected error for foreign format")
	}
	if _, err := Import(projectID, &ExportPayload{Format: BoardExportFormat}, "upsert"); err == nil {
		t.Fatal("expected error for unknown mode")
	}

	// default mode is replace: an empty payload empties the board
	n, err := Import(projectID, &ExportPayload{Format: BoardExportFormat}, "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 cards imported, got %d", n)
	}
	cards, err := Get(projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 0 {
		t.Fatalf("default mode must replace, board still has %+v", cards)
	}
}

// A hand-edited file with a dangling done-link must not crash the import:
// the link is dropped when its target is not part of the board.
func TestBoardImportDropsDanglingLinks(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()
	projectID := setup(t)

	payload := &ExportPayload{
		Format:  BoardExportFormat,
		Version: 1,
		Cards: []Card{
			{ID: "1", Title: "Solo", Column: "open", Link: "99"},
		},
	}
	if _, err := Import(projectID, payload, ImportReplace); err != nil {
		t.Fatal(err)
	}
	cards, err := Get(projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 || cards[0].Link != "" {
		t.Fatalf("dangling link must be dropped, got %+v", cards)
	}
}
