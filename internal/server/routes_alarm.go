package server

import (
	"errors"
	"net/http"

	"github.com/Intellexie/intellexor/internal/alarm"
	"github.com/Intellexie/intellexor/internal/store"
)

// Alarms: one-shot wake-ups an agent schedules for its own session via the
// schedule_wakeup tool. The web UI lists the project's pending alarms to
// show a ⏰ indicator on sessions that have one armed; the indicator must
// disappear as soon as the last alarm is gone, so every mutation (tool
// set/cancel and the background sweep firing due alarms) broadcasts
// alarm.updated on the instance hub.

func (s *Server) listAlarms(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	alarms, err := alarm.Pending(store.ProjectID(in.Directory))
	if err != nil {
		return badRequest(err.Error())
	}
	writeJSON(w, 200, alarms)
	return nil
}

// sessionAlarms lists every alarm of one session — pending and already
// fired — for the web UI's wake-up modal (the clickable ⏰ indicator).
func (s *Server) sessionAlarms(w http.ResponseWriter, r *http.Request) *apiError {
	in, sess, err := s.resolveSession(r, r.PathValue("sessionID"))
	if err != nil {
		return err
	}
	alarms, lerr := alarm.List(store.ProjectID(in.Directory), sess.ID, false)
	if lerr != nil {
		return badRequest(lerr.Error())
	}
	writeJSON(w, 200, alarms)
	return nil
}

// triggerAlarm fires a wake-up immediately ("Trigger now" in the modal):
// the alarm is atomically marked fired, then the wake message is injected
// into its session exactly like a sweep would.
func (s *Server) triggerAlarm(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	a, err := alarm.Claim(store.ProjectID(in.Directory), r.PathValue("alarmID"))
	if err != nil {
		if errors.Is(err, alarm.ErrNotFound) {
			return notFound("Alarm not found: " + r.PathValue("alarmID"))
		}
		return badRequest(err.Error())
	}
	in.FireAlarmNow(a)
	writeJSON(w, 200, a)
	return nil
}

// deleteAlarm removes a wake-up ("Delete" in the modal) — pending or
// already fired, any session of the project.
func (s *Server) deleteAlarm(w http.ResponseWriter, r *http.Request) *apiError {
	in := s.Instance(directoryOf(r))
	if err := alarm.Delete(store.ProjectID(in.Directory), r.PathValue("alarmID")); err != nil {
		if errors.Is(err, alarm.ErrNotFound) {
			return notFound("Alarm not found: " + r.PathValue("alarmID"))
		}
		return badRequest(err.Error())
	}
	in.PublishAlarmUpdate()
	writeJSON(w, 200, nil)
	return nil
}
