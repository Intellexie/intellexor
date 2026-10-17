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
// cards, add one, move cards between columns, watch cards (getting pinged
// when they are marked done) and start cards in fresh sessions — e.g. move
// the card the session is working on to "done" once the task is finished.
// When a card is linked to the session (the card the agent was started from
// on the board) it can be addressed without passing its ID.
type BoardTool struct {
	// OnUpdate is called after every successful mutation so the server can
	// broadcast board.updated to connected web clients.
	OnUpdate func()
	// StartCard starts a new agent session on a card (the web UI's Start
	// button flow: link the card, move it to inprogress, prompt it with the
	// card's title) and returns the new session ID.
	StartCard func(projectID, cardID string) (string, error)
}

func (BoardTool) Name() string { return "board" }

func (BoardTool) Description() string {
	return `Work with the project kanban board (columns: open, inprogress, done).

## When to use
- When you finish working on the card linked to this session, move it to the done column (action "move", column "done") — the card is resolved automatically, no ID needed.
- Use action "list" to see all cards with their IDs, titles, columns and watchers.
- Use action "add" to create a follow-up card for work that is out of scope for the current task.
- Use action "watch" when your work depends on another card: you receive a message on behalf of the user as soon as that card is marked done.
- Use action "link" to chain a follow-up card to this one: when this card is moved to done, the target card starts automatically in a fresh session (only cards still in the open column can be targeted).
- Use action "start" to begin work on a card in a fresh session, exactly like the user clicking Start on the board.

## Actions
- "list": returns every card (id, title, column, link, watchers).
- "move": moves a card to another column. Pass "card" to move a specific card by ID; omit it to move the card linked to the current session.
- "add": creates a card with "title" in the open column (cards can only be created there; move it afterwards to start work).
- "watch" / "unwatch": start/stop watching a card — pass "card" to watch a specific card by ID, or omit it to use the card linked to the current session. Watchers are messaged when the card is marked done. Watching the card linked to this session is rejected: you are already working on it.
- "link" / "unlink": point "card" (or the session's card) at "target" so the target starts automatically when the card is done; "unlink" removes the link.
- "start": start a new agent session on card "card"; the card's title becomes the new session's task.`
}

func (BoardTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []any{"list", "move", "add", "watch", "unwatch", "link", "unlink", "start"},
				"description": "The board operation to perform",
			},
			"card": map[string]any{
				"type":        "string",
				"description": "Card ID to move, watch/unwatch, link/unlink or start. Omit for move/watch/unwatch/link/unlink to use the card linked to the current session.",
			},
			"column": map[string]any{
				"type":        "string",
				"enum":        []any{"open", "inprogress", "done"},
				"description": "Target column for move (add always creates in open)",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Card title (action add)",
			},
			"target": map[string]any{
				"type":        "string",
				"description": "Card ID to link to (action link): it starts automatically when the source card is done",
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
		Target string `json:"target"`
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
		// cards are only ever created in open — work starts by moving a
		// card to inprogress, never by creating it there
		if p.Column != "" && p.Column != "open" {
			return Result{}, errors.New("cards can only be added to the open column (move the card afterwards to start work on it)")
		}
		card, err := board.Add(projectID, p.Title, nil)
		if err != nil {
			return Result{}, err
		}
		t.notify()
		return Result{
			Title:    "Card added: " + card.Title,
			Metadata: map[string]any{"card": card},
			Output:   fmt.Sprintf("Card %s (%q) created in column %q.", card.ID, card.Title, card.Column),
		}, nil

	case "watch", "unwatch":
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
		// an agent cannot watch the card it is working on: the session is
		// linked to it and will be the one marking it done
		if p.Action == "watch" {
			linked, ok, err := board.ForSession(projectID, c.SessionID)
			if err != nil {
				return Result{}, err
			}
			if ok && linked.ID == id {
				return Result{}, fmt.Errorf("card %s is linked to this session — you are already working on it, so it cannot be watched", id)
			}
		}
		card, err := board.Watch(projectID, id, c.SessionID, p.Action == "watch")
		if err == board.ErrNotFound {
			return Result{}, fmt.Errorf("card not found: %s (use action list to see card IDs)", id)
		}
		if err != nil {
			return Result{}, err
		}
		t.notify()
		if p.Action == "watch" {
			return Result{
				Title:    "Watching " + card.Title,
				Metadata: map[string]any{"card": card},
				Output:   fmt.Sprintf("Now watching card %s (%q). When it is moved to done, you will receive a message on behalf of the user.", card.ID, card.Title),
			}, nil
		}
		return Result{
			Title:    "Unwatched " + card.Title,
			Metadata: map[string]any{"card": card},
			Output:   fmt.Sprintf("Stopped watching card %s (%q).", card.ID, card.Title),
		}, nil

	case "link", "unlink":
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
		target := p.Target
		if p.Action == "unlink" {
			target = ""
		} else if target == "" {
			return Result{}, errors.New("link requires a target card ID (see action list)")
		}
		card, err := board.SetLink(projectID, id, target)
		if err == board.ErrNotFound {
			return Result{}, fmt.Errorf("card not found: %s (use action list to see card IDs)", id)
		}
		if err == board.ErrBadLink {
			return Result{}, fmt.Errorf("card %s cannot be linked to %q: the target must be another card that is still waiting in the open column", id, p.Target)
		}
		if err != nil {
			return Result{}, err
		}
		t.notify()
		if target == "" {
			return Result{
				Title:    "Unlinked " + card.Title,
				Metadata: map[string]any{"card": card},
				Output:   fmt.Sprintf("Card %s (%q) no longer triggers another card when done.", card.ID, card.Title),
			}, nil
		}
		return Result{
			Title:    "Linked to card " + target,
			Metadata: map[string]any{"card": card},
			Output:   fmt.Sprintf("Card %s (%q) is now linked to card %s: when it is moved to done, that card starts automatically in a fresh session.", card.ID, card.Title, target),
		}, nil

	case "start":
		if p.Card == "" {
			return Result{}, errors.New("start requires a card ID (see action list)")
		}
		if t.StartCard == nil {
			return Result{}, errors.New("starting cards is not available in this context")
		}
		cards, err := board.Get(projectID)
		if err != nil {
			return Result{}, err
		}
		title, found := "", false
		for _, c := range cards {
			if c.ID == p.Card {
				title, found = c.Title, true
				break
			}
		}
		if !found {
			return Result{}, fmt.Errorf("card not found: %s (use action list to see card IDs)", p.Card)
		}
		sessID, err := t.StartCard(projectID, p.Card)
		if err != nil {
			return Result{}, err
		}
		return Result{
			Title:    "Started: " + title,
			Metadata: map[string]any{"sessionID": sessID, "cardID": p.Card},
			Output:   fmt.Sprintf("Started a new agent session (%s) on card %s (%q): the card is linked to it, moved to inprogress, and its title is the session's task.", sessID, p.Card, title),
		}, nil

	default:
		return Result{}, fmt.Errorf("unknown action %q (valid: list, move, add, watch, unwatch, link, unlink, start)", p.Action)
	}
}

func (t BoardTool) notify() {
	if t.OnUpdate != nil {
		t.OnUpdate()
	}
}
