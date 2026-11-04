package app

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Intellexie/intellexor/internal/store"
)

// Agents are listed in config `order`: built-ins first (0,10,20,…), config
// agents after them unless they set an explicit order, and a config override
// can reposition a built-in. The order drives GET /agent, the composer
// dropdown and Tab cycling.
func TestAgentOrdering(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	work := t.TempDir()
	cfg := `{
		"agent": {
			"reviewer": {"description": "Reviews code", "mode": "primary"},
			"aaa": {"description": "First of all", "mode": "primary", "order": -5},
			"plan": {"order": 15}
		}
	}`
	if err := os.WriteFile(filepath.Join(work, "intellexor.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	i := New(work)
	var names []string
	for _, a := range i.Agents() {
		names = append(names, a.Name)
	}
	// aaa (order -5) before the built-ins; plan moved between orchestrator
	// (20) and general (30); the built-in review agent sits at 15, between plan
	// and orchestrator; reviewer and the untouched built-ins keep their
	// relative positions (reviewer defaults to 1000, after explore)
	want := []string{"aaa", "build", "plan", "review", "orchestrator", "general", "explore", "reviewer"}
	if !slices.Equal(names, want) {
		t.Fatalf("agents = %v, want %v", names, want)
	}

	// the overridden built-in keeps its other fields
	plan, err := i.Agent("plan")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Order != 15 || plan.Hidden {
		t.Fatalf("plan override not applied: %+v", plan)
	}
	for _, r := range plan.Permission {
		if r.Permission == "edit" && r.Action == "deny" {
			return // built-in permission rules survived the order-only override
		}
	}
	t.Fatalf("order-only override lost the built-in permission rules: %+v", plan)
}
