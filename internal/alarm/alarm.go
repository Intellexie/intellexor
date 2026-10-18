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
// millisecond timestamp; when it is reached, the session receives a user
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

// Add arms a new alarm for sessionID. The timestamp is not validated
// here: the tool rejects future-less timestamps, while tests and recovery
// paths may legitimately add already-due alarms (they fire on the next
// sweep).
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
		Created:   util.Now(),
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
