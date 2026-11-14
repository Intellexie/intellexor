package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Intellexie/intellexor/internal/board"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
)

// setupBoardTest is setupAbortTest with a configurable board block, so the
// review-stage tests can turn the automatic review run on.
func setupBoardTest(t *testing.T, h http.HandlerFunc, boardCfg string) (*Instance, *session.Session) {
	t.Helper()
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	srv := httptest.NewServer(h)
	work := t.TempDir()
	cfg := fmt.Sprintf(`{
		"model": "test/m",
		"small_model": "test/m",
		%s
		"provider": {"test": {"name": "test",
			"options": {"baseURL": %q, "apiKey": "k"},
			"models": {"m": {"name": "m", "tool_call": true,
				"limit": {"context": 100000, "output": 4096}}}}}
	}`, boardCfg, srv.URL+"/v1")
	if err := os.WriteFile(filepath.Join(work, "intellexor.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	i := New(work)
	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "board test"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	return i, s
}

// cardColumn fetches the card's current column (and the card itself).
func cardByID(t *testing.T, i *Instance, projectID, id string) (board.Card, bool) {
	t.Helper()
	cards, err := board.Get(projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cards {
		if c.ID == id {
			return c, true
		}
	}
	return board.Card{}, false
}

// waitCardSettled waits until the card's session turn has fully ended and
// the post-idle board writes (review move, status sync) have landed — the
// last await before the test's temp dir is cleaned up.
func waitCardSettled(t *testing.T, i *Instance, projectID, cardID string) {
	t.Helper()
	waitUntil(t, 10*time.Second, func() bool {
		c, ok := cardByID(t, i, projectID, cardID)
		return ok && c.SessionID != "" && c.Status != "running"
	})
}

// Finishing one card releases the next wave: every open card whose blockers
// are all done starts (fresh session, moved to inprogress), cards with
// unmet blockers stay waiting.
func TestDependenciesTriggerOnDone(t *testing.T) {
	i, s := setupAbortTest(t, textSSE("Wave two."))
	projectID := store.ProjectID(i.Directory)

	source := linkCard(t, i, s, "Source task", "inprogress")
	free, err := board.Add(projectID, "Unblocked follow-up", board.AddOpts{BlockedBy: []string{source.ID}})
	if err != nil {
		t.Fatal(err)
	}
	chained, err := board.Add(projectID, "Chained follow-up", board.AddOpts{BlockedBy: []string{free.ID}})
	if err != nil {
		t.Fatal(err)
	}

	col := "done"
	if _, err := board.Update(projectID, source.ID, nil, &col, nil, nil); err != nil {
		t.Fatal(err)
	}

	// the unblocked card starts; the chained one stays open
	waitUntil(t, 5*time.Second, func() bool {
		c, _ := cardByID(t, i, projectID, free.ID)
		return c.Column == "inprogress" || c.Column == "review"
	})
	c, _ := cardByID(t, i, projectID, free.ID)
	if c.SessionID == "" {
		t.Fatal("unblocked card started without a session")
	}
	if c, _ := cardByID(t, i, projectID, chained.ID); c.Column != "open" {
		t.Fatalf("chained card started before its blocker was done: %s", c.Column)
	}

	// finishing the second card releases the third
	if _, err := board.Update(projectID, free.ID, nil, &col, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		c, _ := cardByID(t, i, projectID, chained.ID)
		return c.Column == "inprogress" || c.Column == "review"
	})

	// let the auto-started turns run to completion before the temp dir is
	// cleaned up — their prompt loops keep writing session files otherwise
	for _, id := range []string{free.ID, chained.ID} {
		waitCardSettled(t, i, projectID, id)
	}
}

// Moving a card to done must only start cards that are explicitly linked
// (blockedBy): an unlinked open card is not part of the dependency chain
// and waits for a manual start.
func TestDoneDoesNotStartUnlinkedCards(t *testing.T) {
	i, s := setupAbortTest(t, textSSE("Wave two."))
	projectID := store.ProjectID(i.Directory)

	source := linkCard(t, i, s, "Source task", "inprogress")
	linked, err := board.Add(projectID, "Linked follow-up", board.AddOpts{BlockedBy: []string{source.ID}})
	if err != nil {
		t.Fatal(err)
	}
	unlinked, err := board.Add(projectID, "Unlinked bystander", board.AddOpts{})
	if err != nil {
		t.Fatal(err)
	}

	col := "done"
	if _, err := board.Update(projectID, source.ID, nil, &col, nil, nil); err != nil {
		t.Fatal(err)
	}

	// the linked card starts; the unlinked one stays waiting in open
	waitUntil(t, 5*time.Second, func() bool {
		c, _ := cardByID(t, i, projectID, linked.ID)
		return c.Column == "inprogress" || c.Column == "review"
	})
	if c, _ := cardByID(t, i, projectID, unlinked.ID); c.Column != "open" || c.SessionID != "" {
		t.Fatalf("unlinked card auto-started: column=%s session=%q", c.Column, c.SessionID)
	}

	// let the auto-started turn run to completion before the temp dir is
	// cleaned up — its prompt loop keeps writing session files otherwise
	waitCardSettled(t, i, projectID, linked.ID)
}

// The project's max_running cap limits how many cards the dependency flow
// starts at once; manual starts are never capped.
func TestDependencyConcurrencyCap(t *testing.T) {
	i, s := setupBoardTest(t, textSSE("Capped."), `"board": {"max_running": 1, "auto_review": false},`)
	projectID := store.ProjectID(i.Directory)

	source := linkCard(t, i, s, "Source task", "inprogress")
	first, err := board.Add(projectID, "First follow-up", board.AddOpts{BlockedBy: []string{source.ID}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := board.Add(projectID, "Second follow-up", board.AddOpts{BlockedBy: []string{source.ID}})
	if err != nil {
		t.Fatal(err)
	}

	col := "done"
	if _, err := board.Update(projectID, source.ID, nil, &col, nil, nil); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, 5*time.Second, func() bool {
		c, _ := cardByID(t, i, projectID, first.ID)
		return c.Column != "open"
	})
	// exactly one of the two started; the other is still waiting
	started, waiting := first, second
	if c, _ := cardByID(t, i, projectID, first.ID); c.Column == "open" {
		started, waiting = second, first
	}
	if c, _ := cardByID(t, i, projectID, waiting.ID); c.Column != "open" {
		t.Fatalf("cap ignored: both cards started (%s and %s)", started.ID, waiting.ID)
	}
	// let the started turn finish before the temp dir is cleaned up
	waitCardSettled(t, i, projectID, started.ID)
}

// A card picks its agent and model; its description and acceptance criteria
// ride along with the title as the session's task.
func TestCardAgentModelAndSpec(t *testing.T) {
	i, _ := setupAbortTest(t, textSSE("On it."))
	projectID := store.ProjectID(i.Directory)

	card, err := board.Add(projectID, "Build the parser", board.AddOpts{
		Agent:       "plan",
		Model:       "test/m",
		Description: "Parse the config format.",
		Criteria:    "All tests pass.",
	})
	if err != nil {
		t.Fatal(err)
	}
	sessID, err := startCard(i, projectID, card.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := i.Mgr.Find(sessID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Agent != "plan" {
		t.Fatalf("session agent = %q, want plan", sess.Agent)
	}
	if sess.Model == nil || sess.Model.ProviderID != "test" || sess.Model.ID != "m" {
		t.Fatalf("session model = %+v, want test/m", sess.Model)
	}
	waitUntil(t, 5*time.Second, func() bool {
		return hasUserText(i, sess, "Build the parser", "Parse the config format.", "All tests pass.")
	})
	if !hasUserText(i, sess, "Build the parser", "Parse the config format.", "All tests pass.") {
		msgs, _ := i.Mgr.LoadMessages(sess)
		t.Fatalf("task prompt missing description/criteria: %v", userTexts(msgs))
	}
}

// When a card's session goes idle the card moves to review and the review
// agent runs on its changes; the findings attach to the card.
func TestIdleCardMovesToReviewAndReviewAgentRuns(t *testing.T) {
	var reviewCalls int32
	h := func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		if strings.Contains(body, "title generator") {
			respondEmptyStop(w)
			return
		}
		if strings.Contains(body, "Review the changes of kanban card") {
			atomic.AddInt32(&reviewCalls, 1)
			textSSE("## Findings\n- the parser misses edge cases")(w, r)
			return
		}
		textSSE("Work done.")(w, r)
	}
	i, s := setupBoardTest(t, h, `"board": {"review_agent": "review"},`)
	projectID := store.ProjectID(i.Directory)

	card := linkCard(t, i, s, "Implement the parser", "inprogress")
	go func() { _, _ = i.Prompt(s, textInput("Implement the parser")) }()

	// the worker turn ends -> the card moves to review and the review
	// session runs on its changes
	waitUntil(t, 10*time.Second, func() bool {
		c, _ := cardByID(t, i, projectID, card.ID)
		return c.Column == "review" && c.Findings != ""
	})
	c, _ := cardByID(t, i, projectID, card.ID)
	if !strings.Contains(c.Findings, "misses edge cases") {
		t.Fatalf("findings not attached: %q", c.Findings)
	}
	if c.ReviewSessionID == "" {
		t.Fatal("review session not linked to the card")
	}
	if atomic.LoadInt32(&reviewCalls) == 0 {
		t.Fatal("review agent never ran")
	}
}

// Approving a review card moves it to done; rejecting sends it back to
// inprogress and prompts its session with the findings.
func TestApproveAndRejectReviewCard(t *testing.T) {
	i, s := setupAbortTest(t, textSSE("Fixing the findings."))
	projectID := store.ProjectID(i.Directory)

	card := linkCard(t, i, s, "Implement the parser", "inprogress")
	col := "review"
	if _, err := board.Update(projectID, card.ID, nil, &col, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := board.Edit(projectID, card.ID, func(c *board.Card) error {
		c.Findings = "- the parser misses edge cases"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// approve: the card is done
	if _, err := ApproveCard(i, projectID, card.ID); err != nil {
		t.Fatalf("approve failed: %v", err)
	}
	if c, _ := cardByID(t, i, projectID, card.ID); c.Column != "done" {
		t.Fatalf("approved card column = %s, want done", c.Column)
	}

	// reject: back to inprogress, findings as the next prompt
	if _, err := board.Update(projectID, card.ID, nil, &col, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := RejectCard(i, projectID, card.ID); err != nil {
		t.Fatalf("reject failed: %v", err)
	}
	if c, _ := cardByID(t, i, projectID, card.ID); c.Column != "inprogress" {
		t.Fatalf("rejected card column = %s, want inprogress", c.Column)
	}
	waitUntil(t, 5*time.Second, func() bool {
		return hasUserText(i, s, "review of this card found issues", "misses edge cases")
	})
	if !hasUserText(i, s, "review of this card found issues", "misses edge cases") {
		t.Fatal("rejected card's session was not prompted with the findings")
	}

	// resolving a missing card is an error
	if _, err := ApproveCard(i, projectID, "404"); err == nil {
		t.Fatal("approving a missing card must fail")
	}
}

// A non-retryable provider error marks the card blocked with the error —
// the work stops visibly instead of silently dying. (Retryable errors are
// retried forever and never block the card.)
func TestProviderErrorBlocksCard(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"provider exploded"}}`, http.StatusBadRequest)
	}
	i, _ := setupAbortTest(t, h)
	projectID := store.ProjectID(i.Directory)

	card, err := board.Add(projectID, "Doomed task", board.AddOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := startCard(i, projectID, card.ID); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, 15*time.Second, func() bool {
		c, _ := cardByID(t, i, projectID, card.ID)
		return c.Status == "blocked"
	})
	c, _ := cardByID(t, i, projectID, card.ID)
	if !strings.Contains(c.StatusText, "provider exploded") {
		t.Fatalf("blocked card status text = %q, want the provider error", c.StatusText)
	}
}

// A card's session that never gets a working provider leaves the card
// blocked too — not silently in-progress with a dead session.
func TestUnresolvableProviderBlocksCard(t *testing.T) {
	i, _ := setupAbortTest(t, textSSE("unused."))
	projectID := store.ProjectID(i.Directory)

	card, err := board.Add(projectID, "No provider", board.AddOpts{Model: "missing/provider"})
	if err != nil {
		t.Fatal(err)
	}
	sessID, err := startCard(i, projectID, card.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := i.Mgr.Find(sessID)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = i.Prompt(sess, PromptInput{Agent: "build", Parts: []json.RawMessage{mustJSON(map[string]any{"type": "text", "text": "go"})}})

	waitUntil(t, 5*time.Second, func() bool {
		c, _ := cardByID(t, i, projectID, card.ID)
		return c.Status == "blocked"
	})
}

func readBody(r *http.Request) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}
