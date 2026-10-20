package server

import (
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
