package tool

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Intellexie/intellexor/internal/alarm"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

// AlarmTool lets an agent set a one-shot wake-up for its own session: at
// the given timestamp the system injects a user message ("alarm-triggered")
// into the session, so the agent can continue itself after starting a long
// background job. Alarms are persisted per project and survive restarts.
type AlarmTool struct {
	// OnUpdate, when set, is called after every mutation so the instance
	// can broadcast a refresh to connected clients.
	OnUpdate func()
}

func (AlarmTool) Name() string { return "schedule_wakeup" }

func (AlarmTool) Description() string {
	return `Schedule a one-shot wake-up for this session instead of waiting. Use this INSTEAD of running "sleep" in bash, polling in a loop, or blocking on a long command: start the work in the background, schedule the wake-up, end your turn — when the timestamp is reached the system sends a user message ("alarm-triggered") into this session and you continue right where you left off. Never busy-wait; the wake-up is free and does not hold the session busy.

- "timestamp" is a unix epoch in MILLISECONDS and must be in the future
- "note" is optional context delivered with the wake-up message (what to check, what was started, ...)
- "action" defaults to "set"; use "list" to see this session's scheduled wake-ups or "cancel" (with "alarm_id") to disarm one

Example: start a build in the background, schedule a wake-up 10 minutes out with a note like "check build output", then end your turn.`
}

func (AlarmTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"timestamp": map[string]any{"type": "number", "description": "Unix epoch timestamp in milliseconds when the wake-up should fire (must be in the future)"},
			"note":      map[string]any{"type": "string", "description": "Optional context delivered with the wake-up message, e.g. what background job to check"},
			"action":    map[string]any{"type": "string", "enum": []any{"set", "list", "cancel"}, "description": "Operation: set (default), list, or cancel"},
			"alarm_id":  map[string]any{"type": "string", "description": "Wake-up ID to cancel (required for action=cancel)"},
		},
	}
}

func (t AlarmTool) Run(c *Ctx, args json.RawMessage) (Result, error) {
	var p struct {
		Timestamp float64 `json:"timestamp"`
		Note      string  `json:"note"`
		Action    string  `json:"action"`
		AlarmID   string  `json:"alarm_id"`
	}
	_ = json.Unmarshal(args, &p)
	if p.Action == "" {
		p.Action = "set"
	}
	projectID := store.ProjectID(c.Directory)

	switch p.Action {
	case "list":
		alarms, err := alarm.List(projectID, c.SessionID, false)
		if err != nil {
			return Result{}, err
		}
		// augment each alarm with the trigger time in the device timezone
		// so the agent can report it without converting from unix ms
		augmented := make([]map[string]any, 0, len(alarms))
		for _, a := range alarms {
			b, _ := json.Marshal(a)
			m := map[string]any{}
			_ = json.Unmarshal(b, &m)
			m["triggerAtLocal"] = formatDevice(a.TriggerAt, c.Timezone)
			augmented = append(augmented, m)
		}
		out, _ := json.MarshalIndent(augmented, "", "  ")
		return Result{
			Title:    "List alarms",
			Metadata: map[string]any{"count": len(alarms)},
			Output:   string(out),
		}, nil

	case "cancel":
		if p.AlarmID == "" {
			return Result{}, errors.New("alarm_id is required for action=cancel")
		}
		if err := alarm.Cancel(projectID, c.SessionID, p.AlarmID); err != nil {
			return Result{}, err
		}
		t.notify()
		return Result{
			Title:    "Cancel alarm",
			Metadata: map[string]any{"alarmID": p.AlarmID},
			Output:   "Alarm " + p.AlarmID + " cancelled.",
		}, nil

	case "set":
		if p.Timestamp <= 0 {
			return Result{}, errors.New("timestamp is required (unix epoch in milliseconds)")
		}
		if int64(p.Timestamp) <= util.Now() {
			return Result{}, errors.New("timestamp must be in the future (unix epoch in milliseconds)")
		}
		a, err := alarm.Add(projectID, c.SessionID, int64(p.Timestamp), p.Note)
		if err != nil {
			return Result{}, err
		}
		t.notify()
		fireIn := time.Duration(a.TriggerAt-util.Now()) * time.Millisecond
		local := formatDevice(a.TriggerAt, c.Timezone)
		return Result{
			Title: "Alarm at " + local,
			Metadata: map[string]any{
				"alarmID":   a.ID,
				"triggerAt": a.TriggerAt,
				"timezone":  c.Timezone,
			},
			Output: fmt.Sprintf("Wake-up %s scheduled for %s — unix ms %d, fires in %s. When it goes off you will receive a user message \"alarm-triggered\"; end your turn now and continue then. Do NOT run \"sleep\" or poll while waiting. When telling the user about this schedule, quote the time exactly as given above (their device timezone) — never convert it to UTC.", a.ID, local, a.TriggerAt, fireIn.Round(time.Second)),
		}, nil

	default:
		return Result{}, fmt.Errorf("unknown action %q (expected set, list or cancel)", p.Action)
	}
}

func (t AlarmTool) notify() {
	if t.OnUpdate != nil {
		t.OnUpdate()
	}
}

// deviceLoc resolves the session's device timezone (recorded from the web
// UI's X-Timezone header). Unknown or empty names fall back to the server's
// local zone — the best approximation when the device zone is not known.
func deviceLoc(tz string) *time.Location {
	if tz == "" {
		return time.Local
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	return time.Local
}

// formatDevice renders a timestamp in the device timezone, labelling the
// zone so the agent can pass it on to the user without falling back to UTC.
func formatDevice(ms int64, tz string) string {
	loc := deviceLoc(tz)
	t := time.UnixMilli(ms).In(loc)
	label := "device time"
	if tz != "" && loc != time.Local {
		label = "device time, " + tz
	} else if tz == "" {
		label = "server time (device timezone unknown)"
	}
	return t.Format("Monday, 02 Jan 2006 15:04:05 MST") + " (" + label + ")"
}
