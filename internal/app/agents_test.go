package app

import (
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Intellexie/intellexor/internal/store"
)

// The orchestrator is a primary agent restricted to coordination tools: the
// model must never be offered a build tool (bash/edit/write/...), and it is
// the only agent with the cron tool.
func TestOrchestratorToolRestriction(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	i := New(t.TempDir())
	orch := i.Agent("orchestrator")
	if orch.Name != "orchestrator" || orch.Mode != "primary" {
		t.Fatalf("unexpected agent: %+v", orch)
	}

	got := activeToolNames(i.Directory, orch)
	want := []string{"invalid", "board", "webfetch", "todowrite", "task", "cron", "schedule_wakeup"}
	if !slices.Equal(got, want) {
		t.Fatalf("orchestrator tools = %v, want %v", got, want)
	}

	// agents without a Tools list keep the full default set — including the
	// build tools, but not the orchestrator-only cron tool
	build := activeToolNames(i.Directory, i.Agent("build"))
	for _, n := range []string{"bash", "read", "edit", "write", "task", "webfetch", "todowrite", "question", "board"} {
		if !slices.Contains(build, n) {
			t.Fatalf("default tool set missing %q: %v", n, build)
		}
	}
	if slices.Contains(build, "cron") {
		t.Fatalf("cron must not be in the default tool set: %v", build)
	}
	if slices.Contains(build, "apply_patch") {
		t.Fatalf("apply_patch is gpt-model-only: %v", build)
	}
}

// The gpt-model apply_patch swap must respect the agent's tool restriction.
func TestToolNamesForModelRespectsRestriction(t *testing.T) {
	restricted := []string{"invalid", "board", "webfetch", "todowrite", "task", "cron"}
	got := toolNamesForModel("gpt-5", restricted, false)
	if slices.Contains(got, "apply_patch") {
		t.Fatalf("restricted agent got apply_patch on a gpt model: %v", got)
	}

	got = toolNamesForModel("gpt-5", []string{"invalid", "bash", "edit", "write"}, true)
	if !slices.Contains(got, "apply_patch") {
		t.Fatalf("gpt model lost apply_patch: %v", got)
	}
	if slices.Contains(got, "edit") || slices.Contains(got, "write") {
		t.Fatalf("gpt model kept edit/write: %v", got)
	}

	// non-gpt models pass through untouched
	got = toolNamesForModel("claude-sonnet-4", restricted, false)
	if !slices.Equal(got, restricted) {
		t.Fatalf("non-gpt model changed the tool list: %v", got)
	}
}

// A session started from a card gets coordination guidance when it runs as
// the orchestrator, and implementation guidance otherwise.
func TestBoardContextByAgent(t *testing.T) {
	i, s := setupAbortTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	card := linkCard(t, i, s, "Coordinate the thing", "inprogress")

	bc := boardContext(i, s, "orchestrator")
	if !strings.Contains(bc, card.ID) {
		t.Fatalf("board context missing card id:\n%s", bc)
	}
	if !strings.Contains(bc, "coordination task") {
		t.Fatalf("orchestrator board context missing coordination guidance:\n%s", bc)
	}
	bc = boardContext(i, s, "build")
	if !strings.Contains(bc, "implement it end to end") {
		t.Fatalf("build board context missing implementation guidance:\n%s", bc)
	}
}
