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

// configEntries returns the providers and agents defined in config files,
// each with the file that wins merge precedence for it, plus the built-in
// agents and the merged default model. Entries are the raw JSON objects from
// the files (not the typed structs) so opencode-only fields survive an
// edit round-trip.
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
		doc := readConfigDoc(t.Path)
		for section, into := range map[string]map[string]*entry{"provider": providers, "agent": agents} {
			m, ok := doc[section].(map[string]any)
			if !ok {
				continue
			}
			for id, v := range m {
				if cm, ok := v.(map[string]any); ok {
					into[id] = &entry{File: t.Path, Config: cm}
				}
			}
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

// readConfigDoc parses a config file (JSONC tolerated) into a raw map.
func readConfigDoc(file string) map[string]any {
	doc := map[string]any{}
	if b, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal([]byte(config.StripJSONC(string(b))), &doc)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc
}

// knownConfigFile reports whether file is one of the config locations the
// settings dialog may write to for this project.
func knownConfigFile(directory, worktree, file string) bool {
	for _, t := range configTargets(directory, worktree) {
		if t.Path == file {
			return true
		}
	}
	return false
}

// writeConfigFile marshals doc and writes it back to file.
func writeConfigFile(file string, doc map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, append(b, '\n'), 0o644)
}

// writeConfigPath sets (value != nil) or removes (value == nil) the value at
// a nested path inside a config file, creating intermediate objects as
// needed. After a removal, intermediate objects left empty on the path are
// pruned (e.g. an empty "models" map), matching how writeConfigEntry drops
// empty sections.
func writeConfigPath(file string, path []string, value any) error {
	doc := readConfigDoc(file)
	type level struct {
		m   map[string]any
		key string
	}
	var stack []level
	cur := doc
	for _, key := range path[:len(path)-1] {
		next, _ := cur[key].(map[string]any)
		if next == nil {
			if value == nil {
				return nil // nothing to remove along the path
			}
			next = map[string]any{}
			cur[key] = next
		}
		stack = append(stack, level{cur, key})
		cur = next
	}
	leaf := path[len(path)-1]
	if value == nil {
		delete(cur, leaf)
		for i := len(stack) - 1; i >= 0; i-- {
			l := stack[i]
			child, _ := l.m[l.key].(map[string]any)
			if child == nil || len(child) > 0 {
				break
			}
			delete(l.m, l.key)
		}
	} else {
		cur[leaf] = value
	}
	return writeConfigFile(file, doc)
}

// writeConfigEntry sets (value != nil) or removes (value == nil) a single
// provider/agent entry inside the given config file, creating the file if
// needed. The path must be one of the known config targets.
func writeConfigEntry(directory, worktree, file, section, id string, value map[string]any) error {
	if !knownConfigFile(directory, worktree, file) {
		return fmt.Errorf("%q is not a known config file for this project", file)
	}
	// box into any carefully: a nil map passed as-is would be a non-nil
	// interface and writeConfigPath would write a JSON null instead of deleting
	var v any
	if value != nil {
		v = value
	}
	return writeConfigPath(file, []string{section, id}, v)
}

// openaiCompatibleNPM is the opencode SDK package for OpenAI-compatible
// providers; it doubles as the provider "type" in the settings UI.
const openaiCompatibleNPM = "@ai-sdk/openai-compatible"

// validateProviderEntry enforces the provider type. opencode carries the type
// in the "npm" field; for now only openai-compatible is supported, so a
// missing npm defaults to it and any other value is rejected.
func validateProviderEntry(entry map[string]any) *apiError {
	if npm, _ := entry["npm"].(string); npm == "" {
		entry["npm"] = openaiCompatibleNPM
	} else if npm != openaiCompatibleNPM {
		return badRequest(fmt.Sprintf("unsupported provider type %q: only %q (openai-compatible) is supported", npm, openaiCompatibleNPM))
	}
	return nil
}

func (s *Server) putConfigEntry(w http.ResponseWriter, r *http.Request, section, param string, validate func(map[string]any) *apiError) *apiError {
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
	if validate != nil {
		if e := validate(body.Entry); e != nil {
			return e
		}
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
	return s.putConfigEntry(w, r, "provider", "providerID", validateProviderEntry)
}

func (s *Server) deleteConfigProvider(w http.ResponseWriter, r *http.Request) *apiError {
	return s.deleteConfigEntry(w, r, "provider", "providerID")
}

// putConfigProviderModel sets a single model inside a provider entry,
// preserving the rest of the provider (and any opencode-only fields).
func (s *Server) putConfigProviderModel(w http.ResponseWriter, r *http.Request) *apiError {
	return s.putProviderModelEntry(w, r, true)
}

func (s *Server) deleteConfigProviderModel(w http.ResponseWriter, r *http.Request) *apiError {
	return s.putProviderModelEntry(w, r, false)
}

func (s *Server) putProviderModelEntry(w http.ResponseWriter, r *http.Request, set bool) *apiError {
	in := s.Instance(directoryOf(r))
	providerID := r.PathValue("providerID")
	modelID := r.PathValue("modelID")
	if providerID == "" || modelID == "" {
		return badRequest("providerID and modelID are required")
	}
	var file string
	var value any
	if set {
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
		file = body.File
		value = body.Entry
	} else {
		file = r.URL.Query().Get("file")
		if file == "" {
			return badRequest("file query parameter is required")
		}
	}
	if !knownConfigFile(in.Directory, in.Worktree, file) {
		return badRequest(fmt.Sprintf("%q is not a known config file for this project", file))
	}
	if err := writeConfigPath(file, []string{"provider", providerID, "models", modelID}, value); err != nil {
		return badRequest(err.Error())
	}
	config.InvalidateGlobal()
	writeTrue(w)
	return nil
}

func (s *Server) putConfigAgent(w http.ResponseWriter, r *http.Request) *apiError {
	return s.putConfigEntry(w, r, "agent", "agentID", nil)
}

func (s *Server) deleteConfigAgent(w http.ResponseWriter, r *http.Request) *apiError {
	return s.deleteConfigEntry(w, r, "agent", "agentID")
}
