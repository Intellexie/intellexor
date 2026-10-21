// Package alarm stores per-project one-shot alarms: a timestamp at which a
// session should be woken up with a message on behalf of the user. Alarms
// live in a single JSON file under the data dir, keyed by project ID (the
// same keying sessions, the board and cron jobs use), so they survive
// restarts — an alarm that came due while the server was down fires right
// after startup. A background sweep in internal/app claims due alarms and
// injects the wake message into their session.
package alarm

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

// ErrNotFound is returned when an alarm ID does not exist.
var ErrNotFound = errors.New("alarm not found")

// Alarm is a single one-shot wake-up for a session. TriggerAt is a unix
// timestamp in SECONDS; when it is reached, the session receives a user
// message ("alarm-triggered", plus Note as context when set) so the agent
// can continue itself after a long background job. Fired alarms are kept
// (marked) so a sweep racing a restart can never double-fire; they are
// pruned lazily on the next Add.
type Alarm struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	TriggerAt int64  `json:"triggerAt"`
	Note      string `json:"note,omitempty"`
	Created   int64  `json:"created"`
	Fired     bool   `json:"fired"`
}

var mu sync.Mutex

// maxKept bounds the per-project alarm file: when a new alarm is added,
// fired alarms beyond the most recent maxKept entries are dropped.
const maxKept = 200

func file(projectID string) string {
	return filepath.Join(store.DataDir(), "storage", "alarm", projectID+".json")
}

// msThreshold separates legacy millisecond timestamps from seconds: any
// value above it is ~year 5138+ in seconds, but a plausible ms epoch.
// Alarms written before the seconds refactor stored TriggerAt (and
// Created) in milliseconds; without migration they would look infinitely
// far in the future and silently never fire.
const msThreshold = 1e11

// migrateLegacy converts pre-refactor millisecond timestamps to seconds.
func migrateLegacy(alarms []Alarm) {
	for i := range alarms {
		if alarms[i].TriggerAt > msThreshold {
			alarms[i].TriggerAt /= 1000
		}
		if alarms[i].Created > msThreshold {
			alarms[i].Created /= 1000
		}
	}
}

func load(projectID string) ([]Alarm, error) {
	var alarms []Alarm
	b, err := os.ReadFile(file(projectID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &alarms); err != nil {
		return nil, err
	}
	migrateLegacy(alarms)
	return alarms, nil
}

func save(projectID string, alarms []Alarm) error {
	path := file(projectID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(alarms, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Add arms a new alarm for sessionID. triggerAt is a unix timestamp in
// SECONDS. The timestamp is not validated here: the tool rejects
// future-less timestamps, while tests and recovery paths may legitimately
// add already-due alarms (they fire on the next sweep).
func Add(projectID, sessionID string, triggerAt int64, note string) (Alarm, error) {
	mu.Lock()
	defer mu.Unlock()
	alarms, err := load(projectID)
	if err != nil {
		return Alarm{}, err
	}
	a := Alarm{
		ID:        util.ID("alm"),
		SessionID: sessionID,
		TriggerAt: triggerAt,
		Note:      note,
		Created:   util.Now() / 1000,
	}
	alarms = append(alarms, a)
	// prune: drop old fired alarms so the file cannot grow unbounded
	var kept []Alarm
	fired := 0
	for i := len(alarms) - 1; i >= 0; i-- {
		if alarms[i].Fired {
			fired++
			if fired > maxKept {
				continue
			}
		}
		kept = append([]Alarm{alarms[i]}, kept...)
	}
	if err := save(projectID, kept); err != nil {
		return Alarm{}, err
	}
	return a, nil
}

// List returns the session's alarms, newest first. Fired alarms are
// included (so the agent can see what already rang) unless pendingOnly.
func List(projectID, sessionID string, pendingOnly bool) ([]Alarm, error) {
	mu.Lock()
	defer mu.Unlock()
	alarms, err := load(projectID)
	if err != nil {
		return nil, err
	}
	var out []Alarm
	for _, a := range alarms {
		if a.SessionID != sessionID {
			continue
		}
		if pendingOnly && a.Fired {
			continue
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	if out == nil {
		out = []Alarm{}
	}
	return out, nil
}

// Pending returns every unfired alarm of the project across all sessions,
// newest first. The web UI uses it to show which sessions have a wake-up
// scheduled (the ⏰ indicator in the header, tab and sidebar).
func Pending(projectID string) ([]Alarm, error) {
	mu.Lock()
	defer mu.Unlock()
	alarms, err := load(projectID)
	if err != nil {
		return nil, err
	}
	var out []Alarm
	for _, a := range alarms {
		if !a.Fired {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	if out == nil {
		out = []Alarm{}
	}
	return out, nil
}

// Cancel disarms one of the session's alarms. Only unfired alarms of the
// given session can be cancelled — a session cannot touch another
// session's alarms, and fired ones are history.
func Cancel(projectID, sessionID, alarmID string) error {
	mu.Lock()
	defer mu.Unlock()
	alarms, err := load(projectID)
	if err != nil {
		return err
	}
	idx := -1
	for i := range alarms {
		if alarms[i].ID == alarmID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrNotFound
	}
	if alarms[idx].SessionID != sessionID {
		return ErrNotFound
	}
	if alarms[idx].Fired {
		return errors.New("alarm already fired")
	}
	alarms = append(alarms[:idx], alarms[idx+1:]...)
	return save(projectID, alarms)
}

// ClaimDue atomically marks every due unfired alarm of the project as
// fired and returns them. The re-check and the mark happen under the
// package lock, so two racing sweeps (or a sweep and a restart) can never
// double-fire an alarm.
func ClaimDue(projectID string, now int64) ([]Alarm, error) {
	mu.Lock()
	defer mu.Unlock()
	alarms, err := load(projectID)
	if err != nil {
		return nil, err
	}
	var due []Alarm
	changed := false
	for i := range alarms {
		if alarms[i].Fired || alarms[i].TriggerAt > now {
			continue
		}
		alarms[i].Fired = true
		changed = true
		due = append(due, alarms[i])
	}
	if changed {
		if err := save(projectID, alarms); err != nil {
			return nil, err
		}
	}
	return due, nil
}

// Claim atomically marks the named unfired alarm as fired and returns it —
// the "trigger now" primitive behind the web UI's button. Unlike ClaimDue
// it ignores TriggerAt: the user explicitly asked for an immediate wake-up.
// A second claim (or a racing sweep) finds the alarm already fired and
// returns ErrNotFound, so an alarm can never double-fire.
func Claim(projectID, alarmID string) (Alarm, error) {
	mu.Lock()
	defer mu.Unlock()
	alarms, err := load(projectID)
	if err != nil {
		return Alarm{}, err
	}
	for i := range alarms {
		if alarms[i].ID != alarmID {
			continue
		}
		if alarms[i].Fired {
			return Alarm{}, ErrNotFound
		}
		alarms[i].Fired = true
		if err := save(projectID, alarms); err != nil {
			return Alarm{}, err
		}
		return alarms[i], nil
	}
	return Alarm{}, ErrNotFound
}

// Delete removes an alarm regardless of session or fired state — the
// web UI's delete button. Unlike Cancel (session-scoped, tool-facing) it
// is an explicit user action on a specific alarm ID.
func Delete(projectID, alarmID string) error {
	mu.Lock()
	defer mu.Unlock()
	alarms, err := load(projectID)
	if err != nil {
		return err
	}
	for i := range alarms {
		if alarms[i].ID == alarmID {
			alarms = append(alarms[:i], alarms[i+1:]...)
			return save(projectID, alarms)
		}
	}
	return ErrNotFound
}

// Import re-arms exported alarms under a (new) session ID. Past-due
// untriggered alarms are marked fired instead of being carried over
// armed: an import must never replay stale wake-ups — the sweep would
// otherwise fire them within one tick. Future alarms stay armed with
// their original trigger time; IDs are re-minted so an import into the
// source project cannot collide with the originals.
func Import(projectID, sessionID string, alarms []Alarm, now int64) error {
	if len(alarms) == 0 {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	existing, err := load(projectID)
	if err != nil {
		return err
	}
	for _, a := range alarms {
		fired := a.Fired || a.TriggerAt <= now
		existing = append(existing, Alarm{
			ID:        util.ID("alm"),
			SessionID: sessionID,
			TriggerAt: a.TriggerAt,
			Note:      a.Note,
			Created:   a.Created,
			Fired:     fired,
		})
	}
	return save(projectID, existing)
}
