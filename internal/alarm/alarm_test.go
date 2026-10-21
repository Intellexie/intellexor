package alarm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

// Alarms written before the seconds refactor stored TriggerAt and Created
// in milliseconds. On load they are migrated to seconds — otherwise a
// legacy pending alarm would look infinitely far in the future and
// silently never fire.
func TestLoadMigratesLegacyMilliseconds(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	pid := "legacy"
	path := filepath.Join(store.DataDir(), "storage", "alarm", pid+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := []Alarm{
		{ID: "alm_old", SessionID: "ses_a", TriggerAt: util.Now() + 60_000, Created: util.Now(), Note: "ms era"},
		{ID: "alm_fired", SessionID: "ses_a", TriggerAt: util.Now() - 1000, Created: util.Now(), Fired: true},
	}
	b, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	alarms, err := List(pid, "ses_a", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 1 {
		t.Fatalf("expected 1 pending alarm, got %d", len(alarms))
	}
	a := alarms[0]
	if a.TriggerAt > msThreshold {
		t.Fatalf("TriggerAt not migrated to seconds: %d", a.TriggerAt)
	}
	if a.Created > msThreshold {
		t.Fatalf("Created not migrated to seconds: %d", a.Created)
	}

	// the migrated alarm is claimable by a seconds-based now: the legacy
	// value would have been stuck in the far future forever
	due, err := ClaimDue(pid, a.TriggerAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != "alm_old" {
		t.Fatalf("migrated alarm not claimable: %+v", due)
	}
}

// Claim is the "trigger now" primitive: it marks one unfired alarm fired
// regardless of its trigger time and returns it; a second claim (or a
// racing sweep) finds nothing left to claim.
func TestClaimMarksFiredExactlyOnce(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	pid := "claim"
	future := util.Now()/1000 + 3600
	if _, err := Add(pid, "ses_a", future, "note"); err != nil {
		t.Fatal(err)
	}

	a, err := Claim(pid, "alm_nope")
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for unknown id, got %v", err)
	}

	a, err = Claim(pid, listOne(pid, "ses_a").ID)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Fired {
		t.Fatalf("claimed alarm not marked fired: %+v", a)
	}
	// a claim ignores TriggerAt (user asked for now), but the sweep must
	// not fire it again either
	if due, err := ClaimDue(pid, util.Now()/1000); err != nil || len(due) != 0 {
		t.Fatalf("claimed alarm re-claimed by the sweep: %+v (%v)", due, err)
	}
	if _, err := Claim(pid, a.ID); err != ErrNotFound {
		t.Fatalf("second claim must be ErrNotFound, got %v", err)
	}
}

// Delete removes any alarm — pending or fired, any session.
func TestDeleteRemovesAnyAlarm(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	pid := "del"
	if _, err := Add(pid, "ses_a", util.Now()/1000+60, ""); err != nil {
		t.Fatal(err)
	}
	id := listOne(pid, "ses_a").ID
	if err := Delete(pid, id); err != nil {
		t.Fatal(err)
	}
	if alarms, _ := List(pid, "ses_a", false); len(alarms) != 0 {
		t.Fatalf("alarm not deleted: %+v", alarms)
	}
	if err := Delete(pid, id); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound deleting again, got %v", err)
	}
}

// Import re-arms exported alarms under the new session id: future ones
// stay armed with their original trigger time, past-due untriggered ones
// are marked fired so the sweep can never replay them, and ids are
// re-minted so an import into the source project cannot collide.
func TestImportNeutralizesPastDueAlarms(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	pid := "import"
	now := util.Now() / 1000
	exported := []Alarm{
		{ID: "alm_future", SessionID: "ses_old", TriggerAt: now + 3600, Note: "still armed", Created: now},
		{ID: "alm_past", SessionID: "ses_old", TriggerAt: now - 60, Note: "stale", Created: now},
		{ID: "alm_fired", SessionID: "ses_old", TriggerAt: now + 60, Fired: true, Created: now},
	}
	if err := Import(pid, "ses_new", exported, now); err != nil {
		t.Fatal(err)
	}

	alarms, err := List(pid, "ses_new", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 3 {
		t.Fatalf("expected 3 imported alarms, got %d", len(alarms))
	}
	byNote := map[string]Alarm{}
	for _, a := range alarms {
		if a.SessionID != "ses_new" {
			t.Fatalf("import did not re-point the session: %+v", a)
		}
		if a.ID == "alm_future" || a.ID == "alm_past" || a.ID == "alm_fired" {
			t.Fatalf("import must re-mint ids, got %q", a.ID)
		}
		byNote[a.Note] = a
	}
	if a := byNote["still armed"]; a.Fired || a.TriggerAt != now+3600 {
		t.Fatalf("future alarm must stay armed: %+v", a)
	}
	if a := byNote["stale"]; !a.Fired {
		t.Fatalf("past-due alarm must be neutralized: %+v", a)
	}
	if a := byNote[""]; !a.Fired {
		t.Fatalf("already-fired alarm must stay fired: %+v", a)
	}

	// the sweep must not fire anything right after the import
	if due, err := ClaimDue(pid, now); err != nil || len(due) != 0 {
		t.Fatalf("sweep fired imported alarms: %+v (%v)", due, err)
	}
}

// listOne fetches the single alarm of a session (test helper).
func listOne(projectID, sessionID string) Alarm {
	alarms, err := List(projectID, sessionID, false)
	if err != nil || len(alarms) != 1 {
		panic("listOne: expected exactly one alarm")
	}
	return alarms[0]
}
