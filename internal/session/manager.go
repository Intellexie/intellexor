package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
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

// Update atomically re-reads the session from disk, applies fn, and writes it
// back — all under the manager lock. Use it for background writers that hold
// no live session object (title generation, renames): a plain Find+Save can
// read a pre-prompt copy and clobber tokens/cost the prompt loop saved in
// between, because Save is a blind write of the passed object.
func (m *Manager) Update(sessionID string, fn func(*Session)) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, projectID, err := m.findFile(sessionID)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := store.ReadJSON(m.st.SessionFile(projectID, sessionID), &s); err != nil {
		return nil, err
	}
	fn(&s)
	s.Time.Updated = util.Now()
	if err := m.st.WriteJSON(m.st.SessionFile(projectID, sessionID), &s); err != nil {
		return nil, err
	}
	return &s, nil
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
	return m.loadMessages(s, false, 0)
}

// LoadMessagesAll loads every stored message, including reverted ones.
func (m *Manager) LoadMessagesAll(s *Session) ([]WithParts, error) {
	return m.loadMessages(s, true, 0)
}

// LoadMessagesTail loads the last visible display units, trimming whole
// messages from the front. The budget counts chat content, not message
// records: a user prompt, a response text, a thinking block and a tool call
// are each one unit (see displayUnits). A message is never split across the
// window edge, so the result can exceed the limit by the oldest kept
// message's units. limit <= 0 loads everything.
func (m *Manager) LoadMessagesTail(s *Session, limit int) ([]WithParts, error) {
	return m.loadMessages(s, false, limit)
}

func (m *Manager) loadMessages(s *Session, includeReverted bool, limit int) ([]WithParts, error) {
	ids, err := m.st.ListMessages(s.ProjectID, s.ID)
	if err != nil {
		return nil, err
	}
	// a revert hides everything from the reverted message onward; the files
	// stay on disk until the revert is undone or a new prompt purges them.
	// Trimming the ID list up front keeps the tail limit relative to what is
	// actually visible and skips decoding hidden messages entirely.
	if !includeReverted && s.Revert != nil && s.Revert.MessageID != "" {
		for j, id := range ids {
			if id == s.Revert.MessageID {
				ids = ids[:j]
				break
			}
		}
	}
	// with a limit the walk starts at the newest message and moves back until
	// the unit budget is spent, so only the returned window is ever decoded
	var out []WithParts
	if limit > 0 {
		units := 0
		for i := len(ids) - 1; i >= 0 && units < limit; i-- {
			wp, ok := m.decodeMessage(s, ids[i])
			if !ok {
				continue
			}
			units += displayUnits(wp)
			out = append(out, wp)
		}
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		return out, nil
	}
	for _, id := range ids {
		wp, ok := m.decodeMessage(s, id)
		if !ok {
			continue
		}
		out = append(out, wp)
	}
	return out, nil
}

// decodeMessage reads one stored message with its parts, sorted by part ID.
func (m *Manager) decodeMessage(s *Session, id string) (WithParts, bool) {
	var info json.RawMessage
	if err := store.ReadJSON(m.st.MessageFile(s.ProjectID, s.ID, id), &info); err != nil {
		return WithParts{}, false
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
			return WithParts{}, false
		}
		v = a
	default:
		var u UserMessage
		if err := json.Unmarshal(info, &u); err != nil {
			return WithParts{}, false
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
	return WithParts{Info: v, Parts: parts}, true
}

// displayUnits is the window accounting rule, mirroring what the web UI
// renders: a user prompt counts 1, and an assistant message contributes one
// unit per response text, thinking block and tool call. Auxiliary parts
// (step markers, patches, file chips, ...) and content that renders as
// nothing (synthetic/ignored text) are free, so they never push visible
// messages out of the window.
func displayUnits(wp WithParts) int {
	if _, ok := wp.Info.(UserMessage); ok {
		for _, p := range wp.Parts {
			if p.Type == "file" || (p.Type == "text" && !p.Synthetic) {
				return 1
			}
		}
		return 0
	}
	n := 0
	for _, p := range wp.Parts {
		switch p.Type {
		case "text":
			if !p.Synthetic && !p.Ignored {
				n++
			}
		case "reasoning", "tool":
			n++
		}
	}
	return n
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
