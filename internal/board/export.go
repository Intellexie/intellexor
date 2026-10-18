package board

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/Intellexie/intellexor/internal/util"
)

// BoardExportFormat identifies the payload shape; Import rejects files with
// a different format value.
const BoardExportFormat = "intellexor.board"

// Import modes: "replace" makes the board exactly the imported one, while
// "merge" appends the imported cards to the current board.
const (
	ImportReplace = "replace"
	ImportMerge   = "merge"
)

// ExportPayload is the portable JSON document produced by GET /board/export
// and consumed by POST /board/import. It carries the whole board: every
// card with its column, order, done-links, schedule and attachments.
type ExportPayload struct {
	Format     string `json:"format"`
	Version    int    `json:"version"`
	ExportedAt int64  `json:"exportedAt"`
	Cards      []Card `json:"cards"`
}

// Export builds the payload for a project's board: every card as it is,
// in display order.
func Export(projectID string) (*ExportPayload, error) {
	cards, err := Get(projectID)
	if err != nil {
		return nil, err
	}
	return &ExportPayload{
		Format:     BoardExportFormat,
		Version:    1,
		ExportedAt: util.Now(),
		Cards:      cards,
	}, nil
}

// Import installs an exported board into the project and returns how many
// cards were written. Mode "replace" (the default) discards the current
// board and installs the imported cards, keeping their ids. Mode "merge"
// keeps the current board and appends the imported cards at the end of
// their columns with fresh ids, remapping card-to-card links to the new
// ids. Session references never survive an import: the session working on
// a card and its watchers point at sessions of the exporting project, so
// they are cleared — a card that kept a dangling session link would look
// started while having nothing behind it.
func Import(projectID string, p *ExportPayload, mode string) (int, error) {
	if p == nil {
		return 0, errors.New("invalid export: missing payload")
	}
	if p.Format != "" && p.Format != BoardExportFormat {
		return 0, fmt.Errorf("invalid export format: %s", p.Format)
	}
	if mode == "" {
		mode = ImportReplace
	}
	if mode != ImportReplace && mode != ImportMerge {
		return 0, fmt.Errorf("invalid import mode: %s", mode)
	}

	mu.Lock()
	defer mu.Unlock()
	cards, err := load(projectID)
	if err != nil {
		return 0, err
	}
	if mode != ImportMerge {
		cards = nil // replace: the board becomes exactly the import
	}
	// exported id -> installed id (first occurrence wins), used to remap
	// done-links once every card is placed
	idMap := map[string]string{}
	n := 0
	for _, src := range p.Cards {
		c := src // sanitize the copy, not the caller's card
		if !validColumn(c.Column) {
			c.Column = "open" // tolerate hand-edited files
		}
		c.SessionID = ""
		c.Watchers = nil
		if c.Column != "open" {
			c.StartAt = 0 // schedules only make sense for waiting cards
		}
		switch {
		case mode == ImportMerge:
			c.ID = nextID(cards)
			c.Order = len(inColumn(cards, c.Column)) // end of the column
		case !validID(c.ID) || idMap[c.ID] != "":
			c.ID = nextID(cards) // invalid or duplicate exported id
		}
		if _, seen := idMap[src.ID]; !seen {
			idMap[src.ID] = c.ID
		}
		cards = append(cards, c)
		n++
	}
	for i := range cards {
		if cards[i].Link == "" {
			continue
		}
		if mapped, ok := idMap[cards[i].Link]; ok {
			cards[i].Link = mapped
		} else {
			cards[i].Link = "" // the target did not make it into the board
		}
	}
	renumber(cards)
	if cards == nil {
		cards = []Card{}
	}
	if err := save(projectID, cards); err != nil {
		return 0, err
	}
	return n, nil
}

// validID reports whether s is a positive number — the only form of card
// id the board mints (legacy non-numeric ids are renumbered on load).
func validID(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0
}
