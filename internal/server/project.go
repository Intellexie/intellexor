package server

import (
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Projects are the opencode-compatible view of workspaces. opencode models
// every working directory as a "project" with a deterministic id, a canonical
// root and display metadata; Intellexor workspaces map onto that shape so the
// same clients work against both servers. Creation stays explicit here
// (POST /project) where opencode resolves projects implicitly from the
// directory of the first request.
//
// Wire shapes follow opencode's Project.Info / Project.Current schemas, with
// a few Intellexor-only fields (directory, sessions, busy, hasIdle,
// lastActivity) that opencode clients ignore.

// projectID derives opencode's fallback project id: sha1("directory:" + abs
// path), hex-encoded. opencode prefers git-remote-derived ids when available;
// the directory hash is its documented fallback and is equally deterministic.
func projectID(dir string) string {
	sum := sha1.Sum([]byte("directory:" + dir))
	return hex.EncodeToString(sum[:])
}

type projectTime struct {
	Created int64 `json:"created"`
	Updated int64 `json:"updated"`
	Active  int64 `json:"active"`
}

type projectInfo struct {
	ID        string      `json:"id"`
	Canonical string      `json:"canonical"`
	Vcs       string      `json:"vcs,omitempty"`
	Name      string      `json:"name,omitempty"`
	Time      projectTime `json:"time"`
	Sandboxes []string    `json:"sandboxes"`

	// Intellexor extensions (not part of opencode's Project.Info):
	Directory    string `json:"directory"`
	Sessions     int    `json:"sessions"`
	Busy         bool   `json:"busy"`
	HasIdle      bool   `json:"hasIdle"`
	LastActivity int64  `json:"lastActivity"`
}

// projectCurrent mirrors opencode's Project.Current: the project resolved for
// the request's directory.
type projectCurrent struct {
	ID        string `json:"id"`
	Directory string `json:"directory"`
	Canonical string `json:"canonical"`
}

func isGitDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git")) // dir or file (worktrees)
	return err == nil
}

// addedAtFor returns the registration timestamp of dir, or 0 for the default
// directory (which exists implicitly and has no stored record).
func addedAtFor(dir string) int64 {
	for _, w := range loadWorkspaces() {
		if w.Directory == dir {
			return w.AddedAt
		}
	}
	return 0
}

func toProjectInfo(ws workspaceInfo) projectInfo {
	return projectInfo{
		ID:        projectID(ws.Directory),
		Canonical: ws.Directory,
		Vcs:       vcsType(ws.Directory),
		Name:      ws.Name,
		Time: projectTime{
			Created: addedAtFor(ws.Directory),
			Updated: ws.LastActivity,
			Active:  ws.LastActivity,
		},
		Sandboxes: []string{},

		Directory:    ws.Directory,
		Sessions:     ws.Sessions,
		Busy:         ws.Busy,
		HasIdle:      ws.HasIdle,
		LastActivity: ws.LastActivity,
	}
}

// vcsType returns "git" when dir is inside a git repository (including
// worktrees, whose .git is a file), else "".
func vcsType(dir string) string {
	if isGitDir(dir) {
		return "git"
	}
	return ""
}

func (s *Server) projectInfos() []projectInfo {
	infos := s.workspaceInfos()
	out := make([]projectInfo, 0, len(infos))
	for _, ws := range infos {
		out = append(out, toProjectInfo(ws))
	}
	return out
}

// listProjects handles GET /project. The `archived` query param is accepted
// for opencode compatibility; Intellexor never archives projects, so the
// response is always the full list.
func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) *apiError {
	writeJSON(w, 200, s.projectInfos())
	return nil
}

// currentProject handles GET /project/current: the project resolved for the
// request's directory (defaulting to the server's default directory).
func (s *Server) currentProject(w http.ResponseWriter, r *http.Request) *apiError {
	dir := directoryOf(r)
	if dir == "" {
		dir = s.DefaultDir
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	writeJSON(w, 200, projectCurrent{ID: projectID(dir), Directory: dir, Canonical: dir})
	return nil
}

// createProject handles POST /project — the explicit counterpart of
// opencode's implicit project resolution. Body: `{ "path": "/abs" }`
// (`directory` accepted as an alias). Validates the directory, eagerly
// creates the instance, seeds its config and registers it.
func (s *Server) createProject(w http.ResponseWriter, r *http.Request) *apiError {
	var body struct {
		Path      string `json:"path"`
		Directory string `json:"directory"`
	}
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	path := body.Path
	if path == "" {
		path = body.Directory
	}
	if path == "" {
		return badRequest("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return badRequest("invalid path: " + path)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return notFound("Directory not found: " + abs)
	}
	if !st.IsDir() {
		return badRequest("Not a directory: " + abs)
	}
	// eagerly create the instance so the project is fully usable the moment
	// the UI switches to it (config merge, agents, providers)
	s.Instance(abs)
	ensureWorkspaceConfig(abs)
	registerWorkspace(abs)
	for _, info := range s.projectInfos() {
		if info.Directory == abs {
			writeJSON(w, 200, info)
			return nil
		}
	}
	writeJSON(w, 200, projectInfo{ID: projectID(abs), Directory: abs, Canonical: abs, Name: workspaceName(abs), Sandboxes: []string{}})
	return nil
}

// updateProject handles PATCH /project/{projectID} — opencode's
// project.update. Only `name` is persisted (the rename); `canonical`, `icon`
// and `commands` are accepted for shape compatibility but ignored. An empty
// name resets to the directory base name.
func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) *apiError {
	id := r.PathValue("projectID")
	if id == "" {
		return badRequest("projectID is required")
	}
	var body struct {
		Name      string         `json:"name"`
		Canonical string         `json:"canonical"`
		Icon      map[string]any `json:"icon"`
		Commands  map[string]any `json:"commands"`
	}
	if e := decodeBody(r, &body); e != nil {
		return e
	}

	// resolve the projectID to a known directory (default dir first, then
	// every registered workspace)
	dirs := append([]string{s.DefaultDir}, func() []string {
		var out []string
		for _, w := range loadWorkspaces() {
			out = append(out, w.Directory)
		}
		return out
	}()...)
	var dir string
	seen := map[string]bool{}
	for _, d := range dirs {
		if seen[d] {
			continue
		}
		seen[d] = true
		if projectID(d) == id {
			dir = d
			break
		}
	}
	if dir == "" {
		return &apiError{404, "ProjectNotFoundError", map[string]any{"projectID": id, "message": "Project not found: " + id}}
	}

	// persist the rename; the default directory is usually not registered
	// (it exists implicitly) — renaming it seeds its record so the name sticks
	list := loadWorkspaces()
	found := false
	for i := range list {
		if list[i].Directory == dir {
			list[i].Name = strings.TrimSpace(body.Name)
			found = true
			break
		}
	}
	if !found && dir == s.DefaultDir {
		list = append(list, Workspace{Directory: dir, Name: strings.TrimSpace(body.Name), AddedAt: time.Now().UnixMilli()})
		found = true
	}
	if !found {
		return &apiError{404, "ProjectNotFoundError", map[string]any{"projectID": id, "message": "Project not found: " + id}}
	}
	if err := saveWorkspaces(list); err != nil {
		return badRequest(err.Error())
	}

	for _, info := range s.projectInfos() {
		if info.Directory == dir {
			writeJSON(w, 200, info)
			return nil
		}
	}
	writeJSON(w, 200, projectInfo{ID: id, Directory: dir, Canonical: dir, Name: workspaceDisplayName(dir), Sandboxes: []string{}})
	return nil
}
