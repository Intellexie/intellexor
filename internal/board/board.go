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
	"sync"

	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

// Columns of the board, in display order.
var Columns = []string{"open", "inprogress", "done"}

// ErrBadColumn is returned when a card targets an unknown column.
var ErrBadColumn = errors.New("unknown column")

// ErrNotFound is returned when a card ID does not exist.
var ErrNotFound = errors.New("card not found")

// Card is a single kanban card. Order positions it within its column
// (ascending); it is renumbered densely on every mutation so clients can
// always sort by it. SessionID optionally links the card to the session
// working on it (set when the card is started from the board).
type Card struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Column    string `json:"column"`
	Order     int    `json:"order"`
	Created   int64  `json:"created"`
	SessionID string `json:"sessionID,omitempty"`
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
	return cards, nil
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

// Add creates a card at the end of the given column.
func Add(projectID, title, column string) (Card, error) {
	if !validColumn(column) {
		return Card{}, ErrBadColumn
	}
	mu.Lock()
	defer mu.Unlock()
	cards, err := load(projectID)
	if err != nil {
		return Card{}, err
	}
	card := Card{
		ID:      util.ID("card"),
		Title:   title,
		Column:  column,
		Order:   len(inColumn(cards, column)), // end of the column
		Created: util.Now(),
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
// affected columns are renumbered.
func Update(projectID, id string, title *string, column, before, sessionID *string) (Card, error) {
	if column != nil && !validColumn(*column) {
		return Card{}, ErrBadColumn
	}
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
	if title != nil {
		cards[idx].Title = *title
	}
	if sessionID != nil {
		cards[idx].SessionID = *sessionID
	}
	if column != nil {
		from := cards[idx].Column
		to := *column
		cards[idx].Column = to
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
		return Card{}, err
	}
	return cards[idx], nil
}

// Delete removes a card and renumbers its column.
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
