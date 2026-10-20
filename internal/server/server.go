package server

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Intellexie/intellexor/internal/app"
	"github.com/Intellexie/intellexor/internal/bus"
	"github.com/Intellexie/intellexor/internal/provider"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
)

type Server struct {
	DefaultDir string
	Version    string

	mu        sync.Mutex
	instances map[string]*app.Instance
	gsubs     map[chan globalEvent]struct{}
}

type globalEvent struct {
	Directory string `json:"directory"`
	Payload   any    `json:"payload"`
}

func (s *Server) broadcastGlobal(directory string, ev any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.gsubs {
		select {
		case ch <- globalEvent{Directory: directory, Payload: ev}:
		default:
		}
	}
}

func (s *Server) subscribeGlobal() chan globalEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gsubs == nil {
		s.gsubs = map[chan globalEvent]struct{}{}
	}
	ch := make(chan globalEvent, 256)
	s.gsubs[ch] = struct{}{}
	return ch
}

func (s *Server) unsubscribeGlobal(ch chan globalEvent) {
	s.mu.Lock()
	delete(s.gsubs, ch)
	s.mu.Unlock()
	close(ch)
}

func New(defaultDir string, version string) *Server {
	abs, _ := filepath.Abs(defaultDir)
	return &Server{DefaultDir: abs, Version: version, instances: map[string]*app.Instance{}}
}

func (s *Server) Instance(dir string) *app.Instance {
	if dir == "" {
		dir = s.DefaultDir
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if i, ok := s.instances[abs]; ok {
		return i
	}
	i := app.New(abs)
	instanceDir := abs
	i.Hub.AddMirror(func(ev bus.Event) {
		s.broadcastGlobal(instanceDir, ev)
	})
	s.instances[abs] = i
	return i
}

// ---- auth (opencode semantics: enforced only when a password is set) ----

func (s *Server) authOK(r *http.Request) bool {
	pass := os.Getenv("OPENCODE_SERVER_PASSWORD")
	if pass == "" {
		return true
	}
	user := os.Getenv("OPENCODE_SERVER_USERNAME")
	if user == "" {
		user = "opencode"
	}
	if tok := r.URL.Query().Get("auth_token"); tok != "" {
		if b, err := base64.StdEncoding.DecodeString(tok); err == nil {
			u, p, ok := strings.Cut(string(b), ":")
			if ok && subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1 && subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1 {
				return true
			}
		}
	}
	u, p, ok := r.BasicAuth()
	if ok && subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1 && subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1 {
		return true
	}
	return false
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeTrue(w http.ResponseWriter) { writeJSON(w, 200, true) }

type apiError struct {
	status int
	name   string
	data   map[string]any
}

func (e *apiError) Error() string { return fmt.Sprintf("%s: %v", e.name, e.data) }

func writeErr(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.status, map[string]any{"name": e.name, "data": e.data})
}

func notFound(msg string) *apiError {
	return &apiError{404, "NotFoundError", map[string]any{"message": msg}}
}

func sessionNotFound(id string) *apiError {
	return &apiError{404, "SessionNotFoundError", map[string]any{"sessionID": id, "message": "Session not found: " + id}}
}

func badRequest(msg string) *apiError {
	return &apiError{400, "BadRequest", map[string]any{"message": msg}}
}

// FindSession locates a session across all known instance directories.
func (s *Server) FindSession(id string) (*session.Session, error) {
	dirs := append([]string{s.DefaultDir}, s.InstanceDirs()...)
	seen := map[string]bool{}
	for _, d := range dirs {
		if seen[d] {
			continue
		}
		seen[d] = true
		for _, sess := range s.Instance(d).Mgr.ListAll() {
			if sess.ID == id {
				return sess, nil
			}
		}
	}
	return nil, fmt.Errorf("session %q not found", id)
}

func (s *Server) resolveSession(r *http.Request, id string) (*app.Instance, *session.Session, *apiError) {
	// per opencode, session routes resolve directory from the session itself
	in := s.Instance(directoryOf(r))
	all := in.Mgr.ListAll()
	for _, sess := range all {
		if sess.ID == id {
			in := s.Instance(sess.Directory)
			return in, s.recordTimezone(in, sess, r), nil
		}
	}
	return nil, nil, sessionNotFound(id)
}

// recordTimezone persists the web UI's device timezone (sent as the
// X-Timezone header on every API request) on the session, so tools can
// report times in the user's local time instead of the server's. It only
// writes when the reported zone differs from the stored one; ACP clients
// send no header and keep whatever was stored (or none).
func (s *Server) recordTimezone(in *app.Instance, sess *session.Session, r *http.Request) *session.Session {
	tz := r.Header.Get("X-Timezone")
	if tz == "" || tz == sess.Timezone {
		return sess
	}
	updated, err := in.Mgr.Update(sess.ID, func(u *session.Session) { u.Timezone = tz })
	if err != nil {
		return sess
	}
	return updated
}

func directoryOf(r *http.Request) string {
	if d := r.URL.Query().Get("directory"); d != "" {
		return d
	}
	if d := r.Header.Get("x-opencode-directory"); d != "" {
		return d
	}
	return ""
}

func decodeBody(r *http.Request, v any) *apiError {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if strings.Contains(err.Error(), "EOF") {
			return nil
		}
		return badRequest(err.Error())
	}
	return nil
}

func pathOr(w http.ResponseWriter, err *apiError) bool {
	if err != nil {
		writeErr(w, err)
		return false
	}
	return true
}

// ---- session list ----

func globalHealth(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"healthy": true, "version": s.Version})
	}
}

func globalConfig(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{})
	}
}

// path route
func pathHandler(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in := s.Instance(directoryOf(r))
		home, _ := os.UserHomeDir()
		writeJSON(w, 200, map[string]any{
			"home":      home,
			"state":     store.StateDir(),
			"config":    store.ConfigDir(),
			"worktree":  in.Worktree,
			"directory": in.Directory,
		})
	}
}

func providersHandler(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in := s.Instance(directoryOf(r))
		connected, _ := provider.List(in.Directory)
		var all []providerInfo
		var defaults map[string]string = map[string]string{}
		var connectedIDs []string
		for _, id := range provider.SortedKeys(connected) {
			res := connected[id]
			all = append(all, providerInfo{
				ID: res.ID, Name: res.Name, Source: res.Source, Env: res.Env,
				Options: map[string]any{"apiKey": "", "baseURL": res.BaseURL},
				Models:  res.Models,
			})
			connectedIDs = append(connectedIDs, id)
			if pref := provider.PreferredModel(res); pref != "" {
				defaults[id] = pref
				continue
			}
			ids := make([]string, 0, len(res.Models))
			for mid := range res.Models {
				ids = append(ids, mid)
			}
			sort.Strings(ids)
			if len(ids) > 0 {
				defaults[id] = ids[0]
			}
		}
		writeJSON(w, 200, map[string]any{"all": all, "default": defaults, "connected": connectedIDs})
	}
}

type providerInfo struct {
	ID      string                    `json:"id"`
	Name    string                    `json:"name"`
	Source  string                    `json:"source"`
	Env     []string                  `json:"env"`
	Key     string                    `json:"key,omitempty"`
	Options map[string]any            `json:"options"`
	Models  map[string]provider.Model `json:"models"`
}

func configProvidersHandler(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		providersHandler(s)(w, r)
	}
}

func providerAuthHandler(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		connected, _ := provider.List(s.Instance(directoryOf(r)).Directory)
		out := map[string][]map[string]any{}
		for id, res := range connected {
			method := map[string]any{"type": "api", "label": "api key"}
			switch res.Source {
			case "env":
				method = map[string]any{"type": "api", "label": "api key (env)"}
			}
			out[id] = []map[string]any{method}
		}
		writeJSON(w, 200, out)
	}
}
