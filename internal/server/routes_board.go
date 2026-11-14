package server

import (
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/Intellexie/intellexor/internal/app"
	"github.com/Intellexie/intellexor/internal/board"
	"github.com/Intellexie/intellexor/internal/store"
)

// The kanban board shown as the first tab in the web UI. Cards are stored
// per project (keyed like sessions) and every mutation is broadcast on the
// instance hub as board.updated so other connected clients refresh.

func (s *Server) boardProjectID(r *http.Request) string {
	return store.ProjectID(s.Instance(directoryOf(r)).Directory)
}

func (s *Server) publishBoardUpdate(r *http.Request) {
	in := s.Instance(directoryOf(r))
	in.Hub.Publish("board.updated", map[string]any{"directory": in.Directory})
}

func (s *Server) getBoard(w http.ResponseWriter, r *http.Request) *apiError {
	cards, err := board.Get(s.boardProjectID(r))
	if err != nil {
		return badRequest(err.Error())
	}
	writeJSON(w, 200, cards)
	return nil
}

func (s *Server) createBoardCard(w http.ResponseWriter, r *http.Request) *apiError {
	var body struct {
		Title       string             `json:"title"`
		Column      string             `json:"column"`
		Isolate     bool               `json:"isolate"`
		Attachments []board.Attachment `json:"attachments"`
		Agent       string             `json:"agent"`
		Model       string             `json:"model"`
		Description string             `json:"description"`
		Criteria    string             `json:"criteria"`
		BlockedBy   []string           `json:"blockedBy"`
	}
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	// cards are only ever created in open — work starts by moving a card to
	// inprogress, never by creating it there
	if body.Column != "" && body.Column != "open" {
		return badRequest("Cards can only be added to the open column")
	}
	card, err := board.Add(s.boardProjectID(r), body.Title, board.AddOpts{
		Attachments: body.Attachments,
		Isolate:     body.Isolate,
		Agent:       body.Agent,
		Model:       body.Model,
		Description: body.Description,
		Criteria:    body.Criteria,
		BlockedBy:   body.BlockedBy,
	})
	if err == board.ErrBadLink {
		return badRequest("blockedBy references a card that does not exist")
	}
	if err != nil {
		return badRequest(err.Error())
	}
	s.publishBoardUpdate(r)
	writeJSON(w, 200, card)
	return nil
}

func (s *Server) patchBoardCard(w http.ResponseWriter, r *http.Request) *apiError {
	var body struct {
		Title       *string             `json:"title"`
		Column      *string             `json:"column"`
		Before      *string             `json:"before"`
		SessionID   *string             `json:"sessionID"`
		Link        *string             `json:"link"` // deprecated: use blockedBy
		BlockedBy   []string            `json:"blockedBy"`
		StartAt     *int64              `json:"startAt"`
		Isolate     *bool               `json:"isolate"`
		Agent       *string             `json:"agent"`
		Model       *string             `json:"model"`
		Description *string             `json:"description"`
		Criteria    *string             `json:"criteria"`
		Attachments *[]board.Attachment `json:"attachments"`
	}
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	// linking is its own mutation: point the card at another one (or clear
	// the link with an empty string) so the target auto-starts on done
	if body.Link != nil {
		card, err := board.SetLink(s.boardProjectID(r), r.PathValue("cardID"), *body.Link)
		if err == board.ErrNotFound {
			return notFound("Card not found: " + r.PathValue("cardID"))
		}
		if err == board.ErrBadLink {
			return badRequest("Cards can only be linked to another card that is still waiting in the open column")
		}
		if err != nil {
			return badRequest(err.Error())
		}
		s.publishBoardUpdate(r)
		writeJSON(w, 200, card)
		return nil
	}
	// scheduling is its own mutation too: set the timed start (unix ms) the
	// background scheduler fires, or clear it with 0
	if body.StartAt != nil {
		card, err := board.SetSchedule(s.boardProjectID(r), r.PathValue("cardID"), *body.StartAt)
		if err == board.ErrNotFound {
			return notFound("Card not found: " + r.PathValue("cardID"))
		}
		if err == board.ErrNotOpen {
			return badRequest("Only cards still waiting in the open column can be scheduled")
		}
		if err != nil {
			return badRequest(err.Error())
		}
		s.publishBoardUpdate(r)
		writeJSON(w, 200, card)
		return nil
	}
	// isolation is its own mutation too: toggle the card's worktree flag
	// (only cards still waiting in open can change it)
	if body.Isolate != nil {
		card, err := board.SetIsolate(s.boardProjectID(r), r.PathValue("cardID"), *body.Isolate)
		if err == board.ErrNotFound {
			return notFound("Card not found: " + r.PathValue("cardID"))
		}
		if err == board.ErrNotOpen {
			return badRequest("Only cards still waiting in the open column can change worktree isolation")
		}
		if err != nil {
			return badRequest(err.Error())
		}
		s.publishBoardUpdate(r)
		writeJSON(w, 200, card)
		return nil
	}
	// the richer card fields (agent/model/description/criteria/attachments)
	// and the dependency list are plain edits under one lock
	if body.Agent != nil || body.Model != nil || body.Description != nil || body.Criteria != nil || body.BlockedBy != nil || body.Attachments != nil {
		card, err := board.Edit(s.boardProjectID(r), r.PathValue("cardID"), func(c *board.Card) error {
			if body.Agent != nil {
				c.Agent = *body.Agent
			}
			if body.Model != nil {
				c.Model = *body.Model
			}
			if body.Description != nil {
				c.Description = *body.Description
			}
			if body.Criteria != nil {
				c.Criteria = *body.Criteria
			}
			if body.Attachments != nil {
				c.Attachments = *body.Attachments
			}
			return nil
		})
		if err == board.ErrNotFound {
			return notFound("Card not found: " + r.PathValue("cardID"))
		}
		if err != nil {
			return badRequest(err.Error())
		}
		if body.BlockedBy != nil {
			card, err = board.SetBlockedBy(s.boardProjectID(r), r.PathValue("cardID"), body.BlockedBy)
			if err == board.ErrNotFound || err == board.ErrBadLink {
				return badRequest("blockedBy references a card that does not exist")
			}
			if err != nil {
				return badRequest(err.Error())
			}
		}
		s.publishBoardUpdate(r)
		writeJSON(w, 200, card)
		return nil
	}
	card, err := board.Update(s.boardProjectID(r), r.PathValue("cardID"), body.Title, body.Column, body.Before, body.SessionID)
	if err == board.ErrNotFound {
		return notFound("Card not found: " + r.PathValue("cardID"))
	}
	if err == board.ErrBadSession {
		return badRequest("Only cards still waiting in the open column can be linked to a session")
	}
	if err != nil {
		return badRequest(err.Error())
	}
	// an explicit move into the review column starts the review stage (the
	// review agent inspects the card's changes) — an agent going idle never
	// triggers it on its own
	if body.Column != nil && *body.Column == "review" && card.Column == "review" {
		app.StartReviewOnMove(s.Instance(directoryOf(r)), s.boardProjectID(r), r.PathValue("cardID"))
	}
	// the web UI's Start flow links a freshly created session to the card:
	// when the card wants worktree isolation, the session moves into a new
	// worktree now (before its first prompt)
	if body.SessionID != nil && *body.SessionID != "" {
		s.Instance(directoryOf(r)).EnsureCardWorktree(s.boardProjectID(r), r.PathValue("cardID"), *body.SessionID)
	}
	s.publishBoardUpdate(r)
	writeJSON(w, 200, card)
	return nil
}

// mergeBoardCard merges the card's worktree branch into the repository and
// cleans the worktree up (directory + branch). The card must have been
// started with worktree isolation and not merged yet.
func (s *Server) mergeBoardCard(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	card, err := app.MergeCard(in, s.boardProjectID(r), r.PathValue("cardID"))
	if err == board.ErrNotFound {
		return notFound("Card not found: " + r.PathValue("cardID"))
	}
	if err != nil {
		return badRequest(err.Error())
	}
	s.publishBoardUpdate(r)
	writeJSON(w, 200, card)
	return nil
}

// resolveBoardReview approves (approve=true) or rejects a card waiting in
// review: approve moves it to done (merging an isolated worktree first),
// reject sends it back to inprogress and prompts its session with the
// findings.
func (s *Server) resolveBoardReview(approve bool) func(http.ResponseWriter, *http.Request) *apiError {
	return func(w http.ResponseWriter, r *http.Request) *apiError {
		in := s.Instance(directoryOf(r))
		var card board.Card
		var err error
		if approve {
			card, err = app.ApproveCard(in, s.boardProjectID(r), r.PathValue("cardID"))
		} else {
			card, err = app.RejectCard(in, s.boardProjectID(r), r.PathValue("cardID"))
		}
		if err == board.ErrNotFound {
			return notFound("Card not found: " + r.PathValue("cardID"))
		}
		if err != nil {
			return badRequest(err.Error())
		}
		s.publishBoardUpdate(r)
		writeJSON(w, 200, card)
		return nil
	}
}

func (s *Server) deleteBoardCard(w http.ResponseWriter, r *http.Request) *apiError {
	err := board.Delete(s.boardProjectID(r), r.PathValue("cardID"))
	if err == board.ErrNotFound {
		return notFound("Card not found: " + r.PathValue("cardID"))
	}
	if err != nil {
		return badRequest(err.Error())
	}
	s.publishBoardUpdate(r)
	writeTrue(w)
	return nil
}

// exportBoard streams the whole board (every card with its column, order,
// done-links, schedule and attachments) as a downloadable JSON document.
func (s *Server) exportBoard(w http.ResponseWriter, r *http.Request) *apiError {
	payload, err := board.Export(s.boardProjectID(r))
	if err != nil {
		return badRequest(err.Error())
	}
	name := filepath.Base(s.Instance(directoryOf(r)).Directory)
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "board"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="intellexor-board-%s.json"`, name))
	writeJSON(w, 200, payload)
	return nil
}

// importBoard installs an exported board document into the request's
// project: ?mode=replace (the default) makes the board exactly the
// imported one, ?mode=merge appends its cards to the current board.
func (s *Server) importBoard(w http.ResponseWriter, r *http.Request) *apiError {
	var payload board.ExportPayload
	if e := decodeBody(r, &payload); e != nil {
		return e
	}
	n, err := board.Import(s.boardProjectID(r), &payload, r.URL.Query().Get("mode"))
	if err != nil {
		return badRequest(err.Error())
	}
	s.publishBoardUpdate(r)
	writeJSON(w, 200, map[string]any{"imported": n})
	return nil
}
