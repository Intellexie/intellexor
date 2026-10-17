package server

import (
	"net/http"

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
		Attachments []board.Attachment `json:"attachments"`
	}
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	// cards are only ever created in open — work starts by moving a card to
	// inprogress, never by creating it there
	if body.Column != "" && body.Column != "open" {
		return badRequest("Cards can only be added to the open column")
	}
	card, err := board.Add(s.boardProjectID(r), body.Title, body.Attachments)
	if err != nil {
		return badRequest(err.Error())
	}
	s.publishBoardUpdate(r)
	writeJSON(w, 200, card)
	return nil
}

func (s *Server) patchBoardCard(w http.ResponseWriter, r *http.Request) *apiError {
	var body struct {
		Title     *string `json:"title"`
		Column    *string `json:"column"`
		Before    *string `json:"before"`
		SessionID *string `json:"sessionID"`
		Link      *string `json:"link"`
		StartAt   *int64  `json:"startAt"`
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
	card, err := board.Update(s.boardProjectID(r), r.PathValue("cardID"), body.Title, body.Column, body.Before, body.SessionID)
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
