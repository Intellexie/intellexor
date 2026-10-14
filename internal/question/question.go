package question

import (
	"context"
	"errors"
	"sync"

	"github.com/Intellexie/intellexor/internal/bus"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/util"
)

type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type Q struct {
	Question string   `json:"question"`
	Header   string   `json:"header"`
	Options  []Option `json:"options"`
	Multiple bool     `json:"multiple,omitempty"`
	Custom   *bool    `json:"custom,omitempty"`
}

type Request struct {
	ID        string               `json:"id"`
	SessionID string               `json:"sessionID"`
	Questions []Q                  `json:"questions"`
	Tool      *session.ToolCallRef `json:"tool,omitempty"`
}

type Service struct {
	mu      sync.Mutex
	hub     *bus.Hub
	pending map[string]*entry
}

type entry struct {
	req     Request
	answers chan [][]string
}

func New(hub *bus.Hub) *Service {
	return &Service{hub: hub, pending: map[string]*entry{}}
}

var RejectedError = errors.New("The user dismissed this question")

func (s *Service) Ask(ctx context.Context, sessionID string, questions []Q, tool *session.ToolCallRef) ([][]string, error) {
	req := Request{ID: util.ID("que"), SessionID: sessionID, Questions: questions, Tool: tool}
	e := &entry{req: req, answers: make(chan [][]string, 1)}
	s.mu.Lock()
	s.pending[req.ID] = e
	s.mu.Unlock()
	s.hub.Publish("question.asked", map[string]any{"id": req.ID, "sessionID": sessionID, "questions": req.Questions, "tool": req.Tool})

	select {
	case <-ctx.Done():
		s.Reject(req.ID)
		return nil, ctx.Err()
	case a := <-e.answers:
		if a == nil {
			return nil, RejectedError
		}
		return a, nil
	}
}

func (s *Service) Reply(id string, answers [][]string) error {
	s.mu.Lock()
	e, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if !ok {
		return errors.New("Question not found")
	}
	e.answers <- answers
	s.hub.Publish("question.replied", map[string]any{"sessionID": e.req.SessionID, "requestID": id, "answers": answers})
	return nil
}

func (s *Service) Reject(id string) error {
	s.mu.Lock()
	e, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if !ok {
		return errors.New("Question not found")
	}
	e.answers <- nil
	s.hub.Publish("question.rejected", map[string]any{"sessionID": e.req.SessionID, "requestID": id})
	return nil
}

func (s *Service) Pending() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Request
	for _, e := range s.pending {
		out = append(out, e.req)
	}
	return out
}
