package app

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/Intellexie/intellexor/internal/cron"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

// Cron jobs: a background sweep mirrors the board's scheduled starts
// (scheduler.go) — every tick, each project's jobs are checked and due ones
// start a fresh session prompted with the job's prompt. The pass is
// deliberately dumb: jobs are tiny JSON files, a coarse sweep keeps the
// logic simple and self-healing (a run that failed to start is retried on
// the next tick only if its schedule came due again; schedules that came
// due while the server was down fire right after startup).

// cronTick is how often due jobs are looked for. Schedules have minute
// granularity, so a few seconds of slack are plenty.
const cronTick = 10 * time.Second

var cronOnce sync.Once

// startCronScheduler launches the background sweep once for the process, on
// the first instance registration (instances register from app.New).
func startCronScheduler() {
	cronOnce.Do(func() {
		go cronPass() // catch schedules that came due before startup
		go func() {
			t := time.NewTicker(cronTick)
			defer t.Stop()
			for range t.C {
				cronPass()
			}
		}()
	})
}

// cronPass starts every due job, at most one sweep per project (several
// instances can share a project — they all see the same job file).
func cronPass() {
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
		sweepCrons(i, pid, now)
	}
}

// sweepCrons starts the due jobs of one project. ClaimDue re-checks the job
// under the cron package's lock before handing it out, so two racing sweeps
// (or a sweep and a future manual trigger) can never double-start a run.
func sweepCrons(i *Instance, projectID string, now int64) {
	jobs, err := cron.Get(projectID)
	if err != nil {
		return
	}
	for _, j := range jobs {
		if j.Paused {
			continue
		}
		if j.NextRun != 0 && now < j.NextRun {
			continue
		}
		claimed, ok, err := cron.ClaimDue(projectID, j.ID, now)
		if err != nil || !ok {
			continue
		}
		go runCronJob(i, projectID, claimed)
	}
}

// runCronJob starts one run: create the session (titled after the job),
// record it on the job (so the UI's View button can target the latest
// active — or last finished — run), then prompt it with the job's prompt.
// The prompt runs synchronously in this goroutine: the sweep already
// launched us per job, and recording the session before prompting keeps the
// job's state consistent even if the provider errors out.
func runCronJob(i *Instance, projectID string, j cron.Job) {
	sess := &session.Session{
		ProjectID: projectID,
		Directory: i.Directory,
		Title:     j.Title,
		Agent:     "build",
	}
	if err := i.Mgr.Create(sess); err != nil {
		return
	}
	i.Hub.Publish("session.created", map[string]any{"sessionID": sess.ID, "info": *sess})
	i.Hub.Publish("session.updated", map[string]any{"sessionID": sess.ID, "info": *sess})
	if err := cron.SetSession(projectID, j.ID, sess.ID); err != nil {
		return
	}
	// connected web clients refresh the cron list (new session, hourglass)
	i.Hub.Publish("cron.updated", map[string]any{"directory": i.Directory})
	raw, _ := json.Marshal(map[string]any{"type": "text", "text": j.Prompt})
	_, _ = i.Prompt(sess, PromptInput{Agent: "build", Parts: []json.RawMessage{raw}})
}
