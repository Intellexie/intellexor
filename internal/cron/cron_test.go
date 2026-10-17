package cron

import (
	"os"
	"testing"
	"time"

	"github.com/Intellexie/intellexor/internal/store"
)

func testDataDir(t *testing.T) {
	t.Helper()
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()
}

// at builds a local time on 2025-01-15 (a Wednesday) at h:m.
func at(h, m int) time.Time {
	return time.Date(2025, 1, 15, h, m, 0, 0, time.Local)
}

func day(y int, mo time.Month, d, h, m int) time.Time {
	return time.Date(y, mo, d, h, m, 0, 0, time.Local)
}

func TestNext(t *testing.T) {
	cases := []struct {
		name  string
		spec  string
		after time.Time
		want  time.Time
	}{
		{"later minute same hour", "30 * * * *", at(9, 0), at(9, 30)},
		{"strictly after", "30 * * * *", at(9, 30), at(10, 30)},
		{"next day", "0 9 * * *", at(9, 0), day(2025, 1, 16, 9, 0)},
		{"step minutes", "*/15 * * * *", at(9, 1), at(9, 15)},
		{"range with step", "10-20/5 * * * *", at(9, 16), at(9, 20)},
		{"list", "5,7 9 * * *", at(9, 6), at(9, 7)},
		{"hourly macro", "@hourly", at(9, 30), day(2025, 1, 15, 10, 0)},
		{"daily macro", "@daily", at(9, 30), day(2025, 1, 16, 0, 0)},
		{"every duration", "@every 30m", at(9, 0), at(9, 30)},
		{"weekday names", "0 9 * * mon-fri", at(16, 0), day(2025, 1, 16, 9, 0)},
		{"month names", "0 9 * feb *", at(16, 0), day(2025, 2, 1, 9, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Next(c.spec, c.after)
			if err != nil {
				t.Fatalf("Next(%q): %v", c.spec, err)
			}
			if !got.Equal(c.want) {
				t.Fatalf("Next(%q, %v) = %v, want %v", c.spec, c.after, got, c.want)
			}
		})
	}
}

func TestNextDomDowRule(t *testing.T) {
	// vixie-cron: when both day-of-month and weekday are restricted, the day
	// matches if EITHER matches. "0 0 13 * 5" fires on the 13th OR on Fridays;
	// after Wed 2025-01-15 the next is Friday the 17th (the 13th passed).
	got, err := Next("0 0 13 * 5", at(12, 0))
	if err != nil {
		t.Fatal(err)
	}
	if want := day(2025, 1, 17, 0, 0); !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// "0 0 13 * 1" fires on the 13th OR on Mondays: the 13th passed this
	// month, so the next match is Monday the 20th.
	got, err = Next("0 0 13 * 1", at(12, 0))
	if err != nil {
		t.Fatal(err)
	}
	if want := day(2025, 1, 20, 0, 0); !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNextSundayAsSeven(t *testing.T) {
	// 2025-01-19 is a Sunday; both "0 0 * * 0" and "0 0 * * 7" must reach it.
	for _, spec := range []string{"0 0 * * 0", "0 0 * * 7"} {
		got, err := Next(spec, at(12, 0))
		if err != nil {
			t.Fatalf("%s: %v", spec, err)
		}
		if want := day(2025, 1, 19, 0, 0); !got.Equal(want) {
			t.Fatalf("%s: got %v, want %v", spec, got, want)
		}
	}
}

func TestNextErrors(t *testing.T) {
	for _, spec := range []string{
		"", "0 9 * *", "* * * * * *", // wrong field count
		"60 * * * *", "0 9 * * foo", // bad values
		"5-1 * * * *",                                 // inverted range
		"@never", "@every 0s", "@every -5m", "@every", // bad macros
	} {
		if _, err := Next(spec, at(9, 0)); err == nil {
			t.Fatalf("Next(%q) should have failed", spec)
		}
	}
}

func TestStorageRoundtrip(t *testing.T) {
	testDataDir(t)
	const pid = "proj1"

	job, err := Add(pid, "Nightly check", "run the checks", "0 9 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if job.Title != "Nightly check" || job.Prompt != "run the checks" || job.Spec != "0 9 * * *" {
		t.Fatalf("bad job: %+v", job)
	}
	if job.NextRun <= utilNow()-60000 {
		t.Fatalf("NextRun should be in the future: %d", job.NextRun)
	}
	if _, err := Add(pid, "Bad", "p", "not a cron"); err == nil {
		t.Fatal("Add with a bad spec should fail")
	}

	jobs, err := Get(pid)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Get: %v, %d jobs", err, len(jobs))
	}

	// update title/prompt/spec
	newSpec := "*/5 * * * *"
	updated, err := Update(pid, job.ID, strPtr("Renamed"), strPtr("new prompt"), &newSpec)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != "Renamed" || updated.Prompt != "new prompt" || updated.Spec != "*/5 * * * *" {
		t.Fatalf("bad update: %+v", updated)
	}
	if _, err := Update(pid, job.ID, nil, nil, strPtr("bogus")); err == nil {
		t.Fatal("Update with a bad spec should fail")
	}

	// pause / resume
	paused, err := SetPaused(pid, job.ID, true)
	if err != nil || !paused.Paused {
		t.Fatalf("SetPaused(true): %v %+v", err, paused)
	}
	resumed, err := SetPaused(pid, job.ID, false)
	if err != nil || resumed.Paused {
		t.Fatalf("SetPaused(false): %v %+v", err, resumed)
	}
	if resumed.NextRun <= utilNow()-60000 {
		t.Fatalf("resume should re-arm NextRun in the future: %d", resumed.NextRun)
	}

	// delete
	if err := Delete(pid, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := Delete(pid, job.ID); err != ErrNotFound {
		t.Fatalf("second delete: %v", err)
	}
	jobs, _ = Get(pid)
	if len(jobs) != 0 {
		t.Fatalf("expected 0 jobs, got %d", len(jobs))
	}
}

func TestClaimDue(t *testing.T) {
	testDataDir(t)
	const pid = "proj2"

	job, err := Add(pid, "J", "P", "@every 1h")
	if err != nil {
		t.Fatal(err)
	}
	now := utilNow()

	// not due yet
	if _, ok, err := ClaimDue(pid, job.ID, now); err != nil || ok {
		t.Fatalf("claim before due: ok=%v err=%v", ok, err)
	}
	// due (NextRun passed)
	if _, ok, err := ClaimDue(pid, job.ID, now+2*time.Hour.Milliseconds()); err != nil || !ok {
		t.Fatalf("claim when due: ok=%v err=%v", ok, err)
	}
	jobs, _ := Get(pid)
	if jobs[0].Runs != 1 || jobs[0].LastRun != now+2*time.Hour.Milliseconds() {
		t.Fatalf("run not recorded: %+v", jobs[0])
	}
	if jobs[0].NextRun <= now+2*time.Hour.Milliseconds() {
		t.Fatalf("NextRun should have advanced past the claim time: %d", jobs[0].NextRun)
	}
	// a second claim in the same instant must not double-fire
	if _, ok, err := ClaimDue(pid, job.ID, now+2*time.Hour.Milliseconds()); err != nil || ok {
		t.Fatalf("double claim: ok=%v err=%v", ok, err)
	}

	// paused jobs never claim
	if _, err := SetPaused(pid, job.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ClaimDue(pid, job.ID, now+5*time.Hour.Milliseconds()); err != nil || ok {
		t.Fatalf("claim while paused: ok=%v err=%v", ok, err)
	}

	// unknown job
	if _, ok, err := ClaimDue(pid, "nope", now); err != ErrNotFound || ok {
		t.Fatalf("claim unknown: ok=%v err=%v", ok, err)
	}
}

func TestSetSessionCapsList(t *testing.T) {
	testDataDir(t)
	const pid = "proj3"

	job, err := Add(pid, "J", "P", "0 9 * * *")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxTrackedSessions+5; i++ {
		if err := SetSession(pid, job.ID, "ses_"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	jobs, _ := Get(pid)
	if len(jobs[0].Sessions) != maxTrackedSessions {
		t.Fatalf("expected %d tracked sessions, got %d", maxTrackedSessions, len(jobs[0].Sessions))
	}
	// newest last
	if jobs[0].Sessions[len(jobs[0].Sessions)-1] != "ses_"+string(rune('a'+maxTrackedSessions+4)) {
		t.Fatalf("newest session not last: %+v", jobs[0].Sessions)
	}
	if err := SetSession(pid, "nope", "ses_x"); err != ErrNotFound {
		t.Fatalf("SetSession unknown: %v", err)
	}
}

func strPtr(s string) *string { return &s }

func utilNow() int64 { return time.Now().UnixMilli() }
