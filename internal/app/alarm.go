package app

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/Intellexie/intellexor/internal/alarm"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

// Alarms: a background sweep mirrors the cron scheduler (cron.go) — every
// tick, each project's alarms are checked and due ones inject a user
// message ("alarm-triggered") into their session, letting an agent continue
// itself after a long background job. ClaimDue marks alarms fired under the
// alarm package's lock before handing them out, so two racing sweeps (or a
// sweep and a restart) can never double-fire.

// alarmTick is how often due alarms are looked for. Alarms are timestamps,
// not minute-granularity schedules, so the tick is tighter than cron's.
const alarmTick = 5 * time.Second

var alarmOnce sync.Once

// startAlarmScheduler launches the background sweep once for the process,
// on the first instance registration (instances register from app.New).
func startAlarmScheduler() {
	alarmOnce.Do(func() {
		go alarmPass() // catch alarms that came due before startup
		go func() {
			t := time.NewTicker(alarmTick)
			defer t.Stop()
			for range t.C {
				alarmPass()
			}
		}()
	})
}

// alarmPass fires every due alarm, at most one sweep per project (several
// instances can share a project — they all see the same alarm file).
func alarmPass() {
	// alarm timestamps are unix seconds (util.Now is milliseconds)
	now := util.Now() / 1000
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
		sweepAlarms(i, pid, now)
	}
}

// sweepAlarms claims and fires the due alarms of one project. Each fire
// runs in its own goroutine: Prompt blocks on the session lock, so a busy
// session simply receives the wake-up once its current turn finishes, and
// the sweep must never wait on that.
func sweepAlarms(i *Instance, projectID string, now int64) {
	due, err := alarm.ClaimDue(projectID, now)
	if err != nil || len(due) == 0 {
		return
	}
	// the claimed alarms are gone from the pending set — tell connected
	// clients so the ⏰ indicator on the session disappears right away
	// (fireAlarm's Prompt broadcast alone would only refresh the timeline).
	i.publishAlarmUpdate()
	for _, a := range due {
		go fireAlarm(i, a)
	}
}

// publishAlarmUpdate broadcasts alarm.updated on every instance hub of
// this instance's project, so connected clients refresh the ⏰ indicators
// (the alarm's session may belong to a different directory than the
// mutating instance).
func (i *Instance) publishAlarmUpdate() {
	pid := store.ProjectID(i.Directory)
	instMu.Lock()
	for _, w := range instReg {
		if store.ProjectID(w.Directory) == pid {
			w.Hub.Publish("alarm.updated", map[string]any{"directory": w.Directory})
		}
	}
	instMu.Unlock()
}

// PublishAlarmUpdate is the exported hook for HTTP handlers that mutate
// alarms outside the sweep (the web UI's trigger-now / delete buttons).
func (i *Instance) PublishAlarmUpdate() { i.publishAlarmUpdate() }

// FireAlarmNow delivers the wake-up message for an alarm the caller has
// already claimed (marked fired) — the web UI's "trigger now" button.
// Like the sweep, the prompt runs in its own goroutine (it blocks on the
// session lock) and the indicators are refreshed via alarm.updated.
func (i *Instance) FireAlarmNow(a alarm.Alarm) {
	i.publishAlarmUpdate()
	go fireAlarm(i, a)
}

// fireAlarm injects the wake-up message into the alarm's session, on
// behalf of the user. The message is the literal "alarm-triggered" token
// (so the agent can recognize it), followed by the alarm's note as context
// when one was set. Like the board's watcher prompts, the turn runs in the
// session's own instance when one exists, so hub events reach its clients.
func fireAlarm(i *Instance, a alarm.Alarm) {
	sess, err := i.Mgr.Find(a.SessionID)
	if err != nil {
		return
	}
	target := i
	if w := instanceForDirectory(sess.Directory); w != nil {
		target = w
	}
	text := "alarm-triggered"
	if a.Note != "" {
		text += "\n\n" + a.Note
	}
	raw, _ := json.Marshal(map[string]any{"type": "text", "text": text})
	_, _ = target.Prompt(sess, PromptInput{Parts: []json.RawMessage{raw}})
}
