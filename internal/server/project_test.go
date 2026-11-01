package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Intellexie/intellexor/internal/store"
)

func setupProjectTest(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()
	s := New(t.TempDir(), "test")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func doReq(t *testing.T, method, url, body string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, b
}

func TestCreateAndListProjects(t *testing.T) {
	ts := setupProjectTest(t)

	wsDir := t.TempDir()
	code, body := doReq(t, "POST", ts.URL+"/project", `{"path":"`+wsDir+`"}`)
	if code != 200 {
		t.Fatalf("POST /project: status %d: %s", code, body)
	}
	var ws map[string]any
	if err := json.Unmarshal(body, &ws); err != nil {
		t.Fatal(err)
	}
	if ws["directory"] != wsDir {
		t.Fatalf("unexpected directory %v", ws["directory"])
	}
	// opencode Project.Info shape: deterministic id, canonical, time, sandboxes
	if ws["id"] != projectID(wsDir) {
		t.Fatalf("expected sha1 directory id, got %v", ws["id"])
	}
	if ws["canonical"] != wsDir {
		t.Fatalf("unexpected canonical %v", ws["canonical"])
	}
	if _, ok := ws["time"].(map[string]any); !ok {
		t.Fatalf("missing time object: %s", body)
	}
	if sand, ok := ws["sandboxes"].([]any); !ok || len(sand) != 0 {
		t.Fatalf("expected empty sandboxes array: %s", body)
	}

	// the project gets its own config file with fallbacks intact
	if _, err := os.Stat(filepath.Join(wsDir, "intellexor.json")); err != nil {
		t.Fatalf("project config not seeded: %v", err)
	}

	// creating twice is idempotent (no duplicate registration)
	if code, _ := doReq(t, "POST", ts.URL+"/project", `{"path":"`+wsDir+`"}`); code != 200 {
		t.Fatalf("duplicate create: status %d", code)
	}

	code, body = doReq(t, "GET", ts.URL+"/project", "")
	if code != 200 {
		t.Fatalf("GET /project: status %d", code)
	}
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("GET /project: %s", body)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 projects (default + new), got %d: %s", len(list), body)
	}
	dirs := map[string]bool{}
	for _, p := range list {
		dirs[p["directory"].(string)] = true
	}
	if !dirs[wsDir] {
		t.Fatalf("new project missing from list: %s", body)
	}
}

func TestCreateProjectValidation(t *testing.T) {
	ts := setupProjectTest(t)

	// missing path
	if code, _ := doReq(t, "POST", ts.URL+"/project", `{}`); code != 400 {
		t.Fatalf("expected 400 for empty path, got %d", code)
	}
	// non-existent directory
	if code, _ := doReq(t, "POST", ts.URL+"/project", `{"path":"/nonexistent/dir/xyz"}`); code != 404 {
		t.Fatalf("expected 404 for missing dir, got %d", code)
	}
	// a file, not a directory
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _ := doReq(t, "POST", ts.URL+"/project", `{"path":"`+file+`"}`); code != 400 {
		t.Fatalf("expected 400 for file path, got %d", code)
	}
}

func TestProjectCurrent(t *testing.T) {
	ts := setupProjectTest(t)

	code, body := doReq(t, "GET", ts.URL+"/project/current", "")
	if code != 200 {
		t.Fatalf("GET /project/current: status %d: %s", code, body)
	}
	var cur map[string]any
	if err := json.Unmarshal(body, &cur); err != nil {
		t.Fatal(err)
	}
	if cur["id"] != projectID(cur["directory"].(string)) {
		t.Fatalf("id does not match directory: %s", body)
	}
	if cur["directory"] != cur["canonical"] {
		t.Fatalf("expected directory == canonical: %s", body)
	}

	// ?directory= overrides the resolved project
	code, body = doReq(t, "GET", ts.URL+"/project/current?directory=/tmp", "")
	if code != 200 {
		t.Fatalf("GET /project/current?directory: status %d", code)
	}
	if err := json.Unmarshal(body, &cur); err != nil {
		t.Fatal(err)
	}
	if cur["directory"] != "/tmp" {
		t.Fatalf("expected /tmp, got %v", cur["directory"])
	}
}

func TestRenameProject(t *testing.T) {
	ts := setupProjectTest(t)

	wsDir := t.TempDir()
	code, body := doReq(t, "POST", ts.URL+"/project", `{"path":"`+wsDir+`"}`)
	if code != 200 {
		t.Fatalf("POST /project: status %d: %s", code, body)
	}
	var created map[string]any
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	id := created["id"].(string)

	// rename via opencode's project.update shape
	code, body = doReq(t, "PATCH", ts.URL+"/project/"+id, `{"name":"Renamed"}`)
	if code != 200 {
		t.Fatalf("PATCH /project/{id}: status %d: %s", code, body)
	}
	var updated map[string]any
	if err := json.Unmarshal(body, &updated); err != nil {
		t.Fatal(err)
	}
	if updated["name"] != "Renamed" {
		t.Fatalf("expected renamed project, got %s", body)
	}

	// the name shows up in the list
	code, body = doReq(t, "GET", ts.URL+"/project", "")
	if code != 200 {
		t.Fatalf("GET /project: status %d", code)
	}
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range list {
		if p["directory"] == wsDir && p["name"] == "Renamed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("rename not reflected in list: %s", body)
	}

	// empty name resets to the base name
	code, body = doReq(t, "PATCH", ts.URL+"/project/"+id, `{"name":""}`)
	if code != 200 {
		t.Fatalf("PATCH empty name: status %d: %s", code, body)
	}
	if err := json.Unmarshal(body, &updated); err != nil {
		t.Fatal(err)
	}
	if updated["name"] != filepath.Base(wsDir) {
		t.Fatalf("expected basename reset, got %s", body)
	}

	// unknown project id → opencode's ProjectNotFoundError
	code, body = doReq(t, "PATCH", ts.URL+"/project/deadbeef", `{"name":"x"}`)
	if code != 404 {
		t.Fatalf("expected 404 for unknown project, got %d: %s", code, body)
	}
	if !strings.Contains(string(body), "ProjectNotFoundError") {
		t.Fatalf("expected ProjectNotFoundError name, got %s", body)
	}
}

func TestFileHandlerDirsOnly(t *testing.T) {
	ts := setupProjectTest(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, body := doReq(t, "GET", ts.URL+"/file?dirs=1&path="+root, "")
	if code != 200 {
		t.Fatalf("GET /file: status %d", code)
	}
	var ents []map[string]any
	if err := json.Unmarshal(body, &ents); err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0]["name"] != "sub" {
		t.Fatalf("expected only the directory, got %s", body)
	}
}
