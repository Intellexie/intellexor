package app

import (
	"encoding/json"
	"fmt"

	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

// ExportFormat identifies the payload shape; ImportSession rejects files
// with a different format value.
const ExportFormat = "intellexor.session"

// ExportMessage is one entry of an export payload: the message info (user or
// assistant) plus its parts. Shape-identical to session.WithParts, but the
// info stays raw so import can re-decode it by role.
type ExportMessage struct {
	Info  json.RawMessage `json:"info"`
	Parts []session.Part  `json:"parts"`
}

// ExportPayload is the portable JSON document produced by
// GET /session/:id/export and consumed by POST /session/import. It carries
// the whole conversation — every message with all of its parts (tool calls,
// reasoning, patches, attachments) — plus the todo and diff state, so an
// imported session continues exactly where the exported one left off.
type ExportPayload struct {
	Format     string           `json:"format"`
	Version    int              `json:"version"`
	ExportedAt int64            `json:"exportedAt"`
	Session    *session.Session `json:"session"`
	Messages   []ExportMessage  `json:"messages"`
	Todos      json.RawMessage  `json:"todos,omitempty"`
	Diffs      json.RawMessage  `json:"diffs,omitempty"`
}

// ExportSession builds the payload for a session: the session info, every
// visible message with its parts, and todo/diff state. Like Fork, the
// session is re-read from disk first so a live object is not exported, and
// reverted (hidden) messages are not included — the export is the session
// as it is, not as it was.
func (i *Instance) ExportSession(s *session.Session) (*ExportPayload, error) {
	if fresh, err := i.Mgr.Find(s.ID); err == nil {
		s = fresh
	}
	msgs, err := i.Mgr.LoadMessages(s)
	if err != nil {
		return nil, err
	}
	out := &ExportPayload{
		Format:     ExportFormat,
		Version:    1,
		ExportedAt: util.Now(),
		Session:    s,
		Messages:   make([]ExportMessage, 0, len(msgs)),
	}
	for _, m := range msgs {
		raw, err := json.Marshal(m.Info)
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, ExportMessage{Info: raw, Parts: m.Parts})
	}
	// todo and diff state ride along (best effort, like Fork's copy)
	_ = i.Mgr.ReadKV("todo", s.ID, &out.Todos)
	_ = i.Mgr.ReadKV("session_diff", s.ID, &out.Diffs)
	return out, nil
}

// ImportSession creates a brand-new session from an exported payload: a new
// id, slug and timestamps, but the original title, agent, model, stats and
// the complete message history with every tool call and part. Message and
// part ids are kept (they are unique per session directory and their
// time-ordered prefixes preserve the timeline order); only the sessionID
// references are rewritten. The result is a fully loaded session in the
// backend — the UI renders it through the normal tail window.
func (i *Instance) ImportSession(p *ExportPayload) (*session.Session, error) {
	if p == nil || p.Session == nil {
		return nil, fmt.Errorf("invalid export: missing session")
	}
	if p.Format != "" && p.Format != ExportFormat {
		return nil, fmt.Errorf("invalid export format: %s", p.Format)
	}

	src := p.Session
	dup := *src
	dup.ID = ""
	dup.Slug = ""
	dup.ProjectID = store.ProjectID(i.Directory)
	dup.Directory = i.Directory
	dup.ParentID = ""
	dup.Share = nil
	dup.Revert = nil
	dup.Time = session.SessionTime{}
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

	for _, em := range p.Messages {
		info, err := decodeImportedMessage(em.Info)
		if err != nil {
			return nil, fmt.Errorf("invalid message in export: %w", err)
		}
		switch t := info.(type) {
		case session.UserMessage:
			t.SessionID = dup.ID
			info = t
		case session.AssistantMessage:
			t.SessionID = dup.ID
			info = t
		default:
			return nil, fmt.Errorf("invalid message role in export")
		}
		if err := i.Mgr.SaveMessage(&dup, info); err != nil {
			return nil, err
		}
		for _, part := range em.Parts {
			part.SessionID = dup.ID
			if part.MessageID == "" {
				part.MessageID = messageIDOf(info)
			}
			if err := i.Mgr.SavePart(&dup, part); err != nil {
				return nil, err
			}
		}
	}

	// restore todo and diff state when the export carries them
	if len(p.Todos) > 0 && string(p.Todos) != "null" {
		_ = i.Mgr.WriteKV("todo", dup.ID, p.Todos)
	}
	if len(p.Diffs) > 0 && string(p.Diffs) != "null" {
		_ = i.Mgr.WriteKV("session_diff", dup.ID, p.Diffs)
	}

	i.Hub.Publish("session.created", map[string]any{"sessionID": dup.ID, "info": dup})
	i.Hub.Publish("session.updated", map[string]any{"sessionID": dup.ID, "info": dup})
	return &dup, nil
}

// decodeImportedMessage re-decodes a raw message info by role, mirroring
// Manager.decodeMessage's user/assistant split — but strict: only the two
// chat roles are accepted, anything else invalidates the export.
func decodeImportedMessage(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty message")
	}
	var probe struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	switch probe.Role {
	case "assistant":
		var a session.AssistantMessage
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		return a, nil
	case "user":
		var u session.UserMessage
		if err := json.Unmarshal(raw, &u); err != nil {
			return nil, err
		}
		return u, nil
	default:
		return nil, fmt.Errorf("unsupported role %q", probe.Role)
	}
}
