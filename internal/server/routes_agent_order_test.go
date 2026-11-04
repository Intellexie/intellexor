package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Intellexie/intellexor/internal/store"
)

// POST /config/agent-order persists the agent sequence: agents with an
// existing config entry keep their fields (order merged in, written to the
// defining file), pure built-ins get a minimal {order} entry in the requested
// file, and GET /agent reflects the new order.
func TestConfigAgentOrder(t *testing.T) {
	t.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	dir := t.TempDir()
	cfg := `{"agent": {"reviewer": {"description": "Reviews code", "mode": "primary", "prompt": "review!"}}}`
	if err := os.WriteFile(filepath.Join(dir, "intellexor.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(dir, "test")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	cfgPath := filepath.Join(dir, "intellexor.json")
	order := []string{"reviewer", "build", "plan", "review", "orchestrator", "general", "explore"}
	b, _ := json.Marshal(map[string]any{"file": cfgPath, "agents": order})
	code, body := doReq(t, "POST", ts.URL+"/config/agent-order", string(b))
	if code != 200 {
		t.Fatalf("POST /config/agent-order: status %d: %s", code, body)
	}

	// GET /agent reflects the new sequence
	code, body = doReq(t, "GET", ts.URL+"/agent", "")
	if code != 200 {
		t.Fatalf("GET /agent: status %d: %s", code, body)
	}
	var agents []struct {
		Name  string `json:"name"`
		Order int    `json:"order"`
	}
	if err := json.Unmarshal(body, &agents); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range agents {
		names = append(names, a.Name)
	}
	if len(names) != len(order) || names[0] != "reviewer" || names[1] != "build" {
		t.Fatalf("GET /agent order = %v, want %v", names, order)
	}

	// the config-defined agent kept its fields, order merged in
	var doc map[string]any
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	agentSec := doc["agent"].(map[string]any)
	rev := agentSec["reviewer"].(map[string]any)
	if rev["prompt"] != "review!" || rev["order"] != float64(0) {
		t.Fatalf("reviewer entry not preserved: %v", rev)
	}

	// the pure built-in got a minimal order entry in the requested file
	build := agentSec["build"].(map[string]any)
	if build["order"] != float64(10) || len(build) != 1 {
		t.Fatalf("build entry = %v, want only {order: 10}", build)
	}

	// unknown agent names are rejected
	b, _ = json.Marshal(map[string]any{"file": cfgPath, "agents": []string{"build", "nope"}})
	code, _ = doReq(t, "POST", ts.URL+"/config/agent-order", string(b))
	if code != 400 {
		t.Fatalf("unknown agent: status %d, want 400", code)
	}
}
