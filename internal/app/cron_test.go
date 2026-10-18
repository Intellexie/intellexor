package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Intellexie/intellexor/internal/cron"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

// The cron sweep starts a fresh session per due job and prompts it with the
// job's prompt. The fake provider answers with an empty completion so the
// prompt itself succeeds; the assertions are about the session and the job
// bookkeeping, not the model output.

func setupCronTest(t *testing.T) *Instance {
	t.Helper()
	i, _ := setupAbortTest(t, func(w http.ResponseWriter, r *http.Request) { respondEmptyStop(w) })
	// registerInstance started the process-level background sweep; let its
	// initial pass drain before any job exists, so it can never claim the
	// jobs added below (the next pass is cronTick away, longer than the test).
	time.Sleep(150 * time.Millisecond)
	return i
}

func firstUserText(t *testing.T, i *Instance, sid string) string {
	t.Helper()
	sess, err := i.Mgr.Find(sid)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := i.Mgr.LoadMessages(sess)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if _, ok := m.Info.(session.UserMessage); !ok {
			continue
		}
		for _, p := range m.Parts {
			if p.Type == "text" {
				return p.Text
			}
		}
	}
	return ""
}

// sessionIDs lists the current session IDs (the setup session included).
func sessionIDs(i *Instance) map[string]bool {
	out := map[string]bool{}
	for _, s := range i.Mgr.ListAll() {
		out[s.ID] = true
	}
	return out
}

// runCronJob creates the session, records it on the job and prompts it with
// the job's prompt.
func TestRunCronJob(t *testing.T) {
	i := setupCronTest(t)
	pid := store.ProjectID(i.Directory)

	job, err := cron.Add(pid, "Cron test job", "hello from cron", "@every 1h")
	if err != nil {
		t.Fatal(err)
	}
	// force the claim: pretend an hour passed so the job is due
	claimed, ok, err := cron.ClaimDue(pid, job.ID, util.Now()+2*time.Hour.Milliseconds())
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	before := sessionIDs(i)
	runCronJob(i, pid, claimed)

	sessions := i.Mgr.ListAll()
	var fresh *session.Session
	for _, s := range sessions {
		if !before[s.ID] {
			if fresh != nil {
				t.Fatalf("unexpected extra session %q", s.Title)
			}
			fresh = s
		}
	}
	if fresh == nil {
		t.Fatal("runCronJob started no session")
	}
	if got := firstUserText(t, i, fresh.ID); got != "hello from cron" {
		t.Fatalf("prompt text = %q", got)
	}

	jobs, err := cron.Get(pid)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs: %v %d", err, len(jobs))
	}
	if len(jobs[0].Sessions) != 1 || jobs[0].Sessions[0] != fresh.ID {
		t.Fatalf("job sessions not linked: %+v", jobs[0].Sessions)
	}
}

// forceDue rewrites the job's NextRun into the past directly in the job
// file — the public API always arms jobs into the future, and a short
// "@every" spec would re-arm instantly and let the process-level background
// sweep (tickers from earlier tests survive in the process) double-fire.
func forceDue(t *testing.T, pid, jobID string) {
	t.Helper()
	path := filepath.Join(store.DataDir(), "storage", "cron", pid+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var jobs []cron.Job
	if err := json.Unmarshal(b, &jobs); err != nil {
		t.Fatal(err)
	}
	for i := range jobs {
		if jobs[i].ID == jobID {
			jobs[i].NextRun = util.Now() - 1000
		}
	}
	b, err = json.Marshal(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The sweep claims only due, unpaused jobs, and a claimed job is never
// double-started by a second sweep.
func TestSweepCrons(t *testing.T) {
	i := setupCronTest(t)
	pid := store.ProjectID(i.Directory)

	due, err := cron.Add(pid, "due job", "p1", "@hourly")
	if err != nil {
		t.Fatal(err)
	}
	future, err := cron.Add(pid, "future job", "p2", "@hourly")
	if err != nil {
		t.Fatal(err)
	}
	paused, err := cron.Add(pid, "paused job", "p3", "@hourly")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cron.SetPaused(pid, paused.ID, true); err != nil {
		t.Fatal(err)
	}
	forceDue(t, pid, due.ID)

	before := sessionIDs(i)
	sweepCrons(i, pid, util.Now())

	// the run starts asynchronously — poll until the due job has a recorded
	// session, then wait for the prompt to finish (user + assistant stored)
	// so no writes race cleanup
	deadline := time.Now().Add(5 * time.Second)
	var freshID string
	for time.Now().Before(deadline) {
		jobs, _ := cron.Get(pid)
		for _, j := range jobs {
			if j.ID == due.ID && len(j.Sessions) > 0 {
				freshID = j.Sessions[len(j.Sessions)-1]
			}
		}
		if freshID != "" {
			sess, err := i.Mgr.Find(freshID)
			if err == nil {
				msgs, _ := i.Mgr.LoadMessages(sess)
				if len(msgs) >= 2 {
					break
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if freshID == "" {
		t.Fatal("sweep started no session")
	}
	if got := firstUserText(t, i, freshID); got != "p1" {
		t.Fatalf("prompt text = %q", got)
	}

	// a second sweep must not double-start: the claim was consumed
	sweepCrons(i, pid, util.Now())
	time.Sleep(100 * time.Millisecond)
	for _, s := range i.Mgr.ListAll() {
		if !before[s.ID] && s.ID != freshID {
			t.Fatalf("double start: extra session %q", s.Title)
		}
	}

	jobs, _ := cron.Get(pid)
	byID := map[string]cron.Job{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	if byID[due.ID].Runs != 1 {
		t.Fatalf("due job runs = %d", byID[due.ID].Runs)
	}
	if byID[future.ID].Runs != 0 || len(byID[future.ID].Sessions) != 0 {
		t.Fatalf("future job should be untouched: %+v", byID[future.ID])
	}
	if byID[paused.ID].Runs != 0 {
		t.Fatalf("paused job should not run: %+v", byID[paused.ID])
	}
}

// A cron run must pin the same model a manual prompt would: the
// UI-equivalent default on the session and the prompt — not the prompt-time
// config fallback, which may name a provider the user cannot use. Here the
// config model points at a provider that does not exist; the run must still
// succeed on the connected default.
func TestRunCronJobPinsDefaultModel(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	srv := httptest.NewServer(textSSE("Working on it."))
	t.Cleanup(srv.Close)

	work := t.TempDir()
	cfg := fmt.Sprintf(`{
		"model": "minimax/bad",
		"small_model": "test/m",
		"provider": {"test": {"name": "test",
			"options": {"baseURL": %q, "apiKey": "k"},
			"models": {"m": {"name": "m", "tool_call": true,
				"limit": {"context": 100000, "output": 4096}}}}}
	}`, srv.URL+"/v1")
	if err := os.WriteFile(filepath.Join(work, "intellexor.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	i := New(work)
	pid := store.ProjectID(i.Directory)

	job, err := cron.Add(pid, "Cron model test", "hello from cron", "@every 1h")
	if err != nil {
		t.Fatal(err)
	}
	// force the claim: pretend an hour passed so the job is due
	claimed, ok, err := cron.ClaimDue(pid, job.ID, util.Now()+2*time.Hour.Milliseconds())
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	runCronJob(i, pid, claimed) // synchronous: no polling needed

	jobs, err := cron.Get(pid)
	if err != nil || len(jobs) != 1 || len(jobs[0].Sessions) != 1 {
		t.Fatalf("job not linked to its run: %v %+v", err, jobs)
	}
	sess, err := i.Mgr.Find(jobs[0].Sessions[0])
	if err != nil {
		t.Fatal(err)
	}
	// the session carries the UI-equivalent default, not the config model
	if sess.Model == nil || sess.Model.ProviderID != "test" || sess.Model.ID != "m" {
		t.Fatalf("session model = %+v, want test/m", sess.Model)
	}

	// the turn ran on that model: the recorded user message proves the
	// prompt resolved to the connected provider instead of erroring on the
	// config's minimax
	msgs, err := i.Mgr.LoadMessages(sess)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range msgs {
		if u, ok := m.Info.(session.UserMessage); ok {
			found = true
			if u.Model.ProviderID != "test" || u.Model.ModelID != "m" {
				t.Fatalf("user message model = %+v, want test/m", u.Model)
			}
		}
	}
	if !found {
		t.Fatal("no user message recorded")
	}
}
