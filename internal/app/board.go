package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Intellexie/intellexor/internal/board"
	"github.com/Intellexie/intellexor/internal/config"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
	"github.com/Intellexie/intellexor/internal/worktree"
)

// Agent-side board integration: watching cards (pinged when they are marked
// done) and starting cards (the web UI's Start button, triggered by the
// agent). Both are exposed through the board tool; this file holds the
// orchestration that needs instance-level access.

// Every instance registers itself so a board mutation made through one of
// them (agent tool call, HTTP route, ACP) can reach the sessions of the same
// project. The first registration also wires the board package's done-hook;
// board.Update is the single choke point for all move entry points.
var (
	instMu   sync.Mutex
	instReg  []*Instance
	hookOnce sync.Once
)

func registerInstance(i *Instance) {
	hookOnce.Do(func() {
		board.OnMovedToDone = onCardDone
		board.OnStopped = onCardStopped
		startScheduler()
		startCronScheduler()
		startAlarmScheduler()
	})
	instMu.Lock()
	instReg = append(instReg, i)
	instMu.Unlock()
}

// onCardStopped aborts the agent session released by a card that was moved
// from inprogress back to open — the "stop the agent" flow the web UI
// confirms before dragging. The session's cancel func lives on the instance
// that ran its prompt loop, so the abort is routed there (sessions are
// keyed by project, so any instance of the project can look it up).
func onCardStopped(projectID, sessionID string) {
	in := instanceForProject(projectID)
	if in == nil {
		return
	}
	if s, err := in.Mgr.Find(sessionID); err == nil {
		if w := instanceForDirectory(s.Directory); w != nil {
			in = w
		}
	}
	in.Abort(sessionID)
}

func instanceForProject(projectID string) *Instance {
	instMu.Lock()
	defer instMu.Unlock()
	for _, i := range instReg {
		if store.ProjectID(i.Directory) == projectID {
			return i
		}
	}
	return nil
}

func instanceForDirectory(dir string) *Instance {
	instMu.Lock()
	defer instMu.Unlock()
	for _, i := range instReg {
		if i.Directory == dir {
			return i
		}
	}
	return nil
}

// onCardDone fires when a card lands in the done column (from any entry
// point — agent tool, HTTP API, ACP): watchers are pinged, and every open
// card whose blockers are now all done is started automatically (the
// dependency flow; the legacy single link is a one-element blockedBy). The
// auto-start is the same flow as the web UI's Start button and only applies
// while the target is still waiting in open — if it was picked up meanwhile,
// it is skipped like any other not-ready card.
func onCardDone(projectID string, card board.Card) {
	notifyWatchers(projectID, card)
	in := instanceForProject(projectID)
	if in == nil {
		return
	}
	go triggerReadyCards(in, projectID)
}

// triggerReadyCards starts every open, unowned card whose blockers are all
// done — the dependency chain: finishing one card releases the next wave.
// The project's max_running cap (config board.max_running) limits how many
// cards run at once; cards already in inprogress count against it. Manual
// starts are never capped. Missing blocker IDs count as satisfied (a deleted
// blocker must not block forever — Delete clears references anyway).
func triggerReadyCards(in *Instance, projectID string) {
	maxRun := config.Merge(in.Directory).Board.MaxRunningOrZero()
	for {
		cards, err := board.Get(projectID)
		if err != nil {
			return
		}
		done := map[string]bool{}
		running := 0
		for _, c := range cards {
			if c.Column == "done" {
				done[c.ID] = true
			}
			if c.Column == "inprogress" {
				running++
			}
		}
		if maxRun > 0 && running >= maxRun {
			return
		}
		started := false
		for _, c := range cards {
			if c.Column != "open" || c.SessionID != "" || !blockersSatisfied(c, done) {
				continue
			}
			if _, err := startCard(in, projectID, c.ID); err != nil {
				// best effort: the card stays in open for a manual start
				continue
			}
			started = true
			break // re-evaluate cap and readiness from a fresh board load
		}
		if !started {
			return
		}
	}
}

// blockersSatisfied reports whether every blocker of the card is done.
// Blockers that no longer exist count as satisfied.
func blockersSatisfied(c board.Card, done map[string]bool) bool {
	for _, b := range c.BlockedBy {
		if !done[b] {
			return false
		}
	}
	return true
}

// notifyWatchers prompts every session watching the card with a short
// message on behalf of the user. Called after the board lock is released;
// each prompt runs asynchronously so the mover is never blocked — a busy
// watcher simply receives the message once its current turn finishes.
func notifyWatchers(projectID string, card board.Card) {
	in := instanceForProject(projectID)
	if in == nil {
		return
	}
	for _, sid := range card.Watchers {
		// session files are keyed by project, so any instance of the project
		// can load the watcher — not just the one in its own directory
		sess, err := in.Mgr.Find(sid)
		if err != nil {
			continue
		}
		// run the watcher's turn in its own instance when one exists, so its
		// hub events reach its connected clients
		target := in
		if w := instanceForDirectory(sess.Directory); w != nil {
			target = w
		}
		go promptWatcher(target, sess, card)
	}
}

func promptWatcher(i *Instance, s *session.Session, card board.Card) {
	msg := fmt.Sprintf("Card %q (id: %s) has been marked done.", card.Title, card.ID)
	if s.Agent == "orchestrator" {
		// the orchestrator coordinates by ticket ID and started the card
		// itself, so the short default message is all it needs to react
		msg = fmt.Sprintf("Ticket %s is done.", card.ID)
	}
	raw, _ := json.Marshal(map[string]any{"type": "text", "text": msg})
	_, _ = i.Prompt(s, PromptInput{Parts: []json.RawMessage{raw}})
}

// errCardNotOpen is returned by startCard when the card is no longer waiting
// in open (already started, moved, or deleted) — the scheduler uses it to
// tell a stale schedule apart from a transient failure.
var errCardNotOpen = errors.New("card is not waiting in the open column")

// startCard starts a new agent session on a card — the same flow as the web
// UI's Start button: create the session, link the card to it and move it to
// inprogress BEFORE prompting (so the agent knows its card from the first
// step), then prompt it with the card's title as its task. The task title
// goes in as the plain user message; all card handling guidance is injected
// server-side into the system prompt (boardContext). Returns the new
// session ID.
//
// The card must still be waiting in open with no session: every caller
// (Start button, board tool, done-link trigger, scheduler) checks that
// beforehand, and the re-check here closes the race where a card is started
// twice by two paths at nearly the same time.
func startCard(i *Instance, projectID, cardID string) (string, error) {
	cards, err := board.Get(projectID)
	if err != nil {
		return "", err
	}
	var card *board.Card
	for idx := range cards {
		if cards[idx].ID == cardID {
			card = &cards[idx]
			break
		}
	}
	if card == nil {
		return "", fmt.Errorf("card not found: %s", cardID)
	}
	if card.Column != "open" || card.SessionID != "" {
		return "", errCardNotOpen
	}
	// the card picks its agent and model, falling back to the defaults:
	// agent "" runs the build agent; model "" (or an unparseable value)
	// pins the UI-equivalent default — first connected provider with its
	// preferred model — so a scheduled, tool-driven or dependency start
	// lands on the same model a manual click would pick, never on whatever
	// the prompt-time fallbacks (config model, pickDefault) would scrape
	// together. With nothing connected, stay unpinned and let the prompt
	// resolve (and error) as usual.
	agentName := card.Agent
	if agentName == "" {
		agentName = "build"
	}
	sel := cardModelSel(card.Model)
	if sel == nil {
		sel = defaultModelSel(i.Directory)
	}
	sess := &session.Session{
		ProjectID: projectID,
		Directory: i.Directory,
		Title:     card.Title,
		Agent:     agentName,
	}
	// worktree isolation: when the card opts in (or the config default is
	// on), the session runs in a fresh worktree on its own branch — before
	// the session is created, so the very first prompt already works there
	started := false
	defer func() {
		// if the start fails after the worktree was created, do not leave
		// the branch and directory behind
		if !started && sess.Worktree != nil {
			_ = worktree.Remove(i.Directory, worktree.Info{
				Branch: sess.Worktree.Branch, Path: sess.Worktree.Path,
			})
		}
	}()
	if isolationWanted(i, card) {
		wt, werr := worktree.Create(i.Directory, "card-"+card.ID, worktree.ConfigOf(i.Directory))
		if werr != nil {
			return "", fmt.Errorf("worktree isolation failed: %w", werr)
		}
		sess.Directory = wt.Path
		sess.Worktree = &session.WorktreeInfo{Branch: wt.Branch, Path: wt.Path, Base: wt.Base}
	}
	if sel != nil {
		sess.Model = &session.ModelRef{ID: sel.ModelID, ProviderID: sel.ProviderID}
	}
	if err := i.Mgr.Create(sess); err != nil {
		return "", err
	}
	if sess.Worktree != nil {
		if _, err := board.SetWorktree(projectID, cardID, sess.Worktree.Branch, sess.Worktree.Path); err == nil {
			card.Branch, card.WorktreePath = sess.Worktree.Branch, sess.Worktree.Path
		}
	}
	i.Hub.Publish("session.created", map[string]any{"sessionID": sess.ID, "info": *sess})
	i.Hub.Publish("session.updated", map[string]any{"sessionID": sess.ID, "info": *sess})
	col := "inprogress"
	if _, err := board.Update(projectID, cardID, nil, &col, nil, &sess.ID); err != nil {
		return "", err
	}
	// Update clears the card's start schedule as it leaves open: a schedule
	// is consumed by the start, so moving the card back to open later cannot
	// fire a stale start time
	// same broadcast the board HTTP routes and tool send: connected web
	// clients refresh the board and give the new session a tab (the
	// Start-button flow) even though no client initiated this start
	i.Hub.Publish("board.updated", map[string]any{"directory": i.Directory})
	// the card's task is the plain user message — title plus, when set, its
	// description and acceptance criteria; its attachments (if any) ride
	// along as file parts — the same shape the composer sends
	raw, _ := json.Marshal(map[string]any{"type": "text", "text": cardTaskText(*card)})
	parts := []json.RawMessage{raw}
	for _, a := range card.Attachments {
		b, err := json.Marshal(a)
		if err != nil {
			continue
		}
		parts = append(parts, b)
	}
	go func() {
		_, _ = i.Prompt(sess, PromptInput{Agent: agentName, Model: sel, Parts: parts})
	}()
	started = true
	// live status: the card is running from here until its session goes idle
	_, _, _ = board.SetRuntime(projectID, cardID, board.Runtime{
		Status:    "running",
		StartedAt: util.Now(),
	})
	i.Hub.Publish("board.updated", map[string]any{"directory": i.Directory})
	return sess.ID, nil
}

// cardTaskText builds the prompt the card's session is started with: the
// title, plus — when set — the description and the acceptance criteria, so
// the agent works against the full spec and not just the title.
func cardTaskText(c board.Card) string {
	var b strings.Builder
	b.WriteString(c.Title)
	if d := strings.TrimSpace(c.Description); d != "" {
		b.WriteString("\n\nDescription: " + d)
	}
	if cr := strings.TrimSpace(c.Criteria); cr != "" {
		b.WriteString("\n\nAcceptance criteria:\n" + cr)
	}
	return b.String()
}

// cardModelSel parses a card's model field ("provider/model") into a
// selection; "" or a malformed value yields nil (use the default).
func cardModelSel(model string) *ModelSel {
	if model == "" {
		return nil
	}
	p, m, ok := strings.Cut(model, "/")
	if !ok || p == "" || m == "" {
		return nil
	}
	return &ModelSel{ProviderID: p, ModelID: m}
}
