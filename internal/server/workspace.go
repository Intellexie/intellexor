package server

import (
	"os"
	"path/filepath"
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
	Directory string `json:"directory"`
	Name      string `json:"name"`
	Sessions  int    `json:"sessions"`
	Busy      bool   `json:"busy"`
	HasIdle   bool   `json:"hasIdle"`
	// Questions is the number of sessions in this workspace currently blocked
	// on a pending question tool call — the web UI pulses the workspace's tab
	// while it is positive (the agent needs the user's attention there).
	Questions    int   `json:"questions"`
	LastActivity int64 `json:"lastActivity"`
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
		info := workspaceInfo{Directory: d, Name: workspaceDisplayName(d), Busy: len(sts) > 0, Questions: len(in.Quest.Pending())}
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

// renameWorkspace and the other HTTP handlers live in project.go now — the
// workspace surface is exposed through the opencode-compatible /project
// routes. This file keeps the persistence and info-building internals.
