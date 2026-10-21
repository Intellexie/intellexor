package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Intellexie/intellexor/internal/store"
)

// Workspaces are additional working directories registered from the web UI.
// The server already keeps one app.Instance per directory (created lazily on
// first request), but instances only come into being when a request carries
// their directory — so the list of known workspaces is persisted in the data
// dir and re-created eagerly here.

type Workspace struct {
	Directory string `json:"directory"`
	// Name is an optional user-assigned display name (web UI rename); empty
	// means "derive from the directory base name".
	Name    string `json:"name,omitempty"`
	AddedAt int64  `json:"addedAt"`
}

type workspaceInfo struct {
	Directory    string `json:"directory"`
	Name         string `json:"name"`
	Sessions     int    `json:"sessions"`
	Busy         bool   `json:"busy"`
	HasIdle      bool   `json:"hasIdle"`
	LastActivity int64  `json:"lastActivity"`
}

var wsMu sync.Mutex

func workspacesFile() string {
	return filepath.Join(store.DataDir(), "storage", "workspaces.json")
}

func loadWorkspaces() []Workspace {
	wsMu.Lock()
	defer wsMu.Unlock()
	var out []Workspace
	if err := store.ReadJSON(workspacesFile(), &out); err != nil {
		return nil
	}
	return out
}

func saveWorkspaces(list []Workspace) error {
	wsMu.Lock()
	defer wsMu.Unlock()
	st := &store.Store{Root: store.DataDir()}
	return st.WriteJSON(workspacesFile(), list)
}

func registerWorkspace(dir string) {
	list := loadWorkspaces()
	for _, w := range list {
		if w.Directory == dir {
			return
		}
	}
	list = append(list, Workspace{Directory: dir, AddedAt: time.Now().UnixMilli()})
	_ = saveWorkspaces(list)
}

func workspaceName(dir string) string {
	base := filepath.Base(dir)
	if base == "" || base == "/" || base == "." {
		return dir
	}
	return base
}

// ensureWorkspaceConfig gives a new workspace its own config file. Config
// resolution already falls back to parent directories and the global config
// (config.Merge walks upward), so the seeded file only needs to exist — an
// empty object keeps every fallback intact while marking the workspace as
// independently configurable.
func ensureWorkspaceConfig(dir string) {
	for _, name := range []string{"intellexor.json", "intellexor.jsonc", "opencode.json", "opencode.jsonc"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return
		}
	}
	const seed = `{
  // Workspace-local Intellexor config. Settings here override the global
  // config (~/.config/intellexor/) and any parent-directory configs.
}
`
	_ = os.WriteFile(filepath.Join(dir, "intellexor.json"), []byte(seed), 0o644)
}

// workspaceDisplayName returns the user-assigned name for dir when one was
// set, else the directory base name.
func workspaceDisplayName(dir string) string {
	for _, w := range loadWorkspaces() {
		if w.Directory == dir && w.Name != "" {
			return w.Name
		}
	}
	return workspaceName(dir)
}

// workspaceInfos builds the workspace list shown in the UI: the default
// directory first, then every registered workspace. Busy/last-activity come
// from each instance's live status map and session store.
func (s *Server) workspaceInfos() []workspaceInfo {
	var dirs []string
	seen := map[string]bool{}
	add := func(d string) {
		if d == "" || seen[d] {
			return
		}
		seen[d] = true
		dirs = append(dirs, d)
	}
	add(s.DefaultDir)
	for _, w := range loadWorkspaces() {
		add(w.Directory)
	}
	out := make([]workspaceInfo, 0, len(dirs))
	for _, d := range dirs {
		in := s.Instance(d)
		sts := in.Statuses()
		info := workspaceInfo{Directory: d, Name: workspaceDisplayName(d), Busy: len(sts) > 0}
		for _, sess := range in.Mgr.List(in.Directory, false, projectIDOf(in.Directory)) {
			if sess.ParentID != "" {
				continue // subagents are not workspace-level sessions
			}
			info.Sessions++
			if sess.Time.Updated > info.LastActivity {
				info.LastActivity = sess.Time.Updated
			}
			if _, busy := sts[sess.ID]; !busy {
				info.HasIdle = true
			}
		}
		out = append(out, info)
	}
	return out
}

// renameWorkspace sets (or clears) a workspace's display name. An empty
// name resets to the directory base name.
func (s *Server) renameWorkspace(w http.ResponseWriter, r *http.Request) *apiError {
	var body struct {
		Directory string `json:"directory"`
		Name      string `json:"name"`
	}
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	if body.Directory == "" {
		return badRequest("directory is required")
	}
	list := loadWorkspaces()
	found := false
	for i := range list {
		if list[i].Directory == body.Directory {
			list[i].Name = strings.TrimSpace(body.Name)
			found = true
			break
		}
	}
	// the default directory is usually not registered (it exists implicitly)
	// — renaming it seeds its record so the name persists
	if !found && body.Directory == s.DefaultDir {
		list = append(list, Workspace{Directory: body.Directory, Name: strings.TrimSpace(body.Name), AddedAt: time.Now().UnixMilli()})
		found = true
	}
	if !found {
		return notFound("Workspace not found: " + body.Directory)
	}
	if err := saveWorkspaces(list); err != nil {
		return badRequest(err.Error())
	}
	writeTrue(w)
	return nil
}

func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) *apiError {
	writeJSON(w, 200, s.workspaceInfos())
	return nil
}

func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) *apiError {
	var body struct {
		Path string `json:"path"`
	}
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	if body.Path == "" {
		return badRequest("path is required")
	}
	abs, err := filepath.Abs(body.Path)
	if err != nil {
		return badRequest("invalid path: " + body.Path)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return notFound("Directory not found: " + abs)
	}
	if !st.IsDir() {
		return badRequest("Not a directory: " + abs)
	}
	// eagerly create the instance so the workspace is fully usable the moment
	// the UI switches to it (config merge, agents, providers)
	s.Instance(abs)
	ensureWorkspaceConfig(abs)
	registerWorkspace(abs)
	for _, info := range s.workspaceInfos() {
		if info.Directory == abs {
			writeJSON(w, 200, info)
			return nil
		}
	}
	writeJSON(w, 200, workspaceInfo{Directory: abs, Name: workspaceName(abs)})
	return nil
}
