//go:build desktop

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Intellexie/intellexor/internal/server"
	"github.com/Intellexie/intellexor/internal/store"
)

// The bridge must route requests through the same handler the HTTP server
// uses — same status codes, same error shapes.
func TestApiCallRoutesThroughHandler(t *testing.T) {
	t.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()
	api := &Api{srv: server.New(t.TempDir(), "test-bridge")}

	// health endpoint
	r, err := api.Call("GET", "/global/health", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != 200 {
		t.Fatalf("health status = %d, want 200", r.Status)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatalf("health body not JSON: %v (%q)", err, r.Body)
	}
	if body["version"] != "test-bridge" {
		t.Fatalf("health version = %v", body["version"])
	}

	// unknown route → opencode-style error shape (non-GET misses the web UI
	// fallback in serveWeb, which serves index.html for any GET)
	r, err = api.Call("POST", "/definitely/not/a/route", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != 404 {
		t.Fatalf("missing route status = %d, want 404", r.Status)
	}

	// POST with a JSON body must reach the handler (bad input → 400, not a
	// bridge error — proves the body was forwarded)
	r, err = api.Call("POST", "/session", "{not json", "Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status < 400 {
		t.Fatalf("bad JSON body status = %d, want an error status", r.Status)
	}
	if !strings.Contains(r.Body, "name") {
		t.Fatalf("error body missing opencode error shape: %q", r.Body)
	}
}
