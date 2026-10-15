package tool

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Intellexie/intellexor/internal/board"
	"github.com/Intellexie/intellexor/internal/store"
)

// BoardTool lets the agent work with the project kanban board: list its
// cards, add one, and move cards between columns — e.g. move the card the
// session is working on to "done" once the task is finished. When a card is
// linked to the session (the card the agent was started from on the board)
// it can be addressed without passing its ID.
type BoardTool struct {
	// OnUpdate is called after every successful mutation so the server can
	// broadcast board.updated to connected web clients.
	OnUpdate func()
}

func (BoardTool) Name() string { return "board" }

func (BoardTool) Description() string {
	return `Work with the project kanban board (columns: open, inprogress, done).

## When to use
- When you finish working on the card linked to this session, move it to the done column (action "move", column "done") — the card is resolved automatically, no ID needed.
- Use action "list" to see all cards with their IDs, titles and columns.
- Use action "add" to create a follow-up card for work that is out of scope for the current task.

## Actions
- "list": returns every card (id, title, column).
- "move": moves a card to another column. Pass "card" to move a specific card by ID; omit it to move the card linked to the current session.
- "add": creates a card with "title" in "column" (default open).`
}

func (BoardTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []any{"list", "move", "add"},
				"description": "The board operation to perform",
			},
			"card": map[string]any{
				"type":        "string",
				"description": "Card ID to move. Omit to use the card linked to the current session.",
			},
			"column": map[string]any{
				"type":        "string",
				"enum":        []any{"open", "inprogress", "done"},
				"description": "Target column for move, or starting column for add",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Card title (action add)",
			},
		},
		"required": []any{"action"},
	}
}

func (t BoardTool) Run(c *Ctx, args json.RawMessage) (Result, error) {
	var p struct {
		Action string `json:"action"`
		Card   string `json:"card"`
		Column string `json:"column"`
		Title  string `json:"title"`
	}
	_ = json.Unmarshal(args, &p)
	if c.Ask != nil {
		if err := c.Ask(AskInput{Permission: "board", Patterns: []string{"*"}, Always: []string{"*"}, Metadata: map[string]any{}}); err != nil {
			return Result{}, err
		}
	}
	projectID := store.ProjectID(c.Directory)

	switch p.Action {
	case "list":
		cards, err := board.Get(projectID)
		if err != nil {
			return Result{}, err
		}
		out, _ := json.MarshalIndent(cards, "", "  ")
		return Result{
			Title:    fmt.Sprintf("%d cards", len(cards)),
			Metadata: map[string]any{"cards": cards},
			Output:   string(out),
		}, nil

	case "move":
		if p.Column == "" {
			return Result{}, errors.New("move requires a target column (open, inprogress or done)")
		}
		id := p.Card
		if id == "" {
			card, ok, err := board.ForSession(projectID, c.SessionID)
			if err != nil {
				return Result{}, err
			}
			if !ok {
				return Result{}, errors.New("no card is linked to this session; pass the card ID (see action list)")
			}
			id = card.ID
		}
		card, err := board.Update(projectID, id, nil, &p.Column, nil, nil)
		if err == board.ErrNotFound {
			return Result{}, fmt.Errorf("card not found: %s (use action list to see card IDs)", id)
		}
		if err == board.ErrBadColumn {
			return Result{}, fmt.Errorf("unknown column %q (valid: open, inprogress, done)", p.Column)
		}
		if err != nil {
			return Result{}, err
		}
		t.notify()
		return Result{
			Title:    fmt.Sprintf("%s → %s", card.Title, card.Column),
			Metadata: map[string]any{"card": card},
			Output:   fmt.Sprintf("Card %s (%q) moved to column %q.", card.ID, card.Title, card.Column),
		}, nil

	case "add":
		if strings.TrimSpace(p.Title) == "" {
			return Result{}, errors.New("add requires a title")
		}
		column := p.Column
		if column == "" {
			column = "open"
		}
		card, err := board.Add(projectID, p.Title, column)
		if err == board.ErrBadColumn {
			return Result{}, fmt.Errorf("unknown column %q (valid: open, inprogress, done)", column)
		}
		if err != nil {
			return Result{}, err
		}
		t.notify()
		return Result{
			Title:    "Card added: " + card.Title,
			Metadata: map[string]any{"card": card},
			Output:   fmt.Sprintf("Card %s (%q) created in column %q.", card.ID, card.Title, card.Column),
		}, nil

	default:
		return Result{}, fmt.Errorf("unknown action %q (valid: list, move, add)", p.Action)
	}
}

func (t BoardTool) notify() {
	if t.OnUpdate != nil {
		t.OnUpdate()
	}
}
