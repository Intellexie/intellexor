package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Intellexie/intellexor/internal/config"
	"github.com/Intellexie/intellexor/internal/store"
)

// Settings-dialog endpoints: list the config files that apply to the working
// directory, show which file each configured provider/agent lives in, and
// write/remove single provider/agent entries into a chosen config file.

// configFileTarget is one config file the settings dialog can list or write.
type configFileTarget struct {
	Path   string `json:"path"`
	Scope  string `json:"scope"` // "global" | "project"
	Exists bool   `json:"exists"`
}

// configTargets lists every config file that applies to the directory in
// merge-precedence order (global candidates, then project files root-most
// first), plus the two canonical create-if-missing targets: the primary
// global config and the worktree root. Write requests are validated against
// this list, so only known config locations can be modified.
func configTargets(directory, worktree string) []configFileTarget {
	var out []configFileTarget
	seen := map[string]bool{}
	add := func(path, scope string, force bool) {
		if path == "" || seen[path] {
			return
		}
		_, err := os.Stat(path)
		if err != nil && !force {
			return
		}
		seen[path] = true
		out = append(out, configFileTarget{Path: path, Scope: scope, Exists: err == nil})
	}
	// global: the primary writable config first, then any other existing candidates
	add(filepath.Join(store.ConfigDir(), "intellexor.json"), "global", true)
	for _, p := range config.GlobalCandidates() {
		add(p, "global", false)
	}
	// project chain, root-most first (closest file wins)
	for _, p := range config.ProjectFiles(directory) {
		add(p, "project", false)
	}
	// the worktree root is always offered as a create-new location
	add(filepath.Join(worktree, "intellexor.json"), "project", true)
	return out
}

func (s *Server) configFiles(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	writeJSON(w, 200, map[string]any{"files": configTargets(in.Directory, in.Worktree)})
	return nil
}

// toRawMap round-trips a typed config struct through JSON so the API returns
// exactly the keys the config file carries (omitempty respected).
func toRawMap(v any) map[string]any {
	m := map[string]any{}
	b, err := json.Marshal(v)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

// configEntries returns the providers and agents defined in config files,
// each with the file that wins merge precedence for it, plus the built-in
// agents and the merged default model.
func (s *Server) configEntries(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	type entry struct {
		File   string         `json:"file"`
		Config map[string]any `json:"config"`
	}
	providers := map[string]*entry{}
	agents := map[string]*entry{}
	for _, t := range configTargets(in.Directory, in.Worktree) {
		if !t.Exists {
			continue
		}
		info, err := config.LoadFile(t.Path)
		if err != nil {
			continue
		}
		for id, p := range info.Provider {
			providers[id] = &entry{File: t.Path, Config: toRawMap(p)}
		}
		for id, a := range info.Agent {
			agents[id] = &entry{File: t.Path, Config: toRawMap(a)}
		}
	}
	builtin := []map[string]any{}
	for _, a := range in.Agents() {
		builtin = append(builtin, map[string]any{
			"name": a.Name, "description": a.Description, "mode": a.Mode,
			"color": a.Color, "hidden": a.Hidden,
		})
	}
	writeJSON(w, 200, map[string]any{
		"providers":     providers,
		"agents":        agents,
		"builtin":       builtin,
		"default_model": mergedConfig(in.Directory).Model,
	})
	return nil
}

// writeConfigEntry sets (value != nil) or removes (value == nil) a single
// provider/agent entry inside the given config file, creating the file if
// needed. The path must be one of the known config targets.
func writeConfigEntry(directory, worktree, file, section, id string, value map[string]any) error {
	allowed := map[string]bool{}
	for _, t := range configTargets(directory, worktree) {
		allowed[t.Path] = true
	}
	if !allowed[file] {
		return fmt.Errorf("%q is not a known config file for this project", file)
	}
	doc := map[string]any{}
	if b, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal([]byte(config.StripJSONC(string(b))), &doc)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	sectionMap, _ := doc[section].(map[string]any)
	if sectionMap == nil {
		sectionMap = map[string]any{}
	}
	if value == nil {
		delete(sectionMap, id)
	} else {
		sectionMap[id] = value
	}
	if len(sectionMap) == 0 {
		delete(doc, section)
	} else {
		doc[section] = sectionMap
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, append(b, '\n'), 0o644)
}

func (s *Server) putConfigEntry(w http.ResponseWriter, r *http.Request, section, param string) *apiError {
	in := s.Instance(directoryOf(r))
	var body struct {
		File  string         `json:"file"`
		Entry map[string]any `json:"entry"`
	}
	if e := decodeBody(r, &body); e != nil {
		return e
	}
	if body.File == "" {
		return badRequest("file is required")
	}
	if body.Entry == nil {
		return badRequest("entry is required")
	}
	id := r.PathValue(param)
	if id == "" {
		return badRequest("id is required")
	}
	if err := writeConfigEntry(in.Directory, in.Worktree, body.File, section, id, body.Entry); err != nil {
		return badRequest(err.Error())
	}
	config.InvalidateGlobal()
	writeTrue(w)
	return nil
}

func (s *Server) deleteConfigEntry(w http.ResponseWriter, r *http.Request, section, param string) *apiError {
	in := s.Instance(directoryOf(r))
	file := r.URL.Query().Get("file")
	if file == "" {
		return badRequest("file query parameter is required")
	}
	if err := writeConfigEntry(in.Directory, in.Worktree, file, section, r.PathValue(param), nil); err != nil {
		return badRequest(err.Error())
	}
	config.InvalidateGlobal()
	writeTrue(w)
	return nil
}

func (s *Server) putConfigProvider(w http.ResponseWriter, r *http.Request) *apiError {
	return s.putConfigEntry(w, r, "provider", "providerID")
}

func (s *Server) deleteConfigProvider(w http.ResponseWriter, r *http.Request) *apiError {
	return s.deleteConfigEntry(w, r, "provider", "providerID")
}

func (s *Server) putConfigAgent(w http.ResponseWriter, r *http.Request) *apiError {
	return s.putConfigEntry(w, r, "agent", "agentID")
}

func (s *Server) deleteConfigAgent(w http.ResponseWriter, r *http.Request) *apiError {
	return s.deleteConfigEntry(w, r, "agent", "agentID")
}
