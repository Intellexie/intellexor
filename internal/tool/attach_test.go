package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Intellexie/intellexor/internal/store"
)

// The attach tool copies the source file into <DataDir>/attachments under a
// collision-safe name and reports its metadata for the UI.
func TestAttachToolCopiesFile(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	ws := t.TempDir()
	src := filepath.Join(ws, "report.md")
	if err := os.WriteFile(src, []byte("# hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := AttachTool{}.Run(&Ctx{Directory: ws}, json.RawMessage(`{"path":"report.md","description":"the report"}`))
	if err != nil {
		t.Fatal(err)
	}
	att, ok := res.Metadata["attachment"].(map[string]any)
	if !ok {
		t.Fatalf("metadata.attachment missing: %v", res.Metadata)
	}
	stored, _ := att["stored"].(string)
	if !strings.HasSuffix(stored, "_report.md") || len(stored) != len("abcd1234_report.md") {
		t.Errorf("stored name = %q, want <8hex>_report.md", stored)
	}
	b, err := os.ReadFile(filepath.Join(store.DataDir(), "attachments", stored))
	if err != nil {
		t.Fatalf("stored file: %v", err)
	}
	if string(b) != "# hello" {
		t.Errorf("stored content = %q", b)
	}
	if att["url"] != "/attachment/"+stored {
		t.Errorf("url = %v", att["url"])
	}
	if att["kind"] != "text" {
		t.Errorf("kind = %v, want text", att["kind"])
	}
	if att["description"] != "the report" {
		t.Errorf("description = %v", att["description"])
	}
}

func TestAttachToolErrors(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := AttachTool{}

	if _, err := tool.Run(&Ctx{Directory: ws}, json.RawMessage(`{"path":"missing.txt"}`)); err == nil {
		t.Error("missing file: want error")
	}
	if _, err := tool.Run(&Ctx{Directory: ws}, json.RawMessage(`{"path":"sub"}`)); err == nil {
		t.Error("directory: want error")
	}
	if _, err := tool.Run(&Ctx{Directory: ws}, json.RawMessage(`{}`)); err == nil {
		t.Error("no path: want error")
	}
}

// Two attaches of same-named files must not collide: the second gets a
// different random prefix.
func TestAttachToolNoCollision(t *testing.T) {
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	ws := t.TempDir()
	src := filepath.Join(ws, "same.txt")
	if err := os.WriteFile(src, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	r1, err := AttachTool{}.Run(&Ctx{Directory: ws}, json.RawMessage(`{"path":"same.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	r2, err := AttachTool{}.Run(&Ctx{Directory: ws}, json.RawMessage(`{"path":"same.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	s1 := r1.Metadata["attachment"].(map[string]any)["stored"]
	s2 := r2.Metadata["attachment"].(map[string]any)["stored"]
	if s1 == s2 {
		t.Fatalf("both attaches stored as %v", s1)
	}
	b, _ := os.ReadFile(filepath.Join(store.DataDir(), "attachments", s2.(string)))
	if string(b) != "two" {
		t.Errorf("second copy content = %q", b)
	}
}
