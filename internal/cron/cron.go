// Package cron stores per-project cron jobs: a prompt that automatically
// starts a fresh agent session on a schedule. Jobs live in a single JSON
// file under the data dir, keyed by project ID (the same keying sessions
// and the board use), so they survive restarts and are shared by every
// client connected to the same project. A background sweep in internal/app
// claims due jobs and starts their sessions.
package cron

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

// ErrNotFound is returned when a job ID does not exist.
var ErrNotFound = errors.New("cron job not found")

// ErrBadSpec is returned when a schedule expression cannot be parsed.
var ErrBadSpec = errors.New("invalid cron expression")

// Job is a single cron job. Spec is a standard 5-field cron expression
// (minute hour day-of-month month day-of-week, server local time) or one of
// the @macros (@hourly, @daily, @midnight, @weekly, @monthly, @yearly,
// @annually) or "@every <duration>" (e.g. "@every 30m"). Paused jobs are
// skipped by the sweep; resuming re-arms them for the next run without
// firing immediately. Sessions lists the most recent runs' session IDs
// (newest last, capped) so the UI can offer a View button for the latest
// active — or last finished — session.
type Job struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Prompt   string   `json:"prompt"`
	Spec     string   `json:"spec"`
	Paused   bool     `json:"paused"`
	Created  int64    `json:"created"`
	LastRun  int64    `json:"lastRun,omitempty"`
	NextRun  int64    `json:"nextRun,omitempty"`
	Sessions []string `json:"sessions,omitempty"`
	Runs     int      `json:"runs"`
}

var mu sync.Mutex

// maxTrackedSessions bounds the per-job session list: old entries fall off
// the front as new runs append.
const maxTrackedSessions = 20

func file(projectID string) string {
	return filepath.Join(store.DataDir(), "storage", "cron", projectID+".json")
}

func load(projectID string) ([]Job, error) {
	var jobs []Job
	b, err := os.ReadFile(file(projectID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

func save(projectID string, jobs []Job) error {
	path := file(projectID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Get returns every job of the project, oldest first (creation order).
func Get(projectID string) ([]Job, error) {
	mu.Lock()
	defer mu.Unlock()
	jobs, err := load(projectID)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].Created < jobs[j].Created })
	if jobs == nil {
		jobs = []Job{}
	}
	return jobs, nil
}

// Add creates a job. The spec is validated up front and the first NextRun
// is computed from now, so the job is armed immediately.
func Add(projectID, title, prompt, spec string) (Job, error) {
	spec = strings.TrimSpace(spec)
	if _, err := Next(spec, time.Now()); err != nil {
		return Job{}, err
	}
	mu.Lock()
	defer mu.Unlock()
	jobs, err := load(projectID)
	if err != nil {
		return Job{}, err
	}
	job := Job{
		ID:      util.ID("cron"),
		Title:   title,
		Prompt:  prompt,
		Spec:    spec,
		Created: util.Now(),
	}
	if job.NextRun, err = nextMillis(spec, util.Now()); err != nil {
		return Job{}, err
	}
	jobs = append(jobs, job)
	if err := save(projectID, jobs); err != nil {
		return Job{}, err
	}
	return job, nil
}

// Update edits a job's title, prompt and/or spec. A new spec re-arms the
// job: NextRun is recomputed from now (an already-earned schedule of the old
// spec makes no sense for the new one).
func Update(projectID, id string, title, prompt, spec *string) (Job, error) {
	mu.Lock()
	defer mu.Unlock()
	jobs, err := load(projectID)
	if err != nil {
		return Job{}, err
	}
	idx := -1
	for i := range jobs {
		if jobs[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Job{}, ErrNotFound
	}
	if title != nil {
		jobs[idx].Title = *title
	}
	if prompt != nil {
		jobs[idx].Prompt = *prompt
	}
	if spec != nil {
		s := strings.TrimSpace(*spec)
		if _, err := Next(s, time.Now()); err != nil {
			return Job{}, err
		}
		jobs[idx].Spec = s
		nr, err := nextMillis(s, util.Now())
		if err != nil {
			return Job{}, err
		}
		jobs[idx].NextRun = nr
	}
	if err := save(projectID, jobs); err != nil {
		return Job{}, err
	}
	return jobs[idx], nil
}

// SetPaused pauses (true) or resumes (false) a job. Pausing only stops
// future runs — a session already started by an earlier run keeps working.
// Resuming re-arms the job for the next run from now: it does NOT fire
// immediately, even if the schedule came due while the job was paused.
func SetPaused(projectID, id string, paused bool) (Job, error) {
	mu.Lock()
	defer mu.Unlock()
	jobs, err := load(projectID)
	if err != nil {
		return Job{}, err
	}
	idx := -1
	for i := range jobs {
		if jobs[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Job{}, ErrNotFound
	}
	jobs[idx].Paused = paused
	if !paused {
		nr, err := nextMillis(jobs[idx].Spec, util.Now())
		if err != nil {
			return Job{}, err
		}
		jobs[idx].NextRun = nr
	}
	if err := save(projectID, jobs); err != nil {
		return Job{}, err
	}
	return jobs[idx], nil
}

// Delete removes a job.
func Delete(projectID, id string) error {
	mu.Lock()
	defer mu.Unlock()
	jobs, err := load(projectID)
	if err != nil {
		return err
	}
	kept := jobs[:0]
	found := false
	for _, j := range jobs {
		if j.ID == id {
			found = true
			continue
		}
		kept = append(kept, j)
	}
	if !found {
		return ErrNotFound
	}
	return save(projectID, kept)
}

// ClaimDue atomically claims one run of a job: it re-checks that the job is
// unpaused and due (the sweep re-checks here so two racing sweeps — or a
// sweep and a manual trigger — can never double-start a run), stamps
// LastRun/Runs, advances NextRun and reports whether the caller won the
// claim. A NextRun of 0 (legacy/corrupt data) counts as due so the job
// self-heals on the next sweep.
func ClaimDue(projectID, id string, now int64) (Job, bool, error) {
	mu.Lock()
	defer mu.Unlock()
	jobs, err := load(projectID)
	if err != nil {
		return Job{}, false, err
	}
	idx := -1
	for i := range jobs {
		if jobs[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Job{}, false, ErrNotFound
	}
	if jobs[idx].Paused {
		return Job{}, false, nil
	}
	if jobs[idx].NextRun != 0 && now < jobs[idx].NextRun {
		return Job{}, false, nil
	}
	nr, err := nextMillis(jobs[idx].Spec, now)
	if err != nil {
		return Job{}, false, err
	}
	jobs[idx].LastRun = now
	jobs[idx].NextRun = nr
	jobs[idx].Runs++
	if err := save(projectID, jobs); err != nil {
		return Job{}, false, err
	}
	return jobs[idx], true, nil
}

// SetSession records a run's session ID (newest last). Called after the
// sweep created the session, so the UI's View button can target the latest
// active — or last finished — run.
func SetSession(projectID, id, sessionID string) error {
	mu.Lock()
	defer mu.Unlock()
	jobs, err := load(projectID)
	if err != nil {
		return err
	}
	for i := range jobs {
		if jobs[i].ID != id {
			continue
		}
		jobs[i].Sessions = append(jobs[i].Sessions, sessionID)
		if len(jobs[i].Sessions) > maxTrackedSessions {
			jobs[i].Sessions = jobs[i].Sessions[len(jobs[i].Sessions)-maxTrackedSessions:]
		}
		return save(projectID, jobs)
	}
	return ErrNotFound
}

func nextMillis(spec string, now int64) (int64, error) {
	t, err := Next(spec, time.UnixMilli(now))
	if err != nil {
		return 0, err
	}
	return t.UnixMilli(), nil
}

// ---- schedule parsing ----

// Next returns the next fire time of spec strictly after `after`, in the
// server's local timezone. Supported: standard 5-field expressions with
// `*`, lists, ranges, steps and 3-letter month/day names; the @hourly,
// @daily, @midnight, @weekly, @monthly, @yearly and @annually macros; and
// "@every <duration>" (time.ParseDuration) which fires relative to `after`.
func Next(spec string, after time.Time) (time.Time, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return time.Time{}, fmt.Errorf("%w: empty expression", ErrBadSpec)
	}
	if strings.HasPrefix(spec, "@") {
		return nextMacro(spec, after)
	}
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return time.Time{}, fmt.Errorf("%w: expected 5 fields (minute hour day month weekday), got %d", ErrBadSpec, len(fields))
	}
	minutes, err := parseField(fields[0], 0, 59, nil)
	if err != nil {
		return time.Time{}, err
	}
	hours, err := parseField(fields[1], 0, 23, nil)
	if err != nil {
		return time.Time{}, err
	}
	dom, err := parseField(fields[2], 1, 31, nil)
	if err != nil {
		return time.Time{}, err
	}
	months, err := parseField(fields[3], 1, 12, monthNames)
	if err != nil {
		return time.Time{}, err
	}
	dow, err := parseField(fields[4], 0, 7, dayNames)
	if err != nil {
		return time.Time{}, err
	}
	// 7 means Sunday in the weekday field
	if dow[7] {
		dow[0] = true
	}
	domStar := fields[2] == "*"
	dowStar := fields[4] == "*"

	// Walk days forward from `after`'s date; on the first matching day,
	// pick the earliest matching hour:minute at or after `after`. Four
	// years covers every calendar combination (leap years included); a
	// spec that matches no day in that span can never fire.
	start := after.Truncate(time.Minute)
	for dayOffset := 0; dayOffset < 366*4; dayOffset++ {
		day := start.AddDate(0, 0, dayOffset)
		if !months[int(day.Month())] { // the set is keyed by month value (1-12)
			continue
		}
		domOK := dom[day.Day()]
		dowOK := dow[int(day.Weekday())]
		// vixie-cron rule: when both day fields are restricted the day
		// matches if EITHER matches; a star field imposes no constraint.
		switch {
		case domStar && dowStar:
		case domStar:
			if !dowOK {
				continue
			}
		case dowStar:
			if !domOK {
				continue
			}
		default:
			if !domOK && !dowOK {
				continue
			}
		}
		for h := 0; h < 24; h++ {
			if !hours[h] {
				continue
			}
			cand := time.Date(day.Year(), day.Month(), day.Day(), h, 0, 0, 0, after.Location())
			for m := 0; m < 60; m++ {
				if !minutes[m] {
					continue
				}
				t := cand.Add(time.Duration(m) * time.Minute)
				if t.After(start) {
					return t, nil
				}
			}
		}
	}
	return time.Time{}, fmt.Errorf("%w: expression never matches", ErrBadSpec)
}

func nextMacro(spec string, after time.Time) (time.Time, error) {
	name, arg, _ := strings.Cut(strings.ToLower(spec[1:]), " ")
	arg = strings.TrimSpace(arg)
	switch name {
	case "hourly":
		return Next("0 * * * *", after)
	case "daily", "midnight":
		return Next("0 0 * * *", after)
	case "weekly":
		return Next("0 0 * * 0", after)
	case "monthly":
		return Next("0 0 1 * *", after)
	case "yearly", "annually":
		return Next("0 0 1 1 *", after)
	case "every":
		d, err := time.ParseDuration(arg)
		if err != nil || d <= 0 {
			return time.Time{}, fmt.Errorf("%w: @every needs a positive duration, e.g. \"@every 30m\"", ErrBadSpec)
		}
		return after.Add(d), nil
	default:
		return time.Time{}, fmt.Errorf("%w: unknown macro @%s", ErrBadSpec, name)
	}
}

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var dayNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// parseField parses one cron field into a boolean set over [min, max].
// Terms: `*`, a number/name, a range `a-b`, each optionally with a step
// `/n`. A step on `*` starts at the field minimum; on a range it starts at
// the range's lower bound; `a/step` means a-max/step.
func parseField(field string, min, max int, names map[string]int) ([64]bool, error) {
	var set [64]bool
	if max-min >= len(set) {
		return set, fmt.Errorf("%w: field range too wide", ErrBadSpec)
	}
	for _, term := range strings.Split(field, ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			return set, fmt.Errorf("%w: empty term in %q", ErrBadSpec, field)
		}
		lo, hi, step := min, max, 1
		switch {
		case term == "*":
			// full range, step 1
		case strings.HasPrefix(term, "*/"):
			n, err := strconv.Atoi(term[2:])
			if err != nil || n <= 0 {
				return set, fmt.Errorf("%w: bad step in %q", ErrBadSpec, term)
			}
			step = n
		default:
			rangePart, stepPart, hasStep := strings.Cut(term, "/")
			if hasStep {
				n, err := strconv.Atoi(stepPart)
				if err != nil || n <= 0 {
					return set, fmt.Errorf("%w: bad step in %q", ErrBadSpec, term)
				}
				step = n
			}
			loS, hiS, isRange := strings.Cut(rangePart, "-")
			lo = resolveValue(loS, min, max, names)
			if lo < 0 {
				return set, fmt.Errorf("%w: bad value %q", ErrBadSpec, loS)
			}
			if isRange {
				hi = resolveValue(hiS, min, max, names)
				if hi < 0 {
					return set, fmt.Errorf("%w: bad value %q", ErrBadSpec, hiS)
				}
			} else {
				hi = lo
				if hasStep {
					hi = max // `a/step` means a-max/step
				}
			}
			if lo > hi {
				return set, fmt.Errorf("%w: inverted range %q", ErrBadSpec, term)
			}
		}
		for v := lo; v <= hi; v += step {
			if v < min || v > max {
				return set, fmt.Errorf("%w: value %d out of range %d-%d", ErrBadSpec, v, min, max)
			}
			set[v] = true
		}
	}
	return set, nil
}

// resolveValue resolves one endpoint of a term: a plain number or a
// 3-letter name (when the field has names). Returns -1 when invalid.
func resolveValue(s string, min, max int, names map[string]int) int {
	if names != nil {
		if v, ok := names[strings.ToLower(s)]; ok {
			return v
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	// a bare 7 in the weekday field is accepted here (normalized later);
	// anything else out of range is invalid
	if (v < min || v > max) && !(min == 0 && max == 7 && v == 7) {
		return -1
	}
	return v
}
