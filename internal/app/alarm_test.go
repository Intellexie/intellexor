package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Intellexie/intellexor/internal/alarm"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/tool"
	"github.com/Intellexie/intellexor/internal/util"
)

// The alarm tool arms a persisted alarm for the calling session; the
// background sweep then injects a user message "alarm-triggered" into the
// session so the agent can continue itself after a long background job.

// respondEmptyStopHandler is the minimal fake provider: every completion
// (including the async title generation) returns an empty stop.
func respondEmptyStopHandler(w http.ResponseWriter, r *http.Request) {
	respondEmptyStop(w)
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func TestAlarmToolSetsFutureAlarm(t *testing.T) {
	i, s := setupAbortTest(t, respondEmptyStopHandler)

	at := util.Now()/1000 + 60
	res, err := tool.AlarmTool{}.Run(&tool.Ctx{
		Ctx:       t.Context(),
		SessionID: s.ID,
		Directory: i.Directory,
	}, mustJSON(map[string]any{"timestamp": float64(at), "note": "check the build"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "alarm-triggered") {
		t.Fatalf("output should tell the agent about the wake-up message, got %q", res.Output)
	}

	alarms, err := alarm.List(store.ProjectID(i.Directory), s.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 1 {
		t.Fatalf("expected 1 pending alarm, got %d", len(alarms))
	}
	if alarms[0].TriggerAt != at || alarms[0].Note != "check the build" {
		t.Fatalf("unexpected alarm: %+v", alarms[0])
	}
}

func TestAlarmToolRejectsPastTimestamp(t *testing.T) {
	i, s := setupAbortTest(t, respondEmptyStopHandler)

	_, err := tool.AlarmTool{}.Run(&tool.Ctx{
		Ctx:       t.Context(),
		SessionID: s.ID,
		Directory: i.Directory,
	}, mustJSON(map[string]any{"timestamp": float64(util.Now()/1000 - 1)}))
	if err == nil {
		t.Fatal("expected an error for a past timestamp")
	}
}

// The tool reports the trigger time in the session's device timezone (from
// the X-Timezone header) so the agent quotes the user's local time instead
// of falling back to UTC.
func TestAlarmToolReportsDeviceTime(t *testing.T) {
	i, s := setupAbortTest(t, respondEmptyStopHandler)

	// a fixed instant, far enough in the future to pass the tool's check;
	// the expected device-local rendering is computed with the same zone
	// rules the tool uses, so the test is DST-proof
	at := util.Now()/1000 + 365*24*60*60
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Unix(at, 0).In(loc).Format("Monday, 02 Jan 2006 15:04:05 MST")
	res, err := tool.AlarmTool{}.Run(&tool.Ctx{
		Ctx:       t.Context(),
		SessionID: s.ID,
		Directory: i.Directory,
		Timezone:  "America/New_York",
	}, mustJSON(map[string]any{"timestamp": float64(at)}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, want) {
		t.Fatalf("output should show the device-local time %q, got %q", want, res.Output)
	}
	if !strings.Contains(res.Output, "device time, America/New_York") {
		t.Fatalf("output should label the device timezone, got %q", res.Output)
	}
	if !strings.Contains(res.Output, "never convert it to UTC") {
		t.Fatalf("output should tell the agent not to convert to UTC, got %q", res.Output)
	}

	// without a device timezone the server's local zone is used, labelled as such
	res, err = tool.AlarmTool{}.Run(&tool.Ctx{
		Ctx:       t.Context(),
		SessionID: s.ID,
		Directory: i.Directory,
	}, mustJSON(map[string]any{"timestamp": float64(at + 60)}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "server time (device timezone unknown)") {
		t.Fatalf("fallback should be labelled as server time, got %q", res.Output)
	}
}

// End-to-end: a due alarm is claimed by the sweep and its session receives
// the "alarm-triggered" user message (plus the note), so the agent's next
// turn can continue the work.
func TestAlarmSweepWakesSession(t *testing.T) {
	i, s := setupAbortTest(t, respondEmptyStopHandler)
	pid := store.ProjectID(i.Directory)

	if _, err := alarm.Add(pid, s.ID, util.Now()/1000-1, "build finished, check output"); err != nil {
		t.Fatal(err)
	}
	sweepAlarms(i, pid, util.Now()/1000)

	// fireAlarm prompts asynchronously and the prompt takes the session
	// lock — poll until the wake-up message lands
	ok := false
	waitUntil(t, 10*time.Second, func() bool {
		msgs, err := i.Mgr.LoadMessages(s)
		if err != nil {
			return false
		}
		for _, txt := range userTexts(msgs) {
			if strings.HasPrefix(txt, "alarm-triggered") && strings.Contains(txt, "build finished, check output") {
				ok = true
			}
		}
		return ok
	})
	if !ok {
		msgs, _ := i.Mgr.LoadMessages(s)
		t.Fatalf("session was never woken up; user messages: %v", userTexts(msgs))
	}

	// the alarm is marked fired: a second sweep must not wake again
	if due, err := alarm.ClaimDue(pid, util.Now()/1000); err != nil || len(due) != 0 {
		t.Fatalf("expected no due alarms after the sweep, got %d (%v)", len(due), err)
	}
	alarms, err := alarm.List(pid, s.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 1 || !alarms[0].Fired {
		t.Fatalf("expected the alarm to be marked fired, got %+v", alarms)
	}
}

// Two racing sweeps must never double-fire: ClaimDue marks alarms under the
// package lock, so the second caller gets nothing.
func TestAlarmClaimDueIsAtomic(t *testing.T) {
	i, _ := setupAbortTest(t, respondEmptyStopHandler)
	pid := store.ProjectID(i.Directory)
	s := &session.Session{ProjectID: pid, Directory: i.Directory, Title: "alarm test"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}

	if _, err := alarm.Add(pid, s.ID, util.Now()/1000-1, ""); err != nil {
		t.Fatal(err)
	}
	first, err := alarm.ClaimDue(pid, util.Now()/1000)
	if err != nil {
		t.Fatal(err)
	}
	second, err := alarm.ClaimDue(pid, util.Now()/1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 0 {
		t.Fatalf("expected exactly one claim, got %d then %d", len(first), len(second))
	}
}

// The sweep broadcasts alarm.updated when it claims due alarms, so connected
// clients drop the session's ⏰ indicator as soon as no alarms are left.
func TestAlarmSweepBroadcastsUpdate(t *testing.T) {
	i, s := setupAbortTest(t, respondEmptyStopHandler)
	pid := store.ProjectID(i.Directory)

	ch := i.Hub.Subscribe()
	defer i.Hub.Unsubscribe(ch)

	if _, err := alarm.Add(pid, s.ID, util.Now()/1000-1, ""); err != nil {
		t.Fatal(err)
	}
	sweepAlarms(i, pid, util.Now()/1000)

	waitUntil(t, 5*time.Second, func() bool {
		for {
			select {
			case ev := <-ch:
				if ev.Type == "alarm.updated" {
					return true
				}
			default:
				return false
			}
		}
	})

	// fireAlarm prompts in a background goroutine that keeps writing session
	// files; wait for the wake-up message to land so t.TempDir's cleanup
	// doesn't race the writer (flaky "directory not empty" otherwise).
	waitUntil(t, 5*time.Second, func() bool {
		msgs, err := i.Mgr.LoadMessages(s)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			b, _ := json.Marshal(m)
			if strings.Contains(string(b), "alarm-triggered") {
				return true
			}
		}
		return false
	})
}

// Pending lists every unfired alarm of the project across sessions — the
// web UI's GET /alarm backing — and drops entries as alarms fire.
func TestAlarmPendingAcrossSessions(t *testing.T) {
	i, s := setupAbortTest(t, respondEmptyStopHandler)
	pid := store.ProjectID(i.Directory)

	if _, err := alarm.Add(pid, s.ID, util.Now()/1000+60, "first"); err != nil {
		t.Fatal(err)
	}
	other := &session.Session{ProjectID: pid, Directory: i.Directory, Title: "other"}
	if err := i.Mgr.Create(other); err != nil {
		t.Fatal(err)
	}
	if _, err := alarm.Add(pid, other.ID, util.Now()/1000+120, "second"); err != nil {
		t.Fatal(err)
	}

	pending, err := alarm.Pending(pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending alarms, got %d", len(pending))
	}

	// firing one removes it from the pending set
	if _, err := alarm.Add(pid, s.ID, util.Now()/1000-1, "due"); err != nil {
		t.Fatal(err)
	}
	if _, err := alarm.ClaimDue(pid, util.Now()/1000); err != nil {
		t.Fatal(err)
	}
	pending, err = alarm.Pending(pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected the 2 future alarms to remain pending, got %d", len(pending))
	}
	for _, a := range pending {
		if a.Fired {
			t.Fatalf("fired alarm in pending set: %+v", a)
		}
	}
}
