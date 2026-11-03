package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
)

// effortRecorder collects the reasoning_effort of every request body that
// reaches the fake provider.
type effortRecorder struct {
	mu  sync.Mutex
	all []string
}

func (e *effortRecorder) add(v string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.all = append(e.all, v)
}

// seen returns the distinct non-empty reasoning_effort values recorded so far
// (the async title-generation request carries none and is ignored).
func (e *effortRecorder) seen() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := map[string]bool{}
	for _, v := range e.all {
		if v != "" {
			m[v] = true
		}
	}
	out := []string{}
	for v := range m {
		out = append(out, v)
	}
	return out
}

func (e *effortRecorder) reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.all = nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// setupEffortTest is setupAbortTest with a reasoning-capable model and a
// handler that records the reasoning_effort of every request body.
func setupEffortTest(t *testing.T) (*Instance, *session.Session, *effortRecorder) {
	t.Helper()
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	rec := &effortRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ReasoningEffort string `json:"reasoning_effort"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.add(body.ReasoningEffort)
		respondEmptyStop(w)
	}))
	t.Cleanup(srv.Close)

	work := t.TempDir()
	cfg := fmt.Sprintf(`{
		"model": "test/m",
		"small_model": "test/m",
		"provider": {"test": {"name": "test",
			"options": {"baseURL": %q, "apiKey": "k"},
			"models": {"m": {"name": "m", "tool_call": true, "reasoning": true,
				"limit": {"context": 100000, "output": 4096}}}}}
	}`, srv.URL+"/v1")
	if err := os.WriteFile(filepath.Join(work, "intellexor.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	i := New(work)
	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "effort test"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	return i, s, rec
}

// The per-message variant must reach the provider request body as
// reasoning_effort, low vs high must visibly differ, and an unset variant
// must fall back to the previous hardcoded default (medium).
func TestEffortReachesProvider(t *testing.T) {
	i, s, rec := setupEffortTest(t)

	if _, err := i.Prompt(s, PromptInput{Agent: "build", Variant: "low", Parts: textInput("low effort").Parts}); err != nil {
		t.Fatalf("prompt failed: %v", err)
	}
	if seen := rec.seen(); len(seen) != 1 || !contains(seen, "low") {
		t.Fatalf("want only reasoning_effort=low, got %v", seen)
	}
	rec.reset()

	if _, err := i.Prompt(s, PromptInput{Agent: "build", Variant: "high", Parts: textInput("x").Parts}); err != nil {
		t.Fatalf("prompt failed: %v", err)
	}
	if seen := rec.seen(); len(seen) != 1 || !contains(seen, "high") {
		t.Fatalf("want only reasoning_effort=high, got %v", seen)
	}
	rec.reset()

	if _, err := i.Prompt(s, textInput("default effort")); err != nil {
		t.Fatalf("prompt failed: %v", err)
	}
	if seen := rec.seen(); len(seen) != 1 || !contains(seen, "medium") {
		t.Fatalf("unset variant must default to medium, got %v", seen)
	}

	// the variant is stamped on the messages it produced
	msgs, _ := i.Mgr.LoadMessages(s)
	var sawUser, sawAsst bool
	for _, m := range msgs {
		if u, ok := m.Info.(session.UserMessage); ok && u.Model.Variant == "high" {
			sawUser = true
		}
		if a, ok := m.Info.(session.AssistantMessage); ok && a.Variant == "high" {
			sawAsst = true
		}
	}
	if !sawUser || !sawAsst {
		t.Fatalf("variant not stamped on messages (user=%v assistant=%v)", sawUser, sawAsst)
	}
}

// The session's persisted model variant is the fallback when the prompt
// carries no explicit one.
func TestEffortFromSessionModel(t *testing.T) {
	i, s, rec := setupEffortTest(t)
	s.Model = &session.ModelRef{ID: "m", ProviderID: "test", Variant: "minimal"}
	if err := i.Mgr.Save(s); err != nil {
		t.Fatal(err)
	}

	if _, err := i.Prompt(s, textInput("session variant")); err != nil {
		t.Fatalf("prompt failed: %v", err)
	}
	if seen := rec.seen(); !contains(seen, "minimal") {
		t.Fatalf("session model variant not applied, got %v", seen)
	}
}
