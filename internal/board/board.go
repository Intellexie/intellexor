// Package board stores the per-project kanban board shown as the first tab
// in the web UI. Cards live in a single JSON file under the data dir, keyed
// by project ID (the same keying sessions use), so the board survives
// restarts and is shared by every client connected to the same project.
package board

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

// Columns of the board, in display order. Review sits between in-progress
// and done: a card whose session went idle lands there for inspection
// (optionally by a review agent) before it counts as done.
var Columns = []string{"open", "inprogress", "review", "done"}

// ErrBadColumn is returned when a card targets an unknown column.
var ErrBadColumn = errors.New("unknown column")

// ErrNotFound is returned when a card ID does not exist.
var ErrNotFound = errors.New("card not found")

// ErrBadLink is returned when a card cannot be linked to the requested
// target: the target does not exist, is the card itself, or is not waiting
// in the open column (only open cards can be targeted).
var ErrBadLink = errors.New("card can only be linked to a card in the open column")

// ErrNotOpen is returned when a start schedule is set on a card that is not
// waiting in the open column — only cards that have not started yet can be
// scheduled (clearing a schedule is always allowed).
var ErrNotOpen = errors.New("only cards in the open column can be scheduled")

// ErrBadSession is returned when a session is linked to a card it cannot
// work on: the card is no longer waiting in the open column (already
// started, moved or done), or another session is already linked to it.
var ErrBadSession = errors.New("only cards waiting in the open column can be linked to a session")

// OnMovedToDone, when set, is called after a card is moved into the done
// column from another column — from any entry point (the agent's board tool,
// the HTTP API, ACP), since they all go through Update. It is called without
// the board lock held (handlers may call back into this package) and is used
// to notify watcher sessions.
var OnMovedToDone func(projectID string, card Card)

// OnStopped, when set, is called after a card is moved from inprogress back
// to open — the "stop the agent" flow. The card's session link is cleared
// (a card back in open is waiting and unowned again, so it can be started
// fresh) and the hook hands the released session ID to the app layer, which
// aborts the agent that was working on the card. Like OnMovedToDone it is
// called without the board lock held, from any entry point.
var OnStopped func(projectID, sessionID string)

// Attachment is a file (usually an image) attached to a card when it is
// created — the same shape the chat composer sends as a prompt part
// (type/mime/url/filename with a data URL), so attachments can be passed to
// the session prompt verbatim when the card is started.
type Attachment struct {
	Type     string `json:"type"`
	MIME     string `json:"mime"`
	URL      string `json:"url"`
	Filename string `json:"filename"`
}

// Card is a single kanban card. IDs are plain numbers ("1", "2", …) so
// users can reference them easily; they are assigned max+1 and never
// repurposed while any card exists (cards created before IDs became
// numeric are renumbered on load). Order positions it within its column
// (ascending); it is renumbered densely on every mutation so clients can
// always sort by it. SessionID optionally links the card to the session
// working on it (set when the card is started from the board). Watchers
// lists session IDs watching the card: when it is marked done, each of them
// is prompted with a message on behalf of the user. BlockedBy lists card
// IDs this one depends on: it can only start once every blocker is done
// (the legacy single Link field is migrated into it on load and kept
// accepted, deprecated, in the API). Attachments are files (usually
// images) attached at creation, passed to the session prompt on start.
// StartAt is an optional scheduled start time (unix milliseconds, like
// Created): when it passes, a background scheduler starts the card the same
// way the Start button does; it is cleared once the card has started.
// Isolate opts the card into git worktree isolation: when it is started,
// the session runs in a fresh worktree on its own branch instead of the
// shared directory. Branch and WorktreePath record the worktree the
// running session was isolated into (set on start, cleared on merge).
//
// Richer cards: Agent and Model pick the agent/model the card runs with
// (empty = the defaults); Description and Criteria are sent to the agent
// alongside the title and shown on the card. Findings holds the review
// agent's findings (set when the review session finishes); ReviewSessionID
// links the review session while it inspects the card.
//
// Live status: Status is the card's runtime state — "running", "waiting"
// (needs attention: pending question or permission), "error" or "blocked"
// (non-retryable provider failure; "" means idle). StatusText carries
// the error/waiting detail, StartedAt is the last start (unix ms, for
// elapsed time) and Cost/Tokens snapshot the linked session's usage.
type Card struct {
	ID           string       `json:"id"`
	Title        string       `json:"title"`
	Column       string       `json:"column"`
	Order        int          `json:"order"`
	Created      int64        `json:"created"`
	SessionID    string       `json:"sessionID,omitempty"`
	Watchers     []string     `json:"watchers,omitempty"`
	Link         string       `json:"link,omitempty"` // deprecated: migrated into BlockedBy
	BlockedBy    []string     `json:"blockedBy,omitempty"`
	StartAt      int64        `json:"startAt,omitempty"`
	Attachments  []Attachment `json:"attachments,omitempty"`
	Isolate      bool         `json:"isolate,omitempty"`
	Branch       string       `json:"branch,omitempty"`
	WorktreePath string       `json:"worktreePath,omitempty"`
	Agent        string       `json:"agent,omitempty"`
	Model        string       `json:"model,omitempty"`
	Description  string       `json:"description,omitempty"`
	Criteria     string       `json:"criteria,omitempty"`
	Findings     string       `json:"findings,omitempty"`

	ReviewSessionID string  `json:"reviewSessionID,omitempty"`
	Status          string  `json:"status,omitempty"`
	StatusText      string  `json:"statusText,omitempty"`
	StartedAt       int64   `json:"startedAt,omitempty"`
	Cost            float64 `json:"cost,omitempty"`
	Tokens          float64 `json:"tokens,omitempty"`
}

var mu sync.Mutex

func file(projectID string) string {
	return filepath.Join(store.DataDir(), "storage", "board", projectID+".json")
}

func load(projectID string) ([]Card, error) {
	var cards []Card
	b, err := os.ReadFile(file(projectID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &cards); err != nil {
		return nil, err
	}
	// cards created before IDs became plain numbers keep working, but are
	// renumbered (and the renumber persisted) the first time they are loaded
	changed := normalizeIDs(cards)
	// the deprecated single Link migrates into BlockedBy — on the *target*:
	// "when A is done, B starts" is the dependency "B is blockedBy A". The
	// Link field is kept as-is so old clients reading the JSON keep working.
	byID := map[string]int{}
	for i := range cards {
		byID[cards[i].ID] = i
	}
	for _, c := range cards {
		if c.Link == "" {
			continue
		}
		ti, ok := byID[c.Link]
		if !ok || containsID(cards[ti].BlockedBy, c.ID) {
			continue
		}
		cards[ti].BlockedBy = append(cards[ti].BlockedBy, c.ID)
		changed = true
	}
	if changed {
		if err := save(projectID, cards); err != nil {
			return nil, err
		}
	}
	return cards, nil
}

func containsID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// normalizeIDs assigns a numeric ID to every card that still carries a
// legacy non-numeric one, filling the smallest free numbers. It reports
// whether anything was renumbered.
func normalizeIDs(cards []Card) bool {
	used := map[int]bool{}
	for _, c := range cards {
		if n, err := strconv.Atoi(c.ID); err == nil && n > 0 {
			used[n] = true
		}
	}
	next := 1
	changed := false
	for i := range cards {
		if n, err := strconv.Atoi(cards[i].ID); err == nil && n > 0 {
			continue
		}
		for used[next] {
			next++
		}
		cards[i].ID = strconv.Itoa(next)
		used[next] = true
		changed = true
	}
	return changed
}

// nextID returns the next free numeric card ID: one past the largest number
// in use, so existing cards keep their ID and new ones count up from 1.
func nextID(cards []Card) string {
	max := 0
	for _, c := range cards {
		if n, err := strconv.Atoi(c.ID); err == nil && n > max {
			max = n
		}
	}
	return strconv.Itoa(max + 1)
}

func save(projectID string, cards []Card) error {
	path := file(projectID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(cards)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// inColumn returns the cards of one column sorted by order.
func inColumn(cards []Card, column string) []Card {
	var out []Card
	for _, c := range cards {
		if c.Column == column {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

// renumber assigns dense 0..n-1 orders within every column, preserving the
// relative order cards already have.
func renumber(cards []Card) {
	for _, col := range Columns {
		var idxs []int
		for i := range cards {
			if cards[i].Column == col {
				idxs = append(idxs, i)
			}
		}
		sort.SliceStable(idxs, func(a, b int) bool { return cards[idxs[a]].Order < cards[idxs[b]].Order })
		for n, i := range idxs {
			cards[i].Order = n
		}
	}
}

// Get returns every card of the project, ordered for display.
func Get(projectID string) ([]Card, error) {
	mu.Lock()
	defer mu.Unlock()
	cards, err := load(projectID)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(cards, func(i, j int) bool {
		if cards[i].Column != cards[j].Column {
			return columnRank(cards[i].Column) < columnRank(cards[j].Column)
		}
		return cards[i].Order < cards[j].Order
	})
	if cards == nil {
		cards = []Card{}
	}
	return cards, nil
}

func columnRank(column string) int {
	for i, c := range Columns {
		if c == column {
			return i
		}
	}
	return len(Columns)
}

// ForSession returns the card linked to the given session (the card the
// session was started from on the board), if any.
func ForSession(projectID, sessionID string) (Card, bool, error) {
	cards, err := Get(projectID)
	if err != nil {
		return Card{}, false, err
	}
	for _, c := range cards {
		if c.SessionID == sessionID {
			return c, true, nil
		}
	}
	return Card{}, false, nil
}

// AddOpts carries the optional fields of a new card: attachments and the
// worktree-isolation flag (as before), plus the richer card fields — the
// agent/model the card runs with, its description and acceptance criteria,
// and the cards it is blocked by.
type AddOpts struct {
	Attachments []Attachment
	Isolate     bool
	Agent       string
	Model       string
	Description string
	Criteria    string
	BlockedBy   []string
}

// Add creates a card at the end of the open column. Cards can only be
// created in open — work begins by moving a card to inprogress (which links
// the session working on it), never by creating it there. Attachments, when
// given, ride along and are prompted to the session when the card starts.
// Isolate opts the card into git worktree isolation when it is started.
func Add(projectID, title string, o AddOpts) (Card, error) {
	mu.Lock()
	defer mu.Unlock()
	cards, err := load(projectID)
	if err != nil {
		return Card{}, err
	}
	card := Card{
		ID:          nextID(cards),
		Title:       title,
		Column:      "open",
		Order:       len(inColumn(cards, "open")), // end of the column
		Created:     util.Now(),
		Attachments: o.Attachments,
		Isolate:     o.Isolate,
		Agent:       o.Agent,
		Model:       o.Model,
		Description: o.Description,
		Criteria:    o.Criteria,
		BlockedBy:   o.BlockedBy,
	}
	cards = append(cards, card)
	renumber(cards)
	if err := save(projectID, cards); err != nil {
		return Card{}, err
	}
	for _, c := range cards {
		if c.ID == card.ID {
			return c, nil
		}
	}
	return card, nil
}

// Update edits a card's title, links it to a session, and/or moves it.
// When column is set the card is placed before the card with ID before
// (end of the column when before is empty or missing from it). Both
// affected columns are renumbered. Linking a session is only allowed while
// the card is still waiting in open and unowned — the same precondition
// startCard enforces. Moving a card out of open clears its
// start schedule (a schedule only makes sense for a waiting card — starting
// it, by hand or by the scheduler, consumes it). Moving a card from
// inprogress back to open stops it: the session link is cleared (the card
// is waiting and unowned again) and OnStopped fires so the released
// session's agent is aborted. Moving a card into done fires OnMovedToDone
// (both after the lock is released).
func Update(projectID, id string, title *string, column, before, sessionID *string) (Card, error) {
	card, movedDone, stoppedSession, err := updateLocked(projectID, id, title, column, before, sessionID)
	if err == nil && stoppedSession != "" && OnStopped != nil {
		OnStopped(projectID, stoppedSession)
	}
	if err == nil && movedDone && OnMovedToDone != nil {
		OnMovedToDone(projectID, card)
	}
	return card, err
}

func updateLocked(projectID, id string, title *string, column, before, sessionID *string) (Card, bool, string, error) {
	if column != nil && !validColumn(*column) {
		return Card{}, false, "", ErrBadColumn
	}
	mu.Lock()
	defer mu.Unlock()
	cards, err := load(projectID)
	if err != nil {
		return Card{}, false, "", err
	}
	idx := -1
	for i := range cards {
		if cards[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Card{}, false, "", ErrNotFound
	}
	// linking a session is only meaningful for a card that is still waiting
	// in open and not owned by another session — the same precondition
	// startCard enforces, so the web UI's link dialog cannot hijack a card
	// that is in progress or done (clearing a sessionID is always allowed)
	if sessionID != nil && *sessionID != "" {
		if cards[idx].Column != "open" || (cards[idx].SessionID != "" && cards[idx].SessionID != *sessionID) {
			return Card{}, false, "", ErrBadSession
		}
	}
	if title != nil {
		cards[idx].Title = *title
	}
	if sessionID != nil {
		cards[idx].SessionID = *sessionID
	}
	movedDone := false
	stoppedSession := ""
	if column != nil {
		from := cards[idx].Column
		to := *column
		movedDone = to == "done" && from != "done"
		cards[idx].Column = to
		// leaving open consumes the start schedule: the card is no longer
		// waiting, whether it was started by hand, by the scheduler, or by
		// a done-link trigger
		if from == "open" && to != "open" {
			cards[idx].StartAt = 0
		}
		// returning to open stops the card: drop the stale session link so
		// the card is waiting and unowned again (a fresh Start can link a
		// new session), and let the caller abort the released agent — from
		// inprogress (the stop flow) or from review (dragged back)
		if (from == "inprogress" || from == "review") && to == "open" && cards[idx].SessionID != "" {
			stoppedSession = cards[idx].SessionID
			cards[idx].SessionID = ""
		}
		// target column in display order, without the moved card
		col := inColumn(cards, to)
		for i := range col {
			if col[i].ID == id {
				col = append(col[:i], col[i+1:]...)
				break
			}
		}
		// insertion position: before the `before` card, else at the end
		pos := len(col)
		if before != nil && *before != "" && *before != id {
			for i, c := range col {
				if c.ID == *before {
					pos = i
					break
				}
			}
		}
		col = append(col, Card{})
		copy(col[pos+1:], col[pos:])
		col[pos] = cards[idx]
		for i, c := range col {
			for j := range cards {
				if cards[j].ID == c.ID {
					cards[j].Order = i
					break
				}
			}
		}
		if from != to {
			renumber(cards) // densify the column the card left
		}
	}
	if err := save(projectID, cards); err != nil {
		return Card{}, false, "", err
	}
	return cards[idx], movedDone, stoppedSession, nil
}

// SetIsolate toggles the card's worktree isolation flag. Only cards still
// waiting in open can change it — a started card already has its
// environment fixed.
func SetIsolate(projectID, id string, isolate bool) (Card, error) {
	mu.Lock()
	defer mu.Unlock()
	cards, err := load(projectID)
	if err != nil {
		return Card{}, err
	}
	idx := -1
	for i := range cards {
		if cards[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Card{}, ErrNotFound
	}
	if cards[idx].Column != "open" {
		return Card{}, ErrNotOpen
	}
	cards[idx].Isolate = isolate
	if err := save(projectID, cards); err != nil {
		return Card{}, err
	}
	return cards[idx], nil
}

// SetWorktree records (or clears, with empty branch) the worktree the
// card's session was isolated into: the branch name and the worktree path,
// shown on the card and used by the merge flow.
func SetWorktree(projectID, id, branch, path string) (Card, error) {
	mu.Lock()
	defer mu.Unlock()
	cards, err := load(projectID)
	if err != nil {
		return Card{}, err
	}
	idx := -1
	for i := range cards {
		if cards[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Card{}, ErrNotFound
	}
	cards[idx].Branch = branch
	cards[idx].WorktreePath = path
	if err := save(projectID, cards); err != nil {
		return Card{}, err
	}
	return cards[idx], nil
}

// Edit applies fn to the card under the board lock and persists the change.
// It is the generic mutation used for the richer card fields (description,
// criteria, agent, model, blockers) that the older dedicated setters do not
// cover. ErrNotFound when the card does not exist.
func Edit(projectID, id string, fn func(c *Card) error) (Card, error) {
	mu.Lock()
	defer mu.Unlock()
	cards, err := load(projectID)
	if err != nil {
		return Card{}, err
	}
	idx := -1
	for i := range cards {
		if cards[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Card{}, ErrNotFound
	}
	if err := fn(&cards[idx]); err != nil {
		return Card{}, err
	}
	if err := save(projectID, cards); err != nil {
		return Card{}, err
	}
	return cards[idx], nil
}

// SetBlockedBy replaces the card's dependency list. Every blocker must be
// an existing card other than the card itself; duplicates are dropped.
// An empty list clears the dependencies.
func SetBlockedBy(projectID, id string, blockers []string) (Card, error) {
	seen := map[string]bool{}
	var clean []string
	for _, b := range blockers {
		if b == id || seen[b] {
			continue
		}
		seen[b] = true
		clean = append(clean, b)
	}
	card, err := Edit(projectID, id, func(c *Card) error {
		for _, b := range clean {
			if _, err := findCard(projectID, b); err != nil {
				return err
			}
		}
		c.BlockedBy = clean
		return nil
	})
	if err == ErrNotFound {
		return Card{}, ErrBadLink // a blocker that does not exist
	}
	return card, err
}

// findCard returns the card with the given ID (caller holds the lock or
// accepts the race-free file read).
func findCard(projectID, id string) (Card, error) {
	cards, err := load(projectID)
	if err != nil {
		return Card{}, err
	}
	for _, c := range cards {
		if c.ID == id {
			return c, nil
		}
	}
	return Card{}, ErrNotFound
}

// Runtime is a card's live status snapshot, updated by the app layer as the
// linked session works. Status is "" (idle), "running", "waiting" (needs
// attention: pending question or permission), "error" or "blocked"
// (non-retryable provider failure); StatusText carries the detail.
type Runtime struct {
	Status     string
	StatusText string
	StartedAt  int64
	Cost       float64
	Tokens     float64
}

// SetRuntime stores the card's live status snapshot and reports whether
// anything changed (so callers can skip the board.updated broadcast when
// nothing did).
func SetRuntime(projectID, id string, rt Runtime) (Card, bool, error) {
	card, err := Edit(projectID, id, func(c *Card) error {
		if c.Status == rt.Status && c.StatusText == rt.StatusText && c.StartedAt == rt.StartedAt &&
			c.Cost == rt.Cost && c.Tokens == rt.Tokens {
			return errUnchanged
		}
		c.Status = rt.Status
		c.StatusText = rt.StatusText
		c.StartedAt = rt.StartedAt
		c.Cost = rt.Cost
		c.Tokens = rt.Tokens
		return nil
	})
	if err == errUnchanged {
		return card, false, nil
	}
	return card, err == nil, err
}

// errUnchanged is the sentinel Edit fn returns when the runtime snapshot is
// already stored — not an error, a "nothing to do" signal.
var errUnchanged = errors.New("unchanged")

// Watch adds the session to (watch=true) or removes it from (watch=false)
// the card's watchers.
func Watch(projectID, cardID, sessionID string, watch bool) (Card, error) {
	mu.Lock()
	defer mu.Unlock()
	cards, err := load(projectID)
	if err != nil {
		return Card{}, err
	}
	idx := -1
	for i := range cards {
		if cards[i].ID == cardID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Card{}, ErrNotFound
	}
	has := false
	for _, w := range cards[idx].Watchers {
		if w == sessionID {
			has = true
			break
		}
	}
	if watch == has {
		return cards[idx], nil // already in the wanted state
	}
	if watch {
		cards[idx].Watchers = append(cards[idx].Watchers, sessionID)
	} else {
		out := cards[idx].Watchers[:0]
		for _, w := range cards[idx].Watchers {
			if w != sessionID {
				out = append(out, w)
			}
		}
		cards[idx].Watchers = out
	}
	if err := save(projectID, cards); err != nil {
		return Card{}, err
	}
	return cards[idx], nil
}

// SetLink points card at target: when the card is moved to done, the target
// is started automatically in a fresh session (the Start-button flow). An
// empty target clears the link. The target must be an existing card other
// than the card itself, and it must still be waiting in the open column —
// only open cards can be targeted.
func SetLink(projectID, id, target string) (Card, error) {
	mu.Lock()
	defer mu.Unlock()
	cards, err := load(projectID)
	if err != nil {
		return Card{}, err
	}
	idx := -1
	for i := range cards {
		if cards[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Card{}, ErrNotFound
	}
	if target != "" {
		if target == id {
			return Card{}, ErrBadLink
		}
		tIdx := -1
		for i := range cards {
			if cards[i].ID == target {
				tIdx = i
				break
			}
		}
		if tIdx < 0 || cards[tIdx].Column != "open" {
			return Card{}, ErrBadLink
		}
	}
	oldTarget := cards[idx].Link
	cards[idx].Link = target
	// keep the dependency list in sync: "when this card is done, the target
	// starts" is the dependency "target is blockedBy this card" — stored on
	// the target, where the dependency flow reads it
	ti := -1
	for i := range cards {
		if cards[i].ID == target {
			ti = i
			break
		}
	}
	if ti >= 0 && target != "" {
		if !containsID(cards[ti].BlockedBy, id) {
			cards[ti].BlockedBy = append(cards[ti].BlockedBy, id)
		}
	}
	// unlink: drop the dependency the old target got from this card
	if oldTarget != "" && oldTarget != target {
		for i := range cards {
			if cards[i].ID != oldTarget {
				continue
			}
			var out []string
			for _, b := range cards[i].BlockedBy {
				if b != id {
					out = append(out, b)
				}
			}
			cards[i].BlockedBy = out
		}
	}
	if err := save(projectID, cards); err != nil {
		return Card{}, err
	}
	return cards[idx], nil
}

// SetSchedule sets the card's scheduled start time (unix milliseconds): a
// background scheduler starts the card when the time passes, exactly like
// the Start button. startAt == 0 clears the schedule (always allowed, so a
// start can consume it on any column). Setting a schedule is only allowed
// while the card is still waiting in open — a card that already started
// cannot be scheduled.
func SetSchedule(projectID, id string, startAt int64) (Card, error) {
	mu.Lock()
	defer mu.Unlock()
	cards, err := load(projectID)
	if err != nil {
		return Card{}, err
	}
	idx := -1
	for i := range cards {
		if cards[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Card{}, ErrNotFound
	}
	if startAt != 0 && (cards[idx].Column != "open" || cards[idx].SessionID != "") {
		return Card{}, ErrNotOpen
	}
	cards[idx].StartAt = startAt
	if err := save(projectID, cards); err != nil {
		return Card{}, err
	}
	return cards[idx], nil
}

// Delete removes a card, clears links pointing at it and renumbers its
// column.
func Delete(projectID, id string) error {
	mu.Lock()
	defer mu.Unlock()
	cards, err := load(projectID)
	if err != nil {
		return err
	}
	kept := cards[:0]
	found := false
	for _, c := range cards {
		if c.ID == id {
			found = true
			continue
		}
		if c.Link == id {
			c.Link = "" // the trigger target is gone
		}
		if containsID(c.BlockedBy, id) {
			var out []string
			for _, b := range c.BlockedBy {
				if b != id {
					out = append(out, b)
				}
			}
			c.BlockedBy = out // the blocker is gone: the card unblocks
		}
		kept = append(kept, c)
	}
	if !found {
		return ErrNotFound
	}
	renumber(kept)
	return save(projectID, kept)
}

func validColumn(c string) bool {
	for _, v := range Columns {
		if c == v {
			return true
		}
	}
	return false
}
