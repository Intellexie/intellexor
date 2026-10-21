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

func setupWorkspaceTest(t *testing.T) *httptest.Server {
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

func TestCreateAndListWorkspaces(t *testing.T) {
	ts := setupWorkspaceTest(t)

	wsDir := t.TempDir()
	code, body := doReq(t, "POST", ts.URL+"/workspace", `{"path":"`+wsDir+`"}`)
	if code != 200 {
		t.Fatalf("POST /workspace: status %d: %s", code, body)
	}
	var ws map[string]any
	if err := json.Unmarshal(body, &ws); err != nil {
		t.Fatal(err)
	}
	if ws["directory"] != wsDir {
		t.Fatalf("unexpected directory %v", ws["directory"])
	}

	// the workspace gets its own config file with fallbacks intact
	if _, err := os.Stat(filepath.Join(wsDir, "intellexor.json")); err != nil {
		t.Fatalf("workspace config not seeded: %v", err)
	}

	// creating twice is idempotent (no duplicate registration)
	if code, _ := doReq(t, "POST", ts.URL+"/workspace", `{"path":"`+wsDir+`"}`); code != 200 {
		t.Fatalf("duplicate create: status %d", code)
	}

	code, body = doReq(t, "GET", ts.URL+"/workspace", "")
	if code != 200 {
		t.Fatalf("GET /workspace: status %d", code)
	}
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("GET /workspace: %s", body)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 workspaces (default + new), got %d: %s", len(list), body)
	}
	dirs := map[string]bool{}
	for _, w := range list {
		dirs[w["directory"].(string)] = true
	}
	if !dirs[wsDir] {
		t.Fatalf("new workspace missing from list: %s", body)
	}
}

func TestCreateWorkspaceValidation(t *testing.T) {
	ts := setupWorkspaceTest(t)

	// missing path
	if code, _ := doReq(t, "POST", ts.URL+"/workspace", `{}`); code != 400 {
		t.Fatalf("expected 400 for empty path, got %d", code)
	}
	// non-existent directory
	if code, _ := doReq(t, "POST", ts.URL+"/workspace", `{"path":"/nonexistent/dir/xyz"}`); code != 404 {
		t.Fatalf("expected 404 for missing dir, got %d", code)
	}
	// a file, not a directory
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _ := doReq(t, "POST", ts.URL+"/workspace", `{"path":"`+file+`"}`); code != 400 {
		t.Fatalf("expected 400 for file path, got %d", code)
	}
}

func TestFileHandlerDirsOnly(t *testing.T) {
	ts := setupWorkspaceTest(t)
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
