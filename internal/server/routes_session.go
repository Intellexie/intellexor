package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Intellexie/intellexor/internal/app"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/util"
)

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	merged := mergedConfig(in.Directory)
	scope := r.URL.Query().Get("scope")
	list := in.Mgr.List(in.Directory, scope == "project", projectIDOf(in.Directory))
	out := make([]*session.Session, 0, len(list))
	for _, sess := range list {
		if sess.ParentID != "" && r.URL.Query().Get("roots") == "true" {
			continue
		}
		cp := *sess
		if cp.Model == nil && merged.Model != "" {
			// leave unset like opencode
		}
		out = append(out, &cp)
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	var body struct {
		ParentID string `json:"parentID"`
		Title    string `json:"title"`
		Agent    string `json:"agent"`
		Model    *struct {
			ID         string `json:"id"`
			ProviderID string `json:"providerID"`
			Variant    string `json:"variant"`
		} `json:"model"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := decodeBody(r, &body); err != nil {
		return err
	}
	sess := &session.Session{
		ProjectID: projectIDOf(in.Directory),
		Directory: in.Directory,
		Title:     body.Title,
		Agent:     body.Agent,
		Metadata:  body.Metadata,
	}
	if body.ParentID != "" {
		sess.ParentID = body.ParentID
		if sess.Title == "" {
			sess.Title = "Child session - " + time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	if body.Model != nil {
		sess.Model = &session.ModelRef{ID: body.Model.ID, ProviderID: body.Model.ProviderID, Variant: body.Model.Variant}
	}
	if err := in.Mgr.Create(sess); err != nil {
		return badRequest(err.Error())
	}
	in.Hub.Publish("session.created", map[string]any{"sessionID": sess.ID, "info": *sess})
	in.Hub.Publish("session.updated", map[string]any{"sessionID": sess.ID, "info": *sess})
	writeJSON(w, 200, sess)
	return nil
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	_ = in
	if err != nil {
		return err
	}
	writeJSON(w, 200, sess)
	return nil
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	in.Abort(sess.ID)
	if err := in.Mgr.Delete(sess); err != nil {
		return badRequest(err.Error())
	}
	in.Hub.Publish("session.deleted", map[string]any{"sessionID": sess.ID, "info": *sess})
	writeTrue(w)
	return nil
}

func (s *Server) patchSession(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	var body struct {
		Title      *string                   `json:"title"`
		Metadata   map[string]any            `json:"metadata"`
		Permission *[]session.PermissionRule `json:"permission"`
		Time       *struct {
			Archived *int64 `json:"archived"`
		} `json:"time"`
	}
	if err := decodeBody(r, &body); err != nil {
		return err
	}
	if body.Title != nil {
		sess.Title = *body.Title
	}
	if body.Metadata != nil {
		sess.Metadata = body.Metadata
	}
	if body.Permission != nil {
		merged := map[string]session.PermissionRule{}
		var order []string
		for _, r := range sess.Permission {
			k := r.Permission + "\x00" + r.Pattern
			if _, ok := merged[k]; !ok {
				order = append(order, k)
			}
			merged[k] = r
		}
		for _, r := range *body.Permission {
			k := r.Permission + "\x00" + r.Pattern
			if _, ok := merged[k]; !ok {
				order = append(order, k)
			}
			merged[k] = r
		}
		var out []session.PermissionRule
		for _, k := range order {
			out = append(out, merged[k])
		}
		sess.Permission = out
	}
	if body.Time != nil && body.Time.Archived != nil {
		sess.Time.Archived = body.Time.Archived
	}
	if err := in.Mgr.Save(sess); err != nil {
		return badRequest(err.Error())
	}
	in.Hub.Publish("session.updated", map[string]any{"sessionID": sess.ID, "info": *sess})
	writeJSON(w, 200, sess)
	return nil
}

func (s *Server) sessionChildren(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	children := in.Mgr.Children(sess.ID)
	if children == nil {
		children = []*session.Session{}
	}
	writeJSON(w, 200, children)
	return nil
}

func (s *Server) sessionTodo(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	var todos []map[string]any
	_ = in.Mgr.ReadKV("todo", sess.ID, &todos)
	if todos == nil {
		todos = []map[string]any{}
	}
	writeJSON(w, 200, todos)
	return nil
}

func (s *Server) sessionDiff(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	var diffs []session.FileDiff
	_ = in.Mgr.ReadKV("session_diff", sess.ID, &diffs)
	if diffs == nil {
		diffs = []session.FileDiff{}
	}
	writeJSON(w, 200, diffs)
	return nil
}

func (s *Server) sessionMessages(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	msgs, err2 := in.Mgr.LoadMessages(sess)
	if err2 != nil {
		return badRequest(err2.Error())
	}
	if msgs == nil {
		msgs = []session.WithParts{}
	}
	writeJSON(w, 200, msgs)
	return nil
}

func (s *Server) sessionStatus(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	writeJSON(w, 200, in.Statuses())
	return nil
}

func (s *Server) abortSession(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	in.Abort(sess.ID)
	writeTrue(w)
	return nil
}

func (s *Server) sessionMessage(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	var in2 app.PromptInput
	if e := decodeBody(r, &in2); e != nil {
		return e
	}
	if len(in2.Parts) == 0 {
		return badRequest("parts are required")
	}
	wp, perr := in.Prompt(sess, in2)
	if perr != nil {
		return badRequest(perr.Error())
	}
	writeJSON(w, 200, wp)
	return nil
}

func (s *Server) sessionPromptAsync(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	var in2 app.PromptInput
	if e := decodeBody(r, &in2); e != nil {
		return e
	}
	if len(in2.Parts) == 0 {
		return badRequest("parts are required")
	}
	go func() {
		_, _ = in.Prompt(sess, in2)
	}()
	w.WriteHeader(204)
	return nil
}

func (s *Server) sessionShell(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	var body struct {
		Command string `json:"command"`
		Agent   string `json:"agent"`
	}
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	cmd := "!" + body.Command
	raw, _ := json.Marshal(map[string]any{"type": "text", "text": cmd})
	_, perr := in.Prompt(sess, app.PromptInput{Agent: body.Agent, Parts: []json.RawMessage{raw}})
	if perr != nil {
		return badRequest(perr.Error())
	}
	msgs, _ := in.Mgr.LoadMessages(sess)
	for j := len(msgs) - 1; j >= 0; j-- {
		if _, ok := msgs[j].Info.(session.AssistantMessage); ok {
			writeJSON(w, 200, msgs[j])
			return nil
		}
	}
	writeJSON(w, 200, session.WithParts{})
	return nil
}

func (s *Server) sessionCommand(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	_ = sess
	var body struct {
		Command   string `json:"command"`
		Arguments string `json:"arguments"`
		Agent     string `json:"agent"`
		Model     string `json:"model"`
	}
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	merged := mergedConfig(in.Directory)
	template, cmdAgent, ok := resolveCommand(body.Command, merged.Commands)
	if !ok {
		return notFound("Command not found: " + body.Command)
	}
	template = strings.ReplaceAll(template, "${path}", in.Directory)
	text := strings.ReplaceAll(template, "$ARGUMENTS", body.Arguments)
	for i, arg := range strings.Fields(body.Arguments) {
		text = strings.ReplaceAll(text, fmt.Sprintf("$%d", i+1), arg)
	}
	raw, _ := json.Marshal(map[string]any{"type": "text", "text": text})
	agent := cmdAgent
	if body.Agent != "" {
		agent = body.Agent
	}
	_, perr := in.Prompt(sess, app.PromptInput{Agent: agent, Parts: []json.RawMessage{raw}})
	if perr != nil {
		return badRequest(perr.Error())
	}
	in.Hub.Publish("command.executed", map[string]any{"name": body.Command, "sessionID": sess.ID, "arguments": body.Arguments})
	msgs, _ := in.Mgr.LoadMessages(sess)
	for j := len(msgs) - 1; j >= 0; j-- {
		if _, ok := msgs[j].Info.(session.AssistantMessage); ok {
			writeJSON(w, 200, msgs[j])
			return nil
		}
	}
	writeJSON(w, 200, session.WithParts{})
	return nil
}

func (s *Server) sessionInit(w http.ResponseWriter, r *http.Request) *apiError {
	writeTrue(w)
	return nil
}

func (s *Server) sessionFork(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	var body struct {
		MessageID string `json:"messageID"`
	}
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	dup, ferr := in.Fork(sess, body.MessageID)
	if ferr != nil {
		return badRequest(ferr.Error())
	}
	writeJSON(w, 200, dup)
	return nil
}

func (s *Server) deleteSessionMessage(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	mid := r.PathValue("messageID")
	if err := in.Mgr.DeleteMessage(sess, mid); err != nil {
		return notFound("Message not found: " + mid)
	}
	in.Hub.Publish("message.removed", map[string]any{"sessionID": sess.ID, "messageID": mid})
	writeTrue(w)
	return nil
}

func (s *Server) deleteSessionPart(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	mid := r.PathValue("messageID")
	pid := r.PathValue("partID")
	if err := in.Mgr.DeletePart(sess, mid, pid); err != nil {
		return notFound("Part not found: " + pid)
	}
	in.Hub.Publish("message.part.removed", map[string]any{"sessionID": sess.ID, "messageID": mid, "partID": pid})
	writeTrue(w)
	return nil
}

func (s *Server) revertSession(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	var body struct {
		MessageID string `json:"messageID"`
		PartID    string `json:"partID"`
	}
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	if body.MessageID == "" {
		return badRequest("messageID is required")
	}
	updated, rerr := in.Revert(sess, body.MessageID)
	if rerr != nil {
		return badRequest(rerr.Error())
	}
	writeJSON(w, 200, updated)
	return nil
}

func (s *Server) unrevertSession(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	updated, rerr := in.Unrevert(sess)
	if rerr != nil {
		return badRequest(rerr.Error())
	}
	writeJSON(w, 200, updated)
	return nil
}

func (s *Server) shareSession(w http.ResponseWriter, r *http.Request) *apiError {
	_, _, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	return &apiError{500, "NamedError.Unknown", map[string]any{"message": "sharing is not available in Intellexor"}}
}

func (s *Server) unreferenced() { _ = util.Now }
