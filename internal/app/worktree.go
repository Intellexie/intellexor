package app

import (
	"fmt"

	"github.com/Intellexie/intellexor/internal/board"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/worktree"
)

// Git worktree isolation: a session can run in a fresh worktree on its own
// branch instead of the shared directory, so several cards/subagents can
// work on the same repository in parallel. The session keeps the parent
// repo's project ID (store.GitRoot resolves the worktree back to the main
// repository), so the board, config and session storage stay grouped.

// isolateSession creates a git worktree for the session and points the
// session's Directory at it. name names the branch (itx/<name>) and the
// worktree directory. The session record is updated in place and the change
// broadcast, so connected clients see the worktree fields.
func (i *Instance) isolateSession(s *session.Session, name string) (worktree.Info, error) {
	wt, err := worktree.Create(i.Directory, name, worktree.ConfigOf(i.Directory))
	if err != nil {
		return worktree.Info{}, err
	}
	fresh, uerr := i.Mgr.Update(s.ID, func(u *session.Session) {
		u.Directory = wt.Path
		u.Worktree = &session.WorktreeInfo{Branch: wt.Branch, Path: wt.Path, Base: wt.Base}
	})
	if uerr != nil {
		_ = worktree.Remove(i.Directory, wt)
		return worktree.Info{}, uerr
	}
	*s = *fresh
	i.Hub.Publish("session.updated", map[string]any{"sessionID": s.ID, "info": *s})
	return wt, nil
}

// isolationWanted reports whether a card should start isolated: the card's
// own flag, or the config default.
func isolationWanted(i *Instance, card *board.Card) bool {
	if card != nil && card.Isolate {
		return true
	}
	return worktree.ConfigOf(i.Directory).Enabled
}

// EnsureCardWorktree gives a freshly linked session its worktree when the
// card wants isolation. This covers the web UI's Start flow, which creates
// the session itself and links it to the card afterwards — the server-side
// startCard path creates the worktree before the session instead. A no-op
// when the card does not want isolation or the session already has one.
func (i *Instance) EnsureCardWorktree(projectID, cardID, sessionID string) {
	cards, err := board.Get(projectID)
	if err != nil {
		return
	}
	var card *board.Card
	for idx := range cards {
		if cards[idx].ID == cardID {
			card = &cards[idx]
			break
		}
	}
	if card == nil || !isolationWanted(i, card) {
		return
	}
	s, err := i.Mgr.Find(sessionID)
	if err != nil || s.Worktree != nil {
		return
	}
	if wt, err := i.isolateSession(s, "card-"+cardID); err == nil {
		_, _ = board.SetWorktree(projectID, cardID, wt.Branch, wt.Path)
		i.Hub.Publish("board.updated", map[string]any{"directory": i.Directory})
	}
}

// MergeCard merges the card's worktree branch into the repository (the
// branch the main checkout currently has) and cleans the worktree up: the
// worktree directory and its branch are removed, the card's worktree fields
// are cleared and the session is pointed back at the main directory. The
// card must have a worktree (started with isolation, not yet merged).
func MergeCard(i *Instance, projectID, cardID string) (board.Card, error) {
	cards, err := board.Get(projectID)
	if err != nil {
		return board.Card{}, err
	}
	var card *board.Card
	for idx := range cards {
		if cards[idx].ID == cardID {
			card = &cards[idx]
			break
		}
	}
	if card == nil {
		return board.Card{}, board.ErrNotFound
	}
	if card.Branch == "" || card.WorktreePath == "" {
		return board.Card{}, fmt.Errorf("card %s has no worktree to merge", cardID)
	}
	wt := worktree.Info{Branch: card.Branch, Path: card.WorktreePath}
	if err := worktree.Merge(i.Directory, wt, ""); err != nil {
		return board.Card{}, err
	}
	if err := worktree.Remove(i.Directory, wt); err != nil {
		return board.Card{}, err
	}
	// the session goes back to the main directory: its worktree is gone
	if card.SessionID != "" {
		if s, err := i.Mgr.Find(card.SessionID); err == nil && s.Worktree != nil {
			if fresh, uerr := i.Mgr.Update(s.ID, func(u *session.Session) {
				u.Directory = i.Directory
				u.Worktree = nil
			}); uerr == nil {
				i.Hub.Publish("session.updated", map[string]any{"sessionID": s.ID, "info": *fresh})
			}
		}
	}
	updated, err := board.SetWorktree(projectID, cardID, "", "")
	if err != nil {
		return board.Card{}, err
	}
	i.Hub.Publish("board.updated", map[string]any{"directory": i.Directory})
	return updated, nil
}
