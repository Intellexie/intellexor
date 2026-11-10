package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Intellexie/intellexor/internal/store"
)

// GET /attachment/{id} serves files the agent sent via the attach tool from
// <DataDir>/attachments: correct mime type, inline vs download disposition,
// the base64 envelope for the desktop bridge, and rejection of path tricks.
func TestAttachmentRoute(t *testing.T) {
	t.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	dir := filepath.Join(store.DataDir(), "attachments")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("abcd1234_report.md", "# hello\n")
	write("deadbeef_pixel.png", "\x89PNG\r\n\x1a\nfake")
	write("cafe0000_app.bin", "\x00\x01\x02binary")

	s := New(t.TempDir(), "test")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	// text file: served inline with a text mime
	code, body := doReq(t, "GET", ts.URL+"/attachment/abcd1234_report.md", "")
	if code != 200 {
		t.Fatalf("GET md: status %d: %s", code, body)
	}
	// headers are checked on the recorder below; doReq only returns the body

	// image: base64 envelope decodes to the original bytes
	code, body = doReq(t, "GET", ts.URL+"/attachment/deadbeef_pixel.png?format=base64", "")
	if code != 200 {
		t.Fatalf("GET base64: status %d: %s", code, body)
	}
	var env struct {
		Name string `json:"name"`
		Mime string `json:"mime"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("base64 envelope: %v (%s)", err, body)
	}
	if env.Name != "pixel.png" {
		t.Errorf("envelope name = %q, want pixel.png", env.Name)
	}
	if !strings.HasPrefix(env.Mime, "image/") {
		t.Errorf("envelope mime = %q, want image/*", env.Mime)
	}
	if b, err := base64.StdEncoding.DecodeString(env.Data); err != nil || string(b) != "\x89PNG\r\n\x1a\nfake" {
		t.Errorf("envelope data mismatch: %v %q", err, b)
	}

	// unknown id
	code, _ = doReq(t, "GET", ts.URL+"/attachment/ffffffff_nope.txt", "")
	if code != 404 {
		t.Errorf("GET missing: status %d, want 404", code)
	}
	// traversal attempts (a bare ".." is cleaned away by ServeMux before the
	// handler runs, so it never serves an attachment either way)
	for _, bad := range []string{"..%2F..%2Fauth.json", "%2e%2e", "a..b..c"} {
		code, _ = doReq(t, "GET", ts.URL+"/attachment/"+bad, "")
		if code == 200 {
			t.Errorf("GET %q: status 200, want rejection", bad)
		}
	}
}

// The disposition and content-type headers are set on the raw response.
func TestAttachmentHeaders(t *testing.T) {
	t.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	dir := filepath.Join(store.DataDir(), "attachments")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "abcd1234_notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "abcd1234_thing.bin"), []byte{0, 1, 2}, 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(t.TempDir(), "test")
	h := s.Handler()

	get := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", target, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	w := get("/attachment/abcd1234_notes.txt")
	if w.Code != 200 {
		t.Fatalf("txt: status %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("txt content-type = %q, want text/plain*", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "inline") || !strings.Contains(cd, "notes.txt") {
		t.Errorf("txt disposition = %q, want inline notes.txt", cd)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("txt missing nosniff")
	}

	// binary: forced download disposition
	w = get("/attachment/abcd1234_thing.bin")
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("bin disposition = %q, want attachment", cd)
	}
	// explicit download=1 forces attachment even for text
	w = get("/attachment/abcd1234_notes.txt?download=1")
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("download=1 disposition = %q, want attachment", cd)
	}
}
