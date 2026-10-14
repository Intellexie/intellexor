package app

import (
	"fmt"
	"os"

	"github.com/Intellexie/intellexor/internal/session"
)

// refresh re-reads the session from disk so mutations start from the current
// stored state (the caller's copy may have been resolved before waiting on
// the session lock).
func (i *Instance) refresh(s *session.Session) {
	if fresh, err := i.Mgr.Find(s.ID); err == nil {
		*s = *fresh
	}
}

// stepTotal is the context footprint of a single request: input + output +
// reasoning + cache read/write. Per-message Tokens hold exactly one step, so
// the last assistant message's total approximates the current context size.
func stepTotal(t session.Tokens) float64 {
	return t.Input + t.Output + t.Reasoning + t.Cache.Read + t.Cache.Write
}

// recomputeContext re-derives s.Context from the last visible assistant
// message. Used after a revert/unrevert, where messages appear or disappear
// without a model call to refresh the estimate.
func (i *Instance) recomputeContext(s *session.Session) {
	msgs, err := i.Mgr.LoadMessages(s)
	if err != nil {
		return
	}
	for j := len(msgs) - 1; j >= 0; j-- {
		if a, ok := msgs[j].Info.(session.AssistantMessage); ok {
			s.Context = stepTotal(a.Tokens)
			return
		}
	}
	s.Context = 0
}

// Revert drops the conversation from messageID onward (inclusive) and restores
// the workspace files touched by those messages to their pre-edit state. The
// removed messages stay on disk (hidden from listings and LLM history) so
// Unrevert can bring them back; a new prompt purges them permanently.
func (i *Instance) Revert(s *session.Session, messageID string) (*session.Session, error) {
	l := i.lock(s.ID)
	l.Lock()
	defer l.Unlock()

	// can't rewind while the agent is mid-run
	i.Abort(s.ID)
	i.refresh(s)

	msgs, err := i.Mgr.LoadMessagesAll(s)
	if err != nil {
		return nil, err
	}
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

	var removed []string
	for _, m := range msgs[idx:] {
		removed = append(removed, messageIDOf(m.Info))
	}

	// restore files touched by the removed portion to their state before it
	touched := i.Mgr.TouchedFiles(s, removed)
	if len(touched) > 0 {
		// capture the current state first so unrevert can put it back
		if _, err := i.Mgr.SnapshotFiles(s, touched); err != nil {
			return nil, err
		}
		if _, err := i.Mgr.RestoreBackups(s, removed); err != nil {
			return nil, err
		}
	}

	s.Revert = &session.RevertState{MessageID: messageID}
	i.recomputeContext(s)
	if err := i.Mgr.Save(s); err != nil {
		return nil, err
	}
	i.Hub.Publish("session.updated", map[string]any{"sessionID": s.ID, "info": *s})
	return s, nil
}

// Unrevert restores the messages and file state captured by a previous Revert.
func (i *Instance) Unrevert(s *session.Session) (*session.Session, error) {
	l := i.lock(s.ID)
	l.Lock()
	defer l.Unlock()

	i.refresh(s)
	if s.Revert == nil {
		return s, nil
	}
	snap, err := i.Mgr.LoadSnapshot(s)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("no revert snapshot to restore")
	}
	if len(snap) > 0 {
		if err := i.Mgr.RestoreSnapshot(s, snap); err != nil {
			return nil, err
		}
	}
	i.Mgr.DeleteSnapshot(s)
	s.Revert = nil
	i.recomputeContext(s)
	if err := i.Mgr.Save(s); err != nil {
		return nil, err
	}
	i.Hub.Publish("session.updated", map[string]any{"sessionID": s.ID, "info": *s})
	return s, nil
}

// purgeReverted permanently deletes messages hidden by a revert. Called when
// a new prompt continues a reverted session (matching opencode: once you move
// on, the undone turns are gone and can no longer be restored).
func (i *Instance) purgeReverted(s *session.Session) {
	msgs, err := i.Mgr.LoadMessagesAll(s)
	if err != nil {
		return
	}
	idx := -1
	for j, m := range msgs {
		if messageIDOf(m.Info) == s.Revert.MessageID {
			idx = j
			break
		}
	}
	if idx >= 0 {
		for _, m := range msgs[idx:] {
			mid := messageIDOf(m.Info)
			if err := i.Mgr.DeleteMessage(s, mid); err == nil {
				i.Hub.Publish("message.removed", map[string]any{"sessionID": s.ID, "messageID": mid})
			}
		}
	}
	i.Mgr.DeleteSnapshot(s)
	s.Revert = nil
	_ = i.Mgr.Save(s)
	i.Hub.Publish("session.updated", map[string]any{"sessionID": s.ID, "info": *s})
}
