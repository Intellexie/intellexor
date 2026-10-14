package app

import (
	"encoding/json"
	"fmt"

	"intellexor/internal/session"
)

// Fork clones a session into a new one: the session info, its visible
// messages and their parts, plus todo and diff state. If messageID is
// non-empty, only the history up to and including that message is copied,
// so the UI can fork from any point in the timeline. The copy starts clean
// (no revert state); file backups stay with the original since both
// sessions share the same working directory.
//
// The source session is only read, never mutated, so no session lock is
// taken — forking a running session snapshots it as-is instead of aborting
// the run. All storage writes are atomic file renames, so the worst case is
// a snapshot that misses an in-flight message.
func (i *Instance) Fork(s *session.Session, messageID string) (*session.Session, error) {
	if fresh, err := i.Mgr.Find(s.ID); err == nil {
		s = fresh
	}

	msgs, err := i.Mgr.LoadMessages(s)
	if err != nil {
		return nil, err
	}
	if messageID != "" {
		idx := -1
		for j, m := range msgs {
			if messageIDOf(m.Info) == messageID {
				idx = j
				break
			}
		}
		if idx < 0 {
			return nil, fmt.Errorf("Message not found: %s", messageID)
		}
		msgs = msgs[:idx+1]
	}

	dup := *s
	dup.ID = ""
	dup.Title = s.Title + " (forked)"
	dup.Time = session.SessionTime{}
	dup.Revert = nil
	if dup.Metadata != nil {
		md := make(map[string]any, len(dup.Metadata))
		for k, v := range dup.Metadata {
			md[k] = v
		}
		dup.Metadata = md
	}
	if err := i.Mgr.Create(&dup); err != nil {
		return nil, err
	}

	for _, m := range msgs {
		info := m.Info
		switch t := info.(type) {
		case session.UserMessage:
			t.SessionID = dup.ID
			info = t
		case session.AssistantMessage:
			t.SessionID = dup.ID
			info = t
		}
		if err := i.Mgr.SaveMessage(&dup, info); err != nil {
			return nil, err
		}
		for _, p := range m.Parts {
			p.SessionID = dup.ID
			if err := i.Mgr.SavePart(&dup, p); err != nil {
				return nil, err
			}
		}
	}

	// carry over todo and diff state (best effort)
	for _, domain := range []string{"todo", "session_diff"} {
		var raw json.RawMessage
		if err := i.Mgr.ReadKV(domain, s.ID, &raw); err == nil && len(raw) > 0 {
			_ = i.Mgr.WriteKV(domain, dup.ID, raw)
		}
	}

	i.Hub.Publish("session.created", map[string]any{"sessionID": dup.ID, "info": dup})
	return &dup, nil
}
