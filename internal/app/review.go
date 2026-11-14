package app

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Intellexie/intellexor/internal/board"
	"github.com/Intellexie/intellexor/internal/config"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
	"github.com/Intellexie/intellexor/internal/worktree"
)

// Review stage and live card status. A card enters the review column only
// by an explicit move (web UI drag, board tool, API) — an agent stopping or
// going idle never moves its card or spawns a review session. Once a card
// is in review, a configurable review agent inspects the card's changes;
// its findings are attached to the card. Approving moves the card to done
// (merging an isolated worktree), rejecting sends it back to inprogress
// with the findings as the next prompt. The same hooks keep the card's
// live status (running/waiting/idle/error/blocked) and usage snapshot in
// sync with its session.

// onSessionStatus is called from Instance.SetStatus for every session
// status change. It is the single hook that keeps cards in sync with their
// sessions: live status, the review→inprogress transition when work
// resumes, and the review agent's findings. Cheap no-op for sessions
// without a card.
func onSessionStatus(i *Instance, sessionID string, st StatusInfo) {
	typ, _ := st["type"].(string)
	projectID := store.ProjectID(i.Directory)
	cards, err := board.Get(projectID)
	if err != nil {
		return
	}
	for _, c := range cards {
		switch {
		case c.SessionID == sessionID:
			syncWorkerCard(i, projectID, c, sessionID, typ)
		case c.ReviewSessionID == sessionID:
			syncReviewCard(i, projectID, c, sessionID, typ)
		}
	}
}

// syncWorkerCard updates a card whose worker session changed status: on
// busy it (re)enters inprogress (a rejected card getting more work leaves
// review). Going idle never moves the card — the card stays where it is
// until it is explicitly moved (to review or done) or its session is
// stopped by moving it back to open.
func syncWorkerCard(i *Instance, projectID string, c board.Card, sessionID, typ string) {
	if typ == "busy" {
		if c.Column == "review" {
			// work resumed on a card that was waiting in review
			if _, err := board.Update(projectID, c.ID, nil, strPtr("inprogress"), nil, nil); err != nil {
				return
			}
			c.Column = "inprogress"
			publishBoardUpdate(i)
		}
		syncCardRuntime(i, projectID, c, sessionID)
		return
	}
	syncCardRuntime(i, projectID, c, sessionID)
}

// syncReviewCard captures the review agent's findings when the review
// session finishes its turn.
func syncReviewCard(i *Instance, projectID string, c board.Card, sessionID, typ string) {
	if typ == "idle" && c.Column == "review" {
		findings := ""
		if s, err := i.Mgr.Find(sessionID); err == nil {
			findings = lastAssistantText(i, s)
		}
		if findings != "" && findings != "Task completed" {
			c, _ = board.Edit(projectID, c.ID, func(cc *board.Card) error { cc.Findings = findings; return nil })
		}
	}
	syncCardRuntime(i, projectID, c, sessionID)
}

// syncCardRuntime snapshots the session's live state onto the card: status
// (waiting when a question or permission is pending, running while busy,
// idle otherwise) plus cost/tokens. Publishes board.updated only when
// something actually changed.
func syncCardRuntime(i *Instance, projectID string, c board.Card, sessionID string) {
	rt := board.Runtime{StartedAt: c.StartedAt}
	if s, err := i.Mgr.Find(sessionID); err == nil {
		rt.Cost = s.Cost
		if s.Tokens.Total != nil {
			rt.Tokens = *s.Tokens.Total
		}
	}
	switch {
	case len(i.Perms.Pending(sessionID)) > 0 || questionPending(i, sessionID):
		rt.Status = "waiting"
		rt.StatusText = "waiting for input (question or permission)"
	case i.statusBusy(sessionID):
		rt.Status = "running"
		if rt.StartedAt == 0 {
			rt.StartedAt = util.Now()
		}
	}
	// a blocked card stays blocked until new work arrives (the busy branch
	// above clears it on the next run) — an idle sync after the failure
	// must not wipe the error
	if rt.Status == "" && c.Status == "blocked" {
		rt.Status = "blocked"
		rt.StatusText = c.StatusText
	}
	_, changed, err := board.SetRuntime(projectID, c.ID, rt)
	if err == nil && changed {
		publishBoardUpdate(i)
	}
}

func questionPending(i *Instance, sessionID string) bool {
	for _, q := range i.Quest.Pending() {
		if q.SessionID == sessionID {
			return true
		}
	}
	return false
}

// refreshCardRuntime re-snapshots the card linked to the session (worker or
// review session) — called outside status changes, e.g. after a step
// updates the session's cost/token totals.
func refreshCardRuntime(i *Instance, sessionID string) {
	projectID := store.ProjectID(i.Directory)
	cards, err := board.Get(projectID)
	if err != nil {
		return
	}
	for _, c := range cards {
		if c.SessionID == sessionID || c.ReviewSessionID == sessionID {
			syncCardRuntime(i, projectID, c, sessionID)
			return
		}
	}
}

func publishBoardUpdate(i *Instance) {
	i.Hub.Publish("board.updated", map[string]any{"directory": i.Directory})
}

func strPtr(s string) *string { return &s }

// StartReviewOnMove runs the review stage for a card that was just moved
// to the review column by an explicit action (web UI drag, board tool,
// API). A no-op for cards in any other column. Called by the board card
// PATCH route and the board tool's move hook.
func StartReviewOnMove(i *Instance, projectID, cardID string) {
	cards, err := board.Get(projectID)
	if err != nil {
		return
	}
	for _, c := range cards {
		if c.ID == cardID {
			if c.Column == "review" {
				startReviewIfNeeded(i, projectID, c)
			}
			return
		}
	}
}

// startReviewIfNeeded runs the configured review agent on a card that just
// entered review — a fresh session prompted with the card's spec and the
// diff of its changes. A no-op when auto-review is off or the configured
// agent does not exist.
func startReviewIfNeeded(i *Instance, projectID string, c board.Card) {
	cfg := config.Merge(i.Directory).Board
	if !cfg.AutoReviewOn() {
		return
	}
	agentName := cfg.ReviewAgentOrDefault()
	if _, err := i.Agent(agentName); err != nil {
		return // no review agent configured: the card waits in review
	}
	sess := &session.Session{
		ProjectID: projectID,
		Directory: i.Directory,
		Title:     "Review: " + c.Title,
		Agent:     agentName,
	}
	if err := i.Mgr.Create(sess); err != nil {
		return
	}
	if _, err := board.Edit(projectID, c.ID, func(cc *board.Card) error {
		cc.ReviewSessionID = sess.ID
		return nil
	}); err != nil {
		return
	}
	i.Hub.Publish("session.created", map[string]any{"sessionID": sess.ID, "info": *sess})
	i.Hub.Publish("session.updated", map[string]any{"sessionID": sess.ID, "info": *sess})
	publishBoardUpdate(i)
	raw, _ := json.Marshal(map[string]any{"type": "text", "text": reviewPromptText(i, c)})
	go func() {
		_, _ = i.Prompt(sess, PromptInput{Agent: agentName, Parts: []json.RawMessage{raw}})
	}()
}

// reviewPromptText builds the review agent's task: the card's spec plus the
// diff of its changes (worktree diff against the base ref for isolated
// cards, working-tree diff otherwise).
func reviewPromptText(i *Instance, c board.Card) string {
	var b strings.Builder
	b.WriteString("Review the changes of kanban card #" + c.ID + " — \"" + c.Title + "\".\n")
	if d := strings.TrimSpace(c.Description); d != "" {
		b.WriteString("\nDescription: " + d + "\n")
	}
	if cr := strings.TrimSpace(c.Criteria); cr != "" {
		b.WriteString("\nAcceptance criteria:\n" + cr + "\n")
	}
	b.WriteString("\nDiff of the card's changes:\n\n```diff\n")
	b.WriteString(cardDiff(i, c))
	b.WriteString("\n```\n")
	return b.String()
}

// cardDiff returns the card's changes as a patch: the worktree diff against
// its base ref for isolated cards, else the repository's uncommitted
// changes. Capped so a huge diff cannot blow the review prompt.
func cardDiff(i *Instance, c board.Card) string {
	const maxDiff = 24000
	var out string
	if c.WorktreePath != "" {
		base := "HEAD"
		if c.SessionID != "" {
			if s, err := i.Mgr.Find(c.SessionID); err == nil && s.Worktree != nil && s.Worktree.Base != "" {
				base = s.Worktree.Base
			}
		}
		if d, err := worktree.Diff(c.WorktreePath, base); err == nil {
			out = d
		}
	} else {
		out = gitDiffHead(i.Directory)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "(no changes found)"
	}
	if len(out) > maxDiff {
		out = out[:maxDiff] + "\n… (diff truncated)"
	}
	return out
}

func gitDiffHead(dir string) string {
	cmd := exec.Command("git", "diff", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return string(out)
}

// ApproveCard resolves a card waiting in review as done: an isolated
// card's worktree is merged back first, then the card moves to done —
// which fires the usual done hooks (watchers, dependency triggers).
func ApproveCard(i *Instance, projectID, cardID string) (board.Card, error) {
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
	if card.Column != "review" {
		return board.Card{}, fmt.Errorf("card %s is not waiting in review (column: %s)", cardID, card.Column)
	}
	if card.Branch != "" && card.WorktreePath != "" {
		if _, err := MergeCard(i, projectID, cardID); err != nil {
			return board.Card{}, fmt.Errorf("merge failed: %w", err)
		}
	}
	col := "done"
	updated, err := board.Update(projectID, cardID, nil, &col, nil, nil)
	if err != nil {
		return board.Card{}, err
	}
	_, _, _ = board.SetRuntime(projectID, cardID, board.Runtime{})
	publishBoardUpdate(i)
	return updated, nil
}

// RejectCard sends a card waiting in review back to inprogress and prompts
// its session with the review findings as the next prompt. When the card
// has no session (it was never started, or the session is gone), the
// findings stay on the card for the next start.
func RejectCard(i *Instance, projectID, cardID string) (board.Card, error) {
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
	if card.Column != "review" {
		return board.Card{}, fmt.Errorf("card %s is not waiting in review (column: %s)", cardID, card.Column)
	}
	col := "inprogress"
	updated, err := board.Update(projectID, cardID, nil, &col, nil, nil)
	if err != nil {
		return board.Card{}, err
	}
	_, _, _ = board.SetRuntime(projectID, cardID, board.Runtime{Status: "running"})
	// the findings ride along as the next prompt for the card's session
	if updated.SessionID != "" {
		if s, err := i.Mgr.Find(updated.SessionID); err == nil {
			msg := "The review of this card found issues. Please address them:\n\n"
			if f := strings.TrimSpace(updated.Findings); f != "" {
				msg += f
			} else {
				msg += "(no findings recorded — check the card's changes against its acceptance criteria)"
			}
			raw, _ := json.Marshal(map[string]any{"type": "text", "text": msg})
			go func() {
				_, _ = i.Prompt(s, PromptInput{Agent: s.Agent, Parts: []json.RawMessage{raw}})
			}()
		}
	}
	publishBoardUpdate(i)
	return updated, nil
}

// markCardBlocked flags the card a failed session was working on as
// blocked, with the provider error as the status text — the failure
// handling for non-retryable provider errors (4xx, bad request): the work
// stops visibly instead of silently dying. Retryable errors are retried
// forever and never reach this.
func markCardBlocked(i *Instance, sessionID, errMsg string) {
	projectID := store.ProjectID(i.Directory)
	cards, err := board.Get(projectID)
	if err != nil {
		return
	}
	for _, c := range cards {
		if c.SessionID != sessionID {
			continue
		}
		// keep the usage snapshot; only the status changes
		if _, changed, err := board.SetRuntime(projectID, c.ID, board.Runtime{
			Status:     "blocked",
			StatusText: errMsg,
			StartedAt:  c.StartedAt,
			Cost:       c.Cost,
			Tokens:     c.Tokens,
		}); err == nil && changed {
			publishBoardUpdate(i)
		}
		return
	}
}
