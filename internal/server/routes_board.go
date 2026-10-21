package server

import (
	"fmt"
	"net/http"
	"path/filepath"

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
	if err == board.ErrBadSession {
		return badRequest("Only cards still waiting in the open column can be linked to a session")
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
