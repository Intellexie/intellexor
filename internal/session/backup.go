package session

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"intellexor/internal/store"
)

// FileBackup is the pre-edit state of a single file, recorded before an
// edit/write/apply_patch tool modifies it. Backups are keyed by message so a
// revert-to-message can restore the workspace to the state it had before that
// message's edits (opencode does the same with git snapshots; Intellexor keeps
// plain copies under the data dir instead).
type FileBackup struct {
	Path    string `json:"path"`
	Existed bool   `json:"existed"`
	Mode    uint32 `json:"mode,omitempty"`
	Content string `json:"content,omitempty"`
}

func (m *Manager) backupDir(projectID, sessionID string) string {
	return filepath.Join(m.st.Root, "storage", "file_backup", projectID, sessionID)
}

func backupFileName(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:8]) + ".json"
}

// BackupFile records the current content of path (or its absence) before the
// given message modifies it. Only the first backup per (message, path) is
// kept, so the state before the message's first edit to that file wins.
func (m *Manager) BackupFile(s *Session, messageID, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	file := filepath.Join(m.backupDir(s.ProjectID, s.ID), messageID, backupFileName(path))
	if _, err := os.Stat(file); err == nil {
		return nil
	}
	b := FileBackup{Path: path}
	if fi, err := os.Stat(path); err == nil {
		b.Existed = true
		b.Mode = uint32(fi.Mode().Perm())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		b.Content = string(data)
	}
	return m.st.WriteJSON(file, b)
}

// TouchedFiles returns the files backed up for the given messages. messageIDs
// must be in chronological order; the first backup seen for a path wins.
func (m *Manager) TouchedFiles(s *Session, messageIDs []string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, mid := range messageIDs {
		dir := filepath.Join(m.backupDir(s.ProjectID, s.ID), mid)
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			var b FileBackup
			if store.ReadJSON(filepath.Join(dir, e.Name()), &b) != nil || b.Path == "" {
				continue
			}
			if seen[b.Path] {
				continue
			}
			seen[b.Path] = true
			out = append(out, b.Path)
		}
	}
	return out
}

// RestoreBackups puts files back to the state recorded before the given
// messages ran, oldest message first. A file whose earliest backup recorded
// non-existence is deleted. Returns the paths restored.
func (m *Manager) RestoreBackups(s *Session, messageIDs []string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	byPath := map[string]FileBackup{}
	var paths []string
	for _, mid := range messageIDs {
		dir := filepath.Join(m.backupDir(s.ProjectID, s.ID), mid)
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			var b FileBackup
			if store.ReadJSON(filepath.Join(dir, e.Name()), &b) != nil || b.Path == "" {
				continue
			}
			if _, ok := byPath[b.Path]; !ok {
				byPath[b.Path] = b
				paths = append(paths, b.Path)
			}
		}
	}
	var restored []string
	for _, p := range paths {
		b := byPath[p]
		if b.Existed {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return restored, err
			}
			mode := os.FileMode(b.Mode)
			if mode == 0 {
				mode = 0o644
			}
			if err := os.WriteFile(p, []byte(b.Content), mode); err != nil {
				return restored, err
			}
		} else {
			_ = os.Remove(p)
		}
		restored = append(restored, p)
	}
	return restored, nil
}

func (m *Manager) snapshotFile(projectID, sessionID string) string {
	return filepath.Join(m.st.Root, "storage", "revert_snapshot", projectID, sessionID+".json")
}

// SnapshotFiles captures the current content of the given paths so a later
// unrevert can put them back. Returns the snapshot file path.
func (m *Manager) SnapshotFiles(s *Session, paths []string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var snap []FileBackup
	for _, p := range paths {
		b := FileBackup{Path: p}
		if fi, err := os.Stat(p); err == nil {
			b.Existed = true
			b.Mode = uint32(fi.Mode().Perm())
			data, err := os.ReadFile(p)
			if err != nil {
				return "", err
			}
			b.Content = string(data)
		}
		snap = append(snap, b)
	}
	file := m.snapshotFile(s.ProjectID, s.ID)
	return file, m.st.WriteJSON(file, snap)
}

// LoadSnapshot reads the revert-time file snapshot, if any.
func (m *Manager) LoadSnapshot(s *Session) ([]FileBackup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var snap []FileBackup
	if err := store.ReadJSON(m.snapshotFile(s.ProjectID, s.ID), &snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// RestoreSnapshot writes the snapshot contents back to disk.
func (m *Manager) RestoreSnapshot(s *Session, snap []FileBackup) error {
	for _, b := range snap {
		if b.Existed {
			if err := os.MkdirAll(filepath.Dir(b.Path), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(b.Mode)
			if mode == 0 {
				mode = 0o644
			}
			if err := os.WriteFile(b.Path, []byte(b.Content), mode); err != nil {
				return err
			}
		} else {
			_ = os.Remove(b.Path)
		}
	}
	return nil
}

// DeleteSnapshot removes the revert-time snapshot.
func (m *Manager) DeleteSnapshot(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = os.Remove(m.snapshotFile(s.ProjectID, s.ID))
}
