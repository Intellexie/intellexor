package tool

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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
	// MergeCard merges a card's worktree branch into the repository and
	// cleans the worktree up (isolation flow).
	MergeCard func(projectID, cardID string) (board.Card, error)
	// ResolveReview approves (approve=true) or rejects a card waiting in
	// review: approve moves it to done (merging its worktree when
	// isolated), reject returns it to inprogress and prompts its session
	// with the findings.
	ResolveReview func(projectID, cardID string, approve bool) (board.Card, error)
	// MovedToReview runs the review stage after a card was explicitly moved
	// into the review column (the review agent inspects the card's changes).
	MovedToReview func(projectID, cardID string)
}

// errNotWaiting is returned when an edit targets a card that has already
// left its waiting column (started, in review or done): a running card is
// owned by its session and a finished one is history.
var errNotWaiting = errors.New("only cards waiting in the planning or open column can be edited")

func (BoardTool) Name() string { return "board" }

func (BoardTool) Description() string {
	return `Work with the project kanban board (columns: planning, open, inprogress, review, done).

## When to use
- When you finish working on the card linked to this session, move it to the done column (action "move", column "done") — the card is resolved automatically, no ID needed.
- Use action "list" to see all cards with their IDs, titles, columns, status and watchers.
- Use action "add" to create a follow-up card for work that is out of scope for the current task.
- Use action "edit" to refine a card that is still waiting in planning or open (title, description, criteria, agent, model, isolate, blockedBy) instead of recreating it.
- Use action "watch" when your work depends on another card: you receive a message on behalf of the user as soon as that card is marked done.
- Use action "link" to chain a follow-up card to this one: when this card is moved to done, the target card starts automatically in a fresh session (only cards still waiting in planning or open can be targeted). "blockedBy" (on add) declares dependencies the other way round: a card with blockers only starts once all of them are done.
- Use action "start" to begin work on a card in a fresh session, exactly like the user clicking Start on the board.
- Use action "approve"/"reject" on a card in the review column: approve moves it to done, reject sends it back to inprogress with the review findings as the next prompt.

## Actions
- "list": returns every card (id, title, column, status, blockedBy, watchers).
- "move": moves a card to another column. Pass "card" to move a specific card by ID; omit it to move the card linked to the current session.
- "add": creates a card with "title" in a waiting column — "column" "planning" or "open" (default; move it afterwards to start work). Optional: "description", "criteria" (acceptance criteria), "agent", "model" ("provider/model"), "blockedBy" (array of card IDs), "isolate".
- "edit": changes fields on a card that is still waiting in planning or open: "title", "description", "criteria", "agent", "model", "isolate", "blockedBy" (empty array clears the dependencies). Only pass the fields you want to change; started or finished cards cannot be edited.
- "watch" / "unwatch": start/stop watching a card — pass "card" to watch a specific card by ID, or omit it to use the card linked to the current session. Watchers are messaged when the card is marked done. Watching the card linked to this session is rejected: you are already working on it.
- "link" / "unlink": point "card" (or the session's card) at "target" so the target starts automatically when the card is done; "unlink" removes the link.
- "schedule": set a timed start on a card — a background scheduler starts it (exactly like "start") when the time passes. Only cards still waiting in planning or open can be scheduled; startAt 0 clears the schedule.
- "start": start a new agent session on card "card"; the card's title becomes the new session's task. The orchestrator agent automatically watches every card it started.
- "approve" / "reject": resolve a card waiting in review — approve moves it to done (merging its worktree when isolated), reject returns it to inprogress and prompts its session with the findings.
- "merge": merge the card's worktree branch into the repository and remove the worktree (cards started with worktree isolation only).`
}

func (BoardTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []any{"list", "move", "add", "edit", "watch", "unwatch", "link", "unlink", "schedule", "start", "approve", "reject", "merge"},
				"description": "The board operation to perform",
			},
			"card": map[string]any{
				"type":        "string",
				"description": "Card ID to move, edit, watch/unwatch, link/unlink, approve/reject or start. Omit for move/watch/unwatch/link/unlink to use the card linked to the current session.",
			},
			"column": map[string]any{
				"type":        "string",
				"enum":        []any{"planning", "open", "inprogress", "review", "done"},
				"description": "Target column for move; for add, the waiting column to create the card in (planning or open, default open)",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Card title (actions add and edit)",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "Optional card description (actions add and edit): context beyond the title, sent to the agent with the task",
			},
			"criteria": map[string]any{
				"type":        "string",
				"description": "Optional acceptance criteria (actions add and edit): what must hold for the card to be done",
			},
			"agent": map[string]any{
				"type":        "string",
				"description": "Agent the card runs with when started (actions add and edit); empty = the default build agent",
			},
			"model": map[string]any{
				"type":        "string",
				"description": "Model the card runs with, as \"provider/model\" (actions add and edit); empty = the default model",
			},
			"blockedBy": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Card IDs this card depends on (actions add and edit): it only starts once all of them are done; an empty array clears the dependencies",
			},
			"target": map[string]any{
				"type":        "string",
				"description": "Card ID to link to (action link): it starts automatically when the source card is done",
			},
			"startAt": map[string]any{
				"type":        "integer",
				"description": "Scheduled start time in unix milliseconds (action schedule); 0 clears the schedule",
			},
			"in": map[string]any{
				"type":        "string",
				"description": "Alternative to startAt (action schedule): a duration from now, e.g. \"30m\", \"2h\", \"1d\"",
			},
			"isolate": map[string]any{
				"type":        "boolean",
				"description": "actions add and edit: run the card's session in an isolated git worktree on its own branch when the card is started",
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
		// pointer fields so "edit" can tell "not passed" from "cleared"
		Title       *string  `json:"title"`
		Description *string  `json:"description"`
		Criteria    *string  `json:"criteria"`
		Agent       *string  `json:"agent"`
		Model       *string  `json:"model"`
		BlockedBy   []string `json:"blockedBy"`
		Target      string   `json:"target"`
		StartAt     int64    `json:"startAt"`
		In          string   `json:"in"`
		Isolate     *bool    `json:"isolate"`
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
			return Result{}, errors.New("move requires a target column (planning, open, inprogress, review or done)")
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
			return Result{}, fmt.Errorf("unknown column %q (valid: planning, open, inprogress, review, done)", p.Column)
		}
		if err != nil {
			return Result{}, err
		}
		t.notify()
		if p.Column == "review" && t.MovedToReview != nil {
			t.MovedToReview(projectID, id)
		}
		return Result{
			Title:    fmt.Sprintf("%s → %s", card.Title, card.Column),
			Metadata: map[string]any{"card": card},
			Output:   fmt.Sprintf("Card %s (%q) moved to column %q.", card.ID, card.Title, card.Column),
		}, nil

	case "add":
		if p.Title == nil || strings.TrimSpace(*p.Title) == "" {
			return Result{}, errors.New("add requires a title")
		}
		// cards are only ever created in a waiting column (planning or
		// open) — work starts by moving a card to inprogress, never by
		// creating it there
		if p.Column != "" && !board.IsWaiting(p.Column) {
			return Result{}, errors.New("cards can only be added to the planning or open column (move the card afterwards to start work on it)")
		}
		card, err := board.Add(projectID, *p.Title, board.AddOpts{
			Column:      p.Column,
			Isolate:     p.Isolate != nil && *p.Isolate,
			Agent:       deref(p.Agent),
			Model:       deref(p.Model),
			Description: deref(p.Description),
			Criteria:    deref(p.Criteria),
			BlockedBy:   p.BlockedBy,
		})
		if err == board.ErrBadLink {
			return Result{}, errors.New("blockedBy references a card that does not exist (see action list)")
		}
		if err != nil {
			return Result{}, err
		}
		t.notify()
		out := fmt.Sprintf("Card %s (%q) created in column %q.", card.ID, card.Title, card.Column)
		if p.Isolate != nil && *p.Isolate {
			out += " It will run in an isolated git worktree on its own branch when started."
		}
		return Result{
			Title:    "Card added: " + card.Title,
			Metadata: map[string]any{"card": card},
			Output:   out,
		}, nil

	case "edit":
		if p.Card == "" {
			return Result{}, errors.New("edit requires a card ID (see action list)")
		}
		if p.Title == nil && p.Description == nil && p.Criteria == nil && p.Agent == nil && p.Model == nil && p.Isolate == nil && p.BlockedBy == nil {
			return Result{}, errors.New("edit requires at least one field to change: title, description, criteria, agent, model, isolate or blockedBy")
		}
		if p.Title != nil && strings.TrimSpace(*p.Title) == "" {
			return Result{}, errors.New("title cannot be empty")
		}
		// blockers must exist and not be the card itself; duplicates and
		// self-references are dropped, an empty array clears the list
		var blockers []string
		if p.BlockedBy != nil {
			cards, err := board.Get(projectID)
			if err != nil {
				return Result{}, err
			}
			known := map[string]bool{}
			for _, c := range cards {
				known[c.ID] = true
			}
			seen := map[string]bool{}
			for _, b := range p.BlockedBy {
				if b == p.Card || seen[b] {
					continue
				}
				if !known[b] {
					return Result{}, fmt.Errorf("blockedBy references a card that does not exist: %s (see action list)", b)
				}
				seen[b] = true
				blockers = append(blockers, b)
			}
		}
		card, err := board.Edit(projectID, p.Card, func(c *board.Card) error {
			// checked under the board lock so a card that starts between
			// the caller's decision and this mutation is still rejected
			if !board.IsWaiting(c.Column) {
				return errNotWaiting
			}
			if p.Title != nil {
				c.Title = strings.TrimSpace(*p.Title)
			}
			if p.Description != nil {
				c.Description = *p.Description
			}
			if p.Criteria != nil {
				c.Criteria = *p.Criteria
			}
			if p.Agent != nil {
				c.Agent = *p.Agent
			}
			if p.Model != nil {
				c.Model = *p.Model
			}
			if p.Isolate != nil {
				c.Isolate = *p.Isolate
			}
			if p.BlockedBy != nil {
				c.BlockedBy = blockers
			}
			return nil
		})
		if err == board.ErrNotFound {
			return Result{}, fmt.Errorf("card not found: %s (use action list to see card IDs)", p.Card)
		}
		if err == errNotWaiting {
			return Result{}, errors.New("only cards waiting in the planning or open column can be edited (a started card belongs to its session, a finished one is history)")
		}
		if err != nil {
			return Result{}, err
		}
		t.notify()
		return Result{
			Title:    "Edited card " + card.ID,
			Metadata: map[string]any{"card": card},
			Output:   fmt.Sprintf("Card %s (%q) updated.", card.ID, card.Title),
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
			return Result{}, fmt.Errorf("card %s cannot be linked to %q: the target must be another card that is still waiting (planning or open)", id, p.Target)
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

	case "schedule":
		id := p.Card
		if id == "" {
			return Result{}, errors.New("schedule requires a card ID (see action list)")
		}
		startAt := p.StartAt
		if p.In != "" {
			if startAt != 0 {
				return Result{}, errors.New("pass either startAt or in, not both")
			}
			d, err := parseDuration(p.In)
			if err != nil {
				return Result{}, fmt.Errorf("invalid duration %q: %v", p.In, err)
			}
			startAt = time.Now().Add(d).UnixMilli()
		}
		card, err := board.SetSchedule(projectID, id, startAt)
		if err == board.ErrNotFound {
			return Result{}, fmt.Errorf("card not found: %s (use action list to see card IDs)", id)
		}
		if err == board.ErrNotOpen {
			return Result{}, errors.New("only cards still waiting (planning or open) can be scheduled")
		}
		if err != nil {
			return Result{}, err
		}
		t.notify()
		if startAt == 0 {
			return Result{
				Title:    "Unscheduled " + card.Title,
				Metadata: map[string]any{"card": card},
				Output:   fmt.Sprintf("Cleared the start schedule of card %s (%q).", card.ID, card.Title),
			}, nil
		}
		return Result{
			Title:    "Scheduled " + card.Title,
			Metadata: map[string]any{"card": card},
			Output:   fmt.Sprintf("Card %s (%q) is scheduled to start at %s: a background scheduler will start it then, exactly like the Start button.", card.ID, card.Title, time.UnixMilli(startAt).Format(time.RFC1123)),
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
		out := fmt.Sprintf("Started a new agent session (%s) on card %s (%q): the card is linked to it, moved to inprogress, and its title is the session's task.", sessID, p.Card, title)
		// the orchestrator automatically watches every ticket it starts:
		// when the card's own session marks it done, the orchestrator is
		// pinged with a short "Ticket X is done." message so it can
		// coordinate follow-ups without being asked
		if c.Agent == "orchestrator" {
			if _, werr := board.Watch(projectID, p.Card, c.SessionID, true); werr == nil {
				t.notify()
				out += "\nYou are now watching this card: when its session marks it done, you will receive a message on behalf of the user."
			}
		}
		return Result{
			Title:    "Started: " + title,
			Metadata: map[string]any{"sessionID": sessID, "cardID": p.Card},
			Output:   out,
		}, nil

	case "approve", "reject":
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
		if t.ResolveReview == nil {
			return Result{}, errors.New("review resolution is not available in this context")
		}
		card, err := t.ResolveReview(projectID, id, p.Action == "approve")
		if err == board.ErrNotFound {
			return Result{}, fmt.Errorf("card not found: %s (use action list to see card IDs)", id)
		}
		if err != nil {
			return Result{}, err
		}
		t.notify()
		if p.Action == "approve" {
			return Result{
				Title:    "Approved card " + card.ID,
				Metadata: map[string]any{"card": card},
				Output:   fmt.Sprintf("Card %s (%q) approved and moved to done.", card.ID, card.Title),
			}, nil
		}
		return Result{
			Title:    "Rejected card " + card.ID,
			Metadata: map[string]any{"card": card},
			Output:   fmt.Sprintf("Card %s (%q) sent back to inprogress; its session was prompted with the review findings.", card.ID, card.Title),
		}, nil

	case "merge":
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
		if t.MergeCard == nil {
			return Result{}, errors.New("merging cards is not available in this context")
		}
		card, err := t.MergeCard(projectID, id)
		if err == board.ErrNotFound {
			return Result{}, fmt.Errorf("card not found: %s (use action list to see card IDs)", id)
		}
		if err != nil {
			return Result{}, err
		}
		t.notify()
		return Result{
			Title:    "Merged card " + card.ID,
			Metadata: map[string]any{"card": card},
			Output:   fmt.Sprintf("Card %s (%q): its worktree branch was merged into the repository and the worktree was removed.", card.ID, card.Title),
		}, nil

	default:
		return Result{}, fmt.Errorf("unknown action %q (valid: list, move, add, edit, watch, unwatch, link, unlink, schedule, start, approve, reject, merge)", p.Action)
	}
}

// deref returns *s or "" when s is nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// parseDuration parses a duration string for the schedule action's "in"
// parameter: time.ParseDuration syntax plus a plain-number-of-days form
// ("1d", "2d") that Go does not support.
func parseDuration(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	if n := len(s); n > 1 && s[n-1] == 'd' {
		var days int
		if _, err := fmt.Sscanf(s[:n-1], "%d", &days); err == nil && days > 0 {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}
	return 0, fmt.Errorf("not a duration")
}

func (t BoardTool) notify() {
	if t.OnUpdate != nil {
		t.OnUpdate()
	}
}
