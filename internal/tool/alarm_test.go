package tool

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Intellexie/intellexor/internal/alarm"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

// The relative "in" form is the preferred way to schedule: it must not
// require the agent to know the current time, and it wins over an explicit
// (possibly stale) timestamp.
func TestAlarmToolRelativeIn(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	at := AlarmTool{}
	ctx := &Ctx{Directory: t.TempDir()}

	before := util.Now()
	res, err := at.Run(ctx, json.RawMessage(`{"in":"5m","note":"check build"}`))
	if err != nil {
		t.Fatalf("relative set failed: %v", err)
	}
	id, _ := res.Metadata["alarmID"].(string)
	if id == "" {
		t.Fatalf("no alarmID in result: %+v", res.Metadata)
	}
	alarms, _ := alarm.List(store.ProjectID(ctx.Directory), ctx.SessionID, false)
	if len(alarms) != 1 {
		t.Fatalf("expected 1 alarm, got %d", len(alarms))
	}
	fireIn := alarms[0].TriggerAt - before/1000
	if fireIn < 4*60 || fireIn > 6*60 {
		t.Fatalf("alarm not ~5m out: fires in %ds", fireIn)
	}

	// "in" takes precedence over an explicit timestamp.
	if _, err := at.Run(ctx, json.RawMessage(`{"in":"1m","timestamp":1}`)); err != nil {
		t.Fatalf("in should win over a stale timestamp: %v", err)
	}
	alarms, _ = alarm.List(store.ProjectID(ctx.Directory), ctx.SessionID, false)
	if len(alarms) != 2 {
		t.Fatalf("expected 2 alarms, got %d", len(alarms))
	}
	for _, a := range alarms {
		if a.TriggerAt <= util.Now()/1000 {
			t.Fatalf("alarm not scheduled in the future: %+v", a)
		}
	}

	// "after_seconds" is the plain-number relative form: the backend does
	// the now()+seconds itself.
	before = util.Now()
	if _, err := at.Run(ctx, json.RawMessage(`{"after_seconds":90,"note":"after"}`)); err != nil {
		t.Fatalf("after_seconds set failed: %v", err)
	}
	alarms, _ = alarm.List(store.ProjectID(ctx.Directory), ctx.SessionID, false)
	if len(alarms) != 3 {
		t.Fatalf("expected 3 alarms, got %d", len(alarms))
	}
	var after *alarm.Alarm
	for i := range alarms {
		if alarms[i].Note == "after" {
			after = &alarms[i]
		}
	}
	if after == nil {
		t.Fatal("after_seconds alarm not found")
	}
	// Created has second granularity, so same-second alarms sort
	// arbitrarily — match by note, not by list position.
	if fireIn = after.TriggerAt - before/1000; fireIn < 60 || fireIn > 120 {
		t.Fatalf("after_seconds alarm not ~90s out: fires in %ds", fireIn)
	}

	// a non-positive after_seconds is rejected, not silently ignored
	if _, err := at.Run(ctx, json.RawMessage(`{"after_seconds":0}`)); err == nil || !strings.Contains(err.Error(), "timestamp is required") {
		t.Fatalf("expected the missing-timestamp error for after_seconds=0, got %v", err)
	}
}

// Validation errors must be self-correcting: they tell the agent the server's
// current time and point at the relative "in" form, so a retry needs no
// extra "date" round-trip.
func TestAlarmToolErrorsAreActionable(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	at := AlarmTool{}
	ctx := &Ctx{Directory: t.TempDir()}

	_, err := at.Run(ctx, json.RawMessage(`{"timestamp":1}`))
	if err == nil || !strings.Contains(err.Error(), "server time is now") || !strings.Contains(err.Error(), `"in"`) {
		t.Fatalf("past-timestamp error not actionable: %v", err)
	}

	_, err = at.Run(ctx, json.RawMessage(`{"in":"soon"}`))
	if err == nil || !strings.Contains(err.Error(), `invalid "in" duration`) {
		t.Fatalf("bad-duration error not actionable: %v", err)
	}

	_, err = at.Run(ctx, json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), `"in"`) {
		t.Fatalf("missing-timestamp error not actionable: %v", err)
	}
}
