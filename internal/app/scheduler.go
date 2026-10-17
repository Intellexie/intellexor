package app

import (
	"errors"
	"sync"
	"time"

	"github.com/Intellexie/intellexor/internal/board"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

// Scheduled card starts: cards in open with a startAt in the past are
// started automatically, exactly like the Start button (a fresh session,
// the card linked to it and prompted with its task). A single background
// ticker sweeps every registered project; the pass is deliberately dumb —
// boards are tiny JSON files, and a coarse sweep keeps the logic simple and
// self-healing (a start that failed is retried on the next tick, and
// schedules that became due while the server was down fire right after
// startup).

// schedTick is how often due cards are looked for. The UI shows schedules at
// minute granularity, so a few seconds of slack are plenty.
const schedTick = 10 * time.Second

var schedOnce sync.Once

// startScheduler launches the background sweep once for the process, on the
// first instance registration (instances register from app.New).
func startScheduler() {
	schedOnce.Do(func() {
		go schedulerPass() // catch schedules that came due before startup
		go func() {
			t := time.NewTicker(schedTick)
			defer t.Stop()
			for range t.C {
				schedulerPass()
			}
		}()
	})
}

// schedulerPass starts every due card, at most one sweep per project (several
// instances can share a project — they all see the same board file).
func schedulerPass() {
	now := util.Now()
	instMu.Lock()
	insts := append([]*Instance(nil), instReg...)
	instMu.Unlock()

	seen := map[string]bool{}
	for _, i := range insts {
		pid := store.ProjectID(i.Directory)
		if seen[pid] {
			continue
		}
		seen[pid] = true
		sweepProject(i, pid, now)
	}
}

// sweepProject starts the due cards of one project. startCard re-checks the
// card before creating a session, so a card started manually between the
// sweep's load and its start is never double-started.
func sweepProject(i *Instance, projectID string, now int64) {
	cards, err := board.Get(projectID)
	if err != nil {
		return
	}
	for _, c := range cards {
		if c.Column != "open" || c.SessionID != "" || c.StartAt == 0 || c.StartAt > now {
			continue
		}
		if _, err := startCard(i, projectID, c.ID); err != nil {
			if errors.Is(err, errCardNotOpen) {
				// the card was started (or moved) by someone else while it
				// sat due — its schedule is stale, drop it so it is not
				// retried forever
				_, _ = board.SetSchedule(projectID, c.ID, 0)
			}
			// anything else is transient (provider hiccup, disk error):
			// leave the schedule and let the next sweep retry
			continue
		}
	}
}
