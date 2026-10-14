package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Mirrors opencode's file storage layout under a private data dir.
type Store struct {
	mu   sync.Mutex
	Root string
}

var mu sync.Mutex
var cached *Store

func DataDir() string {
	mu.Lock()
	defer mu.Unlock()
	if cached != nil {
		return cached.Root
	}
	if v := os.Getenv("INTELLEXOR_DATA"); v != "" {
		cached = &Store{Root: v}
		return v
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	cached = &Store{Root: filepath.Join(base, "intellexor")}
	return cached.Root
}

func StateDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "intellexor")
}

func ConfigDir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "intellexor")
}

// ResetDataDirForTest clears the cached data dir so a new INTELLEXOR_DATA env
// value (set by a test) takes effect.
func ResetDataDirForTest() {
	mu.Lock()
	cached = nil
	mu.Unlock()
}

// opencode-compatible locations we read auth from, in priority order
func AuthSearchPaths() []string {
	home, _ := os.UserHomeDir()
	paths := []string{filepath.Join(DataDir(), "auth.json")}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" && home != "" {
		base = filepath.Join(home, ".local", "share")
	}
	if base != "" {
		paths = append(paths, filepath.Join(base, "opencode", "auth.json"))
	}
	return paths
}

func ProjectID(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	root := GitRoot(abs)
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:])[:12]
}

func GitRoot(dir string) string {
	cur := dir
	for {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return dir
		}
		cur = parent
	}
}

func (s *Store) path(parts ...string) string {
	return filepath.Join(append([]string{s.Root, "storage"}, parts...)...)
}

func (s *Store) WriteJSON(file string, v any) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

func ReadJSON(file string, v any) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (s *Store) Delete(file string) error { return os.Remove(file) }

func (s *Store) SessionFile(projectID, sessionID string) string {
	return s.path("session", projectID, sessionID, "info.json")
}

func (s *Store) SessionDir(projectID, sessionID string) string {
	return s.path("session", projectID, sessionID)
}

func (s *Store) MessageFile(projectID, sessionID, messageID string) string {
	return s.path("session", projectID, sessionID, "message", messageID+".json")
}

func (s *Store) PartFile(projectID, sessionID, messageID, partID string) string {
	return s.path("session", projectID, sessionID, "message_part", messageID, partID+".json")
}

func (s *Store) ListMessages(projectID, sessionID string) ([]string, error) {
	dir := filepath.Join(s.SessionDir(projectID, sessionID), "message")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *Store) ListParts(projectID, sessionID, messageID string) ([]string, error) {
	dir := filepath.Join(s.SessionDir(projectID, sessionID), "message_part", messageID)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *Store) KV(domain, key string) string {
	return s.path(domain, key+".json")
}

func (s *Store) WriteToolOutput(id string, content []byte) string {
	dir := filepath.Join(s.Root, "tool-output")
	_ = os.MkdirAll(dir, 0o755)
	file := filepath.Join(dir, "tool_"+id)
	_ = os.WriteFile(file, content, 0o644)
	return file
}
