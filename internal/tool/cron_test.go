package tool

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Intellexie/intellexor/internal/cron"
	"github.com/Intellexie/intellexor/internal/store"
)

// The full lifecycle through the tool: add (with a title derived from the
// prompt), list, pause, resume, update and delete — each mutation broadcast
// so connected web clients refresh.
func TestCronToolLifecycle(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	updates := 0
	ct := CronTool{OnUpdate: func() { updates++ }}
	ctx := &Ctx{Directory: t.TempDir()}

	res, err := ct.Run(ctx, json.RawMessage(`{"action":"add","prompt":"Run the nightly checks","spec":"@daily"}`))
	if err != nil {
		t.Fatalf("add failed: %v", err)
	}
	if updates != 1 {
		t.Fatalf("add did not broadcast (updates=%d)", updates)
	}
	job, ok := res.Metadata["job"].(cron.Job)
	if !ok || job.ID == "" {
		t.Fatalf("add result carries no job: %+v", res.Metadata)
	}
	if job.Title != "Run the nightly checks" {
		t.Fatalf("title not derived from the prompt: %q", job.Title)
	}
	if !strings.Contains(res.Output, "@daily") || !strings.Contains(res.Output, "next run") {
		t.Fatalf("unexpected add output: %s", res.Output)
	}
	id := job.ID

	res, err = ct.Run(ctx, json.RawMessage(`{"action":"list"}`))
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if !strings.Contains(res.Output, id) || !strings.Contains(res.Output, "Run the nightly checks") {
		t.Fatalf("list output missing the job:\n%s", res.Output)
	}

	if _, err := ct.Run(ctx, json.RawMessage(`{"action":"pause","id":"`+id+`"}`)); err != nil {
		t.Fatalf("pause failed: %v", err)
	}
	jobs, _ := cron.Get(store.ProjectID(ctx.Directory))
	if len(jobs) != 1 || !jobs[0].Paused {
		t.Fatalf("job not paused: %+v", jobs)
	}

	res, err = ct.Run(ctx, json.RawMessage(`{"action":"resume","id":"`+id+`"}`))
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if !strings.Contains(res.Output, "next run") {
		t.Fatalf("resume output missing next run: %s", res.Output)
	}

	if _, err := ct.Run(ctx, json.RawMessage(`{"action":"update","id":"`+id+`","spec":"@hourly"}`)); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	jobs, _ = cron.Get(store.ProjectID(ctx.Directory))
	if len(jobs) != 1 || jobs[0].Spec != "@hourly" {
		t.Fatalf("spec not updated: %+v", jobs)
	}

	if _, err := ct.Run(ctx, json.RawMessage(`{"action":"delete","id":"`+id+`"}`)); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	jobs, _ = cron.Get(store.ProjectID(ctx.Directory))
	if len(jobs) != 0 {
		t.Fatalf("job not deleted: %+v", jobs)
	}
	if updates != 5 {
		t.Fatalf("mutations did not all broadcast (updates=%d)", updates)
	}
}

// Preview validates a spec, and bad input produces actionable errors.
func TestCronToolPreviewAndErrors(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	ct := CronTool{}
	ctx := &Ctx{Directory: t.TempDir()}

	res, err := ct.Run(ctx, json.RawMessage(`{"action":"preview","spec":"@every 30m"}`))
	if err != nil {
		t.Fatalf("preview failed: %v", err)
	}
	if !strings.Contains(res.Output, "next fire time") {
		t.Fatalf("unexpected preview output: %s", res.Output)
	}

	if _, err := ct.Run(ctx, json.RawMessage(`{"action":"add","prompt":"x","spec":"nope"}`)); err == nil || !strings.Contains(err.Error(), "invalid cron expression") {
		t.Fatalf("expected invalid-spec error, got %v", err)
	}
	if _, err := ct.Run(ctx, json.RawMessage(`{"action":"add","spec":"@daily"}`)); err == nil {
		t.Fatal("expected an error adding without a prompt")
	}
	if _, err := ct.Run(ctx, json.RawMessage(`{"action":"add","prompt":"x"}`)); err == nil {
		t.Fatal("expected an error adding without a spec")
	}
	if _, err := ct.Run(ctx, json.RawMessage(`{"action":"update"}`)); err == nil {
		t.Fatal("expected an error updating without an id")
	}
	if _, err := ct.Run(ctx, json.RawMessage(`{"action":"delete","id":"cron_nope"}`)); err == nil || !strings.Contains(err.Error(), "cron_nope") {
		t.Fatalf("expected not-found error mentioning the id, got %v", err)
	}
	if _, err := ct.Run(ctx, json.RawMessage(`{"action":"frobnicate"}`)); err == nil || !strings.Contains(err.Error(), "frobnicate") {
		t.Fatalf("expected unknown-action error, got %v", err)
	}
}
