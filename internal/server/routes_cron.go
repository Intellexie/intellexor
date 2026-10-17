package server

import (
	"net/http"
	"time"

	"github.com/Intellexie/intellexor/internal/cron"
	"github.com/Intellexie/intellexor/internal/store"
)

// Cron jobs: a prompt that automatically starts a fresh agent session on a
// schedule. Jobs are stored per project (keyed like sessions and the board)
// and every mutation is broadcast on the instance hub as cron.updated so
// other connected clients refresh. The runs themselves are driven by the
// background sweep in internal/app.

func (s *Server) cronProjectID(r *http.Request) string {
	return store.ProjectID(s.Instance(directoryOf(r)).Directory)
}

func (s *Server) publishCronUpdate(r *http.Request) {
	in := s.Instance(directoryOf(r))
	in.Hub.Publish("cron.updated", map[string]any{"directory": in.Directory})
}

func (s *Server) listCron(w http.ResponseWriter, r *http.Request) *apiError {
	jobs, err := cron.Get(s.cronProjectID(r))
	if err != nil {
		return badRequest(err.Error())
	}
	writeJSON(w, 200, jobs)
	return nil
}

func (s *Server) createCron(w http.ResponseWriter, r *http.Request) *apiError {
	var body struct {
		Title  string `json:"title"`
		Prompt string `json:"prompt"`
		Spec   string `json:"spec"`
	}
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	if body.Prompt == "" {
		return badRequest("prompt is required")
	}
	if body.Spec == "" {
		return badRequest("spec is required")
	}
	job, err := cron.Add(s.cronProjectID(r), body.Title, body.Prompt, body.Spec)
	if err != nil {
		return badRequest(err.Error())
	}
	s.publishCronUpdate(r)
	writeJSON(w, 200, job)
	return nil
}

func (s *Server) patchCron(w http.ResponseWriter, r *http.Request) *apiError {
	var body struct {
		Title  *string `json:"title"`
		Prompt *string `json:"prompt"`
		Spec   *string `json:"spec"`
		Paused *bool   `json:"paused"`
	}
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	id := r.PathValue("cronID")
	var job cron.Job
	var err error
	if body.Title != nil || body.Prompt != nil || body.Spec != nil {
		job, err = cron.Update(s.cronProjectID(r), id, body.Title, body.Prompt, body.Spec)
		if err == cron.ErrNotFound {
			return notFound("Cron job not found: " + id)
		}
		if err != nil {
			return badRequest(err.Error())
		}
	}
	if body.Paused != nil {
		job, err = cron.SetPaused(s.cronProjectID(r), id, *body.Paused)
		if err == cron.ErrNotFound {
			return notFound("Cron job not found: " + id)
		}
		if err != nil {
			return badRequest(err.Error())
		}
	}
	if body.Title == nil && body.Prompt == nil && body.Spec == nil && body.Paused == nil {
		return badRequest("nothing to update")
	}
	s.publishCronUpdate(r)
	writeJSON(w, 200, job)
	return nil
}

func (s *Server) deleteCron(w http.ResponseWriter, r *http.Request) *apiError {
	err := cron.Delete(s.cronProjectID(r), r.PathValue("cronID"))
	if err == cron.ErrNotFound {
		return notFound("Cron job not found: " + r.PathValue("cronID"))
	}
	if err != nil {
		return badRequest(err.Error())
	}
	s.publishCronUpdate(r)
	writeTrue(w)
	return nil
}

// previewCron validates a schedule expression and returns its next fire
// time (unix ms) — the create/edit dialog uses it for a live "next run"
// preview while the user types.
func (s *Server) previewCron(w http.ResponseWriter, r *http.Request) *apiError {
	spec := r.URL.Query().Get("spec")
	t, err := cron.Next(spec, time.Now())
	if err != nil {
		return badRequest(err.Error())
	}
	writeJSON(w, 200, map[string]any{"next": t.UnixMilli()})
	return nil
}
