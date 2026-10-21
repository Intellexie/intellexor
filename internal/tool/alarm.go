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

- "in" is a relative duration like "5m", "30m" or "2h" — the PREFERRED way to schedule, since it needs no clock knowledge. "after_seconds" is the same idea as a plain number of seconds (e.g. 300). Only pass an absolute "timestamp" (unix epoch in SECONDS, must be in the future) when a specific wall-clock time is required
- "note" is optional context delivered with the wake-up message (what to check, what was started, ...)
- "action" defaults to "set"; use "list" to see this session's scheduled wake-ups or "cancel" (with "alarm_id") to disarm one

Example: start a build in the background, schedule a wake-up with {"in": "10m"} and a note like "check build output", then end your turn.`
}

func (AlarmTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"timestamp":     map[string]any{"type": "number", "description": "Unix epoch timestamp in SECONDS when the wake-up should fire (must be in the future). Prefer \"in\" or \"after_seconds\" for relative scheduling"},
			"in":            map[string]any{"type": "string", "description": "Relative duration until the wake-up fires, e.g. \"5m\", \"30m\", \"2h\" — preferred over an absolute timestamp"},
			"after_seconds": map[string]any{"type": "number", "description": "Fire this many seconds from now, e.g. 300 — same as \"in\" but as a plain number; preferred over an absolute timestamp"},
			"note":          map[string]any{"type": "string", "description": "Optional context delivered with the wake-up message, e.g. what background job to check"},
			"action":        map[string]any{"type": "string", "enum": []any{"set", "list", "cancel"}, "description": "Operation: set (default), list, or cancel"},
			"alarm_id":      map[string]any{"type": "string", "description": "Wake-up ID to cancel (required for action=cancel)"},
		},
	}
}

func (t AlarmTool) Run(c *Ctx, args json.RawMessage) (Result, error) {
	var p struct {
		Timestamp    float64 `json:"timestamp"`
		In           string  `json:"in"`
		AfterSeconds float64 `json:"after_seconds"`
		Note         string  `json:"note"`
		Action       string  `json:"action"`
		AlarmID      string  `json:"alarm_id"`
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
		// so the agent can report it without converting from unix seconds
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
		// Relative forms are preferred: they need no clock knowledge, so
		// the agent cannot guess a stale/hallucinated "now" and fail
		// validation. They win over an explicit timestamp.
		if p.AfterSeconds > 0 {
			p.Timestamp = float64(util.Now()/1000 + int64(p.AfterSeconds))
		}
		if p.In != "" {
			d, err := time.ParseDuration(p.In)
			if err != nil {
				return Result{}, fmt.Errorf("invalid \"in\" duration %q: %v (examples: \"5m\", \"30m\", \"2h\")", p.In, err)
			}
			if d <= 0 {
				return Result{}, fmt.Errorf("\"in\" must be positive, got %q", p.In)
			}
			p.Timestamp = float64(util.Now()/1000 + int64(d.Seconds()))
		}
		if p.Timestamp <= 0 {
			return Result{}, errors.New("timestamp is required (unix epoch in seconds) — or pass a relative \"in\" duration like \"5m\" or \"after_seconds\": 300 instead")
		}
		if int64(p.Timestamp) <= util.Now()/1000 {
			return Result{}, fmt.Errorf("timestamp must be in the future (unix epoch in seconds); server time is now %d — or pass a relative \"in\" duration like \"5m\" or \"after_seconds\": 300 instead", util.Now()/1000)
		}
		a, err := alarm.Add(projectID, c.SessionID, int64(p.Timestamp), p.Note)
		if err != nil {
			return Result{}, err
		}
		t.notify()
		fireIn := time.Duration(a.TriggerAt-util.Now()/1000) * time.Second
		local := formatDevice(a.TriggerAt, c.Timezone)
		return Result{
			Title: "Alarm at " + local,
			Metadata: map[string]any{
				"alarmID":   a.ID,
				"triggerAt": a.TriggerAt,
				"timezone":  c.Timezone,
			},
			Output: fmt.Sprintf("Wake-up %s scheduled for %s — unix seconds %d, fires in %s. When it goes off you will receive a user message \"alarm-triggered\"; end your turn now and continue then. Do NOT run \"sleep\" or poll while waiting. When telling the user about this schedule, quote the time exactly as given above (their device timezone) — never convert it to UTC.", a.ID, local, a.TriggerAt, fireIn.Round(time.Second)),
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

// formatDevice renders a timestamp (unix seconds) in the device timezone,
// labelling the zone so the agent can pass it on to the user without
// falling back to UTC.
func formatDevice(sec int64, tz string) string {
	loc := deviceLoc(tz)
	t := time.Unix(sec, 0).In(loc)
	label := "device time"
	if tz != "" && loc != time.Local {
		label = "device time, " + tz
	} else if tz == "" {
		label = "server time (device timezone unknown)"
	}
	return t.Format("Monday, 02 Jan 2006 15:04:05 MST") + " (" + label + ")"
}
