package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
)

// titleSSE answers title-generation requests (system prompt contains
// "title generator") with the given title after a delay; other requests are
// routed to fallback (the main-model handler).
func titleSSE(title string, delay time.Duration, fallback http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("title generator")) {
			fl, _ := w.(http.Flusher)
			w.Header().Set("Content-Type", "text/event-stream")
			time.Sleep(delay)
			chunk := map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": title}}}}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			final := map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}}
			b, _ = json.Marshal(final)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		fallback(w, r)
	}
}

func setupTitleTest(t *testing.T, cfg string) (*Instance, *session.Session) {
	t.Helper()
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "intellexor.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	i := New(work)
	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "New session - x"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	return i, s
}

// The small model answers while the main step is still streaming; the title
// must survive the prompt loop's per-step session saves.
func TestTitleGeneratedAndSticks(t *testing.T) {
	srv := httptest.NewServer(titleSSE("Fix Login Bug", 50*time.Millisecond, slowSSE(50, 20*time.Millisecond)))
	defer srv.Close()

	cfg := fmt.Sprintf(`{
		"model": "test/m",
		"small_model": "test/m",
		"provider": {"test": {"name": "test",
			"options": {"baseURL": %q, "apiKey": "k"},
			"models": {"m": {"name": "m", "tool_call": true,
				"limit": {"context": 100000, "output": 4096}}}}}
	}`, srv.URL+"/v1")
	i, s := setupTitleTest(t, cfg)

	if _, err := i.Prompt(s, textInput("please fix the login bug in the auth service")); err != nil {
		t.Fatal(err)
	}
	// let the async title goroutine settle
	waitUntil(t, 3*time.Second, func() bool {
		fresh, err := i.Mgr.Find(s.ID)
		return err == nil && fresh.Title == "Fix Login Bug"
	})
	fresh, err := i.Mgr.Find(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Title != "Fix Login Bug" {
		t.Fatalf("title did not stick: got %q, want %q", fresh.Title, "Fix Login Bug")
	}
}

// A failing small model must fall back to the prompt's own model.
func TestTitleFallsBackToMainModel(t *testing.T) {
	var smallReqs atomic.Int32
	small := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		smallReqs.Add(1)
		http.Error(w, "down", http.StatusBadRequest) // non-retryable
	}))
	defer small.Close()

	var mainTitleReqs atomic.Int32
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		if bytes.Contains(body, []byte("title generator")) {
			mainTitleReqs.Add(1)
			chunk := map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "Fix Login Bug"}}}}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			respondEmptyStop(w)
			fl.Flush()
			return
		}
		respondEmptyStop(w)
	}))
	defer main.Close()

	cfg := fmt.Sprintf(`{
		"model": "test/m",
		"small_model": "small/s",
		"provider": {
			"test": {"name": "test",
				"options": {"baseURL": %q, "apiKey": "k"},
				"models": {"m": {"name": "m", "tool_call": true,
					"limit": {"context": 100000, "output": 4096}}}},
			"small": {"name": "small",
				"options": {"baseURL": %q, "apiKey": "k"},
				"models": {"s": {"name": "s"}}}
		}
	}`, main.URL+"/v1", small.URL+"/v1")
	i, s := setupTitleTest(t, cfg)

	if _, err := i.Prompt(s, textInput("please fix the login bug in the auth service")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, func() bool {
		fresh, err := i.Mgr.Find(s.ID)
		return err == nil && fresh.Title == "Fix Login Bug"
	})
	fresh, err := i.Mgr.Find(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Title != "Fix Login Bug" {
		t.Fatalf("expected fallback title, got %q", fresh.Title)
	}
	if smallReqs.Load() != 1 {
		t.Fatalf("expected exactly 1 attempt against the small model, got %d", smallReqs.Load())
	}
	if mainTitleReqs.Load() != 1 {
		t.Fatalf("expected exactly 1 fallback title request to the main model, got %d", mainTitleReqs.Load())
	}
}
