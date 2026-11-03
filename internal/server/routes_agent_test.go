package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Intellexie/intellexor/internal/store"
)

// GET /agent must list built-in agents (native) plus agents defined in the
// project's config files (native=false), so the web UI's agent picker can
// offer config-defined agents.
func TestListAgentsIncludesConfigAgents(t *testing.T) {
	t.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	dir := t.TempDir()
	cfg := `{"agent": {"reviewer": {"description": "Reviews code", "mode": "primary", "color": "#123456"}}}`
	if err := os.WriteFile(filepath.Join(dir, "intellexor.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(dir, "test")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	code, body := doReq(t, "GET", ts.URL+"/agent", "")
	if code != 200 {
		t.Fatalf("GET /agent: status %d: %s", code, body)
	}
	var agents []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Mode        string `json:"mode"`
		Native      bool   `json:"native"`
		Color       string `json:"color"`
	}
	if err := json.Unmarshal(body, &agents); err != nil {
		t.Fatal(err)
	}
	byName := map[string]map[string]any{}
	for _, a := range agents {
		byName[a.Name] = map[string]any{"native": a.Native, "mode": a.Mode, "description": a.Description, "color": a.Color}
	}
	if _, ok := byName["reviewer"]; !ok {
		t.Fatalf("config agent missing from GET /agent: %s", body)
	}
	if byName["reviewer"]["native"] != false {
		t.Fatalf("config agent must not be native: %v", byName["reviewer"])
	}
	if byName["reviewer"]["description"] != "Reviews code" || byName["reviewer"]["mode"] != "primary" || byName["reviewer"]["color"] != "#123456" {
		t.Fatalf("config agent fields not converted: %v", byName["reviewer"])
	}
	if byName["plan"]["native"] != true {
		t.Fatalf("built-in agent must be native: %v", byName["plan"])
	}
}
