package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"intellexor/internal/store"
	"intellexor/internal/util"
)

var ErrNotFound = errors.New("not found")

type Manager struct {
	st *store.Store
	mu sync.Mutex
}

func NewManager() *Manager { return &Manager{st: &store.Store{Root: store.DataDir()}} }

func (m *Manager) Create(s *Session) error {
	s.ID = util.ID("ses")
	s.Slug = util.Slug(8)
	now := util.Now()
	s.Time.Created = now
	s.Time.Updated = now
	if s.Version == "" {
		s.Version = Version
	}
	if s.Title == "" {
		s.Title = "New session - " + iso(now)
	}
	if s.Cost == 0 {
		s.Cost = 0
	}
	return m.st.WriteJSON(m.st.SessionFile(s.ProjectID, s.ID), s)
}

func iso(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

func (m *Manager) Save(s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.Time.Updated = util.Now()
	return m.st.WriteJSON(m.st.SessionFile(s.ProjectID, s.ID), s)
}

func (m *Manager) findFile(sessionID string) (string, string, error) {
	root := filepath.Join(m.st.Root, "storage", "session")
	projects, err := os.ReadDir(root)
	if err != nil {
		return "", "", ErrNotFound
	}
	for _, p := range projects {
		f := filepath.Join(root, p.Name(), sessionID, "info.json")
		if _, err := os.Stat(f); err == nil {
			var s Session
			if err := store.ReadJSON(f, &s); err != nil {
				continue
			}
			return p.Name(), s.ProjectID, nil
		}
	}
	return "", "", ErrNotFound
}

func (m *Manager) Find(sessionID string) (*Session, error) {
	_, projectID, err := m.findFile(sessionID)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := store.ReadJSON(m.st.SessionFile(projectID, sessionID), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (m *Manager) List(dir string, scopeProject bool, projectID string) []*Session {
	var out []*Session
	root := filepath.Join(m.st.Root, "storage", "session")
	projects, _ := os.ReadDir(root)
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		if !scopeProject && p.Name() != projectID {
			continue
		}
		if scopeProject && projectID != "" && p.Name() != projectID {
			continue
		}
		sessions, _ := os.ReadDir(filepath.Join(root, p.Name()))
		for _, s := range sessions {
			if !s.IsDir() {
				continue
			}
			var sess Session
			if err := store.ReadJSON(filepath.Join(root, p.Name(), s.Name(), "info.json"), &sess); err != nil {
				continue
			}
			if !scopeProject && sess.Directory != dir {
				continue
			}
			cp := sess
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Updated > out[j].Time.Updated })
	return out
}

func (m *Manager) Delete(s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return os.RemoveAll(m.st.SessionDir(s.ProjectID, s.ID))
}

// ---- messages ----

type LoadedSession struct {
	Session  *Session
	Messages []WithParts
}

func (m *Manager) LoadMessages(s *Session) ([]WithParts, error) {
	return m.loadMessages(s, false)
}

// LoadMessagesAll loads every stored message, including reverted ones.
func (m *Manager) LoadMessagesAll(s *Session) ([]WithParts, error) {
	return m.loadMessages(s, true)
}

func (m *Manager) loadMessages(s *Session, includeReverted bool) ([]WithParts, error) {
	ids, err := m.st.ListMessages(s.ProjectID, s.ID)
	if err != nil {
		return nil, err
	}
	var out []WithParts
	for _, id := range ids {
		var info json.RawMessage
		if err := store.ReadJSON(m.st.MessageFile(s.ProjectID, s.ID, id), &info); err != nil {
			continue
		}
		var probe struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(info, &probe)
		var v any
		switch probe.Role {
		case "assistant":
			var a AssistantMessage
			if err := json.Unmarshal(info, &a); err != nil {
				continue
			}
			v = a
		default:
			var u UserMessage
			if err := json.Unmarshal(info, &u); err != nil {
				continue
			}
			v = u
		}
		partIDs, _ := m.st.ListParts(s.ProjectID, s.ID, id)
		var parts []Part
		for _, pid := range partIDs {
			var p Part
			if err := store.ReadJSON(m.st.PartFile(s.ProjectID, s.ID, id, pid), &p); err != nil {
				continue
			}
			parts = append(parts, p)
		}
		sort.SliceStable(parts, func(i, j int) bool { return parts[i].ID < parts[j].ID })
		out = append(out, WithParts{Info: v, Parts: parts})
	}
	// a revert hides everything from the reverted message onward; the files
	// stay on disk until the revert is undone or a new prompt purges them
	if !includeReverted && s.Revert != nil && s.Revert.MessageID != "" {
		for j, m := range out {
			if messageID(m.Info) == s.Revert.MessageID {
				out = out[:j]
				break
			}
		}
	}
	return out, nil
}

func (m *Manager) SaveMessage(s *Session, info any) error {
	return m.st.WriteJSON(m.st.MessageFile(s.ProjectID, s.ID, messageID(info)), info)
}

func messageID(info any) string {
	switch t := info.(type) {
	case UserMessage:
		return t.ID
	case *UserMessage:
		return t.ID
	case AssistantMessage:
		return t.ID
	case *AssistantMessage:
		return t.ID
	}
	return ""
}

func (m *Manager) SavePart(s *Session, p Part) error {
	return m.st.WriteJSON(m.st.PartFile(s.ProjectID, s.ID, p.MessageID, p.ID), p)
}

func (m *Manager) DeleteMessage(s *Session, messageID string) error {
	_ = os.Remove(m.st.MessageFile(s.ProjectID, s.ID, messageID))
	return os.RemoveAll(filepath.Join(m.st.SessionDir(s.ProjectID, s.ID), "message_part", messageID))
}

func (m *Manager) DeletePart(s *Session, messageID, partID string) error {
	return os.Remove(m.st.PartFile(s.ProjectID, s.ID, messageID, partID))
}

func (m *Manager) Children(parentID string) []*Session {
	var out []*Session
	for _, s := range m.ListAll() {
		if s.ParentID == parentID {
			out = append(out, s)
		}
	}
	return out
}

func (m *Manager) ListAll() []*Session {
	return m.List("", true, "")
}

// ---- KV (todos, session diff) ----

func (m *Manager) WriteKV(domain, key string, v any) error {
	return m.st.WriteJSON(m.st.KV(domain, key), v)
}

func (m *Manager) ReadKV(domain, key string, v any) error {
	return store.ReadJSON(m.st.KV(domain, key), v)
}
