package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/Intellexie/intellexor/internal/board"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
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
		startScheduler()
		startCronScheduler()
	})
	instMu.Lock()
	instReg = append(instReg, i)
	instMu.Unlock()
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
// point — agent tool, HTTP API, ACP): watchers are pinged, and a card
// linked as its follow-up is started automatically. The auto-start is the
// same flow as the web UI's Start button and only applies while the target
// is still waiting in open — if it was picked up meanwhile, the link is a
// harmless no-op.
func onCardDone(projectID string, card board.Card) {
	notifyWatchers(projectID, card)
	if card.Link == "" {
		return
	}
	in := instanceForProject(projectID)
	if in == nil {
		return
	}
	cards, err := board.Get(projectID)
	if err != nil {
		return
	}
	for _, c := range cards {
		if c.ID != card.Link {
			continue
		}
		if c.Column != "open" || c.SessionID != "" {
			return
		}
		go func() {
			if _, err := startCard(in, projectID, c.ID); err != nil {
				// best effort: the card stays in open for a manual start
				return
			}
		}()
		return
	}
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
	sess := &session.Session{
		ProjectID: projectID,
		Directory: i.Directory,
		Title:     card.Title,
		Agent:     "build",
	}
	if err := i.Mgr.Create(sess); err != nil {
		return "", err
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
	// the card's task is the plain user message; its attachments (if any)
	// ride along as file parts — the same shape the composer sends
	raw, _ := json.Marshal(map[string]any{"type": "text", "text": card.Title})
	parts := []json.RawMessage{raw}
	for _, a := range card.Attachments {
		b, err := json.Marshal(a)
		if err != nil {
			continue
		}
		parts = append(parts, b)
	}
	go func() {
		_, _ = i.Prompt(sess, PromptInput{Agent: "build", Parts: parts})
	}()
	return sess.ID, nil
}
