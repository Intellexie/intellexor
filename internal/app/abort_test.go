package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
)

// slowSSE streams `n` content tokens, one every `delay`, so tests can abort
// mid-stream. Finish reason "stop" is only sent after all tokens.
func slowSSE(n int, delay time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < n; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			chunk := map[string]any{
				"choices": []map[string]any{{"delta": map[string]any{"content": fmt.Sprintf("tok%d ", i)}}},
			}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
			time.Sleep(delay)
		}
		final := map[string]any{
			"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
		}
		b, _ := json.Marshal(final)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}
}

func setupAbortTest(t *testing.T, h http.HandlerFunc) (*Instance, *session.Session) {
	t.Helper()
	os.Setenv("INTELLEXOR_DATA", t.TempDir())
	store.ResetDataDirForTest()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	work := t.TempDir()
	cfg := fmt.Sprintf(`{
		"model": "test/m",
		"provider": {"test": {"name": "test",
			"options": {"baseURL": %q, "apiKey": "k"},
			"models": {"m": {"name": "m", "tool_call": true,
				"limit": {"context": 100000, "output": 4096}}}}}
	}`, srv.URL+"/v1")
	if err := os.WriteFile(filepath.Join(work, "intellexor.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	i := New(work)
	s := &session.Session{ProjectID: store.ProjectID(work), Directory: work, Title: "abort test"}
	if err := i.Mgr.Create(s); err != nil {
		t.Fatal(err)
	}
	return i, s
}

func textInput(text string) PromptInput {
	raw, _ := json.Marshal(map[string]any{"type": "text", "text": text})
	return PromptInput{Agent: "build", Parts: []json.RawMessage{raw}}
}

// waitUntil polls fn until it returns true or the deadline passes.
func waitUntil(t *testing.T, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func userTexts(msgs []session.WithParts) []string {
	var out []string
	for _, m := range msgs {
		if _, ok := m.Info.(session.UserMessage); ok {
			txt := ""
			for _, p := range m.Parts {
				if p.Type == "text" {
					txt = p.Text
				}
			}
			out = append(out, txt)
		}
	}
	return out
}

// Scenario 1: prompt A streams, user aborts mid-stream, then sends prompt B.
// Both user messages must be stored and visible.
func TestAbortThenResend(t *testing.T) {
	i, s := setupAbortTest(t, slowSSE(200, 20*time.Millisecond))

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = i.Prompt(s, textInput("first"))
	}()

	// wait for the assistant message to start (user + assistant stored)
	waitUntil(t, 5*time.Second, func() bool {
		msgs, _ := i.Mgr.LoadMessages(s)
		return len(msgs) >= 2
	})

	i.Abort(s.ID)
	<-done

	// prompt B must go through cleanly
	if _, err := i.Prompt(s, textInput("second")); err != nil {
		t.Fatalf("prompt after abort failed: %v", err)
	}

	msgs, err := i.Mgr.LoadMessages(s)
	if err != nil {
		t.Fatal(err)
	}
	got := userTexts(msgs)
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("expected both user messages visible, got %v (n=%d)", got, len(msgs))
	}
	// the last assistant message must not be marked aborted
	last := msgs[len(msgs)-1].Info.(session.AssistantMessage)
	if last.Error != nil {
		t.Fatalf("last assistant message unexpectedly aborted: %v", last.Error)
	}
}

// Scenario 3: aborting mid-stream must persist the partial text that already
// arrived, so reloads don't bring back an empty text part (which the web UI
// chokes on) and the LLM history keeps what was generated.
func TestAbortKeepsPartialText(t *testing.T) {
	i, s := setupAbortTest(t, slowSSE(500, 10*time.Millisecond))

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = i.Prompt(s, textInput("first"))
	}()

	// wait until the assistant text part exists and has accumulated tokens
	waitUntil(t, 10*time.Second, func() bool {
		msgs, _ := i.Mgr.LoadMessages(s)
		for _, m := range msgs {
			if _, ok := m.Info.(session.AssistantMessage); !ok {
				continue
			}
			for _, p := range m.Parts {
				if p.Type == "text" && strings.Contains(p.Text, "tok") {
					return true
				}
			}
		}
		return false
	})

	i.Abort(s.ID)
	<-done

	msgs, err := i.Mgr.LoadMessages(s)
	if err != nil {
		t.Fatal(err)
	}
	var textPart *session.Part
	for _, m := range msgs {
		if _, ok := m.Info.(session.AssistantMessage); !ok {
			continue
		}
		for _, p := range m.Parts {
			if p.Type == "text" {
				textPart = &p
			}
		}
	}
	if textPart == nil {
		t.Fatal("no text part stored for the aborted assistant message")
	}
	if !strings.Contains(textPart.Text, "tok") {
		t.Fatalf("aborted text part lost its partial text: %q", textPart.Text)
	}
}

// Scenario 2: prompt B is sent while A is still running (queued on the session
// lock), then the user aborts. B must still run and its user message must be
// stored exactly once.
func TestAbortWithQueuedPrompt(t *testing.T) {
	i, s := setupAbortTest(t, slowSSE(200, 20*time.Millisecond))

	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		_, _ = i.Prompt(s, textInput("first"))
	}()

	waitUntil(t, 5*time.Second, func() bool {
		msgs, _ := i.Mgr.LoadMessages(s)
		return len(msgs) >= 2
	})

	doneB := make(chan struct{})
	go func() {
		defer close(doneB)
		_, _ = i.Prompt(s, textInput("second"))
	}()

	// give B time to block on the lock, then abort A
	time.Sleep(100 * time.Millisecond)
	i.Abort(s.ID)
	<-doneA
	<-doneB

	msgs, err := i.Mgr.LoadMessages(s)
	if err != nil {
		t.Fatal(err)
	}
	got := userTexts(msgs)
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("expected both user messages visible, got %v (n=%d)", got, len(msgs))
	}
}

// Scenario 4: aborting while a tool call is running must interrupt the tool
// immediately — opencode allows aborting mid-tool-call, not only between
// steps. The fake provider emits a bash tool_call, the tool sleeps, and the
// abort must cut the prompt short well before the sleep elapses.
func TestAbortDuringToolCall(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := map[string]any{
			"choices": []map[string]any{{
				"delta": map[string]any{
					"tool_calls": []map[string]any{{
						"index": 0, "id": "call_1", "type": "function",
						"function": map[string]any{"name": "bash", "arguments": `{"command":"sleep 30"}`},
					}},
				},
			}},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		final := map[string]any{
			"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "tool_calls"}},
		}
		b, _ = json.Marshal(final)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
		// hold the connection open; the tool is running now
		<-r.Context().Done()
	}

	i, s := setupAbortTest(t, h)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = i.Prompt(s, textInput("run sleep"))
	}()

	// wait until the bash tool part is actually running
	waitUntil(t, 10*time.Second, func() bool {
		msgs, _ := i.Mgr.LoadMessages(s)
		for _, m := range msgs {
			for _, p := range m.Parts {
				if p.Type == "tool" && p.Tool == "bash" && p.State != nil && p.State.Status == "running" {
					return true
				}
			}
		}
		return false
	})

	start := time.Now()
	i.Abort(s.ID)
	select {
	case <-done:
		if el := time.Since(start); el > 5*time.Second {
			t.Fatalf("abort during tool call took %v to take effect (sleep is 30s)", el)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not return after abort during tool call")
	}

	msgs, err := i.Mgr.LoadMessages(s)
	if err != nil {
		t.Fatal(err)
	}
	var last *session.AssistantMessage
	for j := len(msgs) - 1; j >= 0; j-- {
		if a, ok := msgs[j].Info.(session.AssistantMessage); ok {
			last = &a
			break
		}
	}
	if last == nil || last.Error == nil || last.Error.Name != "MessageAbortedError" {
		t.Fatalf("expected aborted assistant message, got %+v", last)
	}
}

// Regression: a tool command that spawns a background child holding the stdout
// pipe must not deadlock the loop after abort. The shell is killed, but the
// grandchild would keep the pipe open and cmd.Wait would block on EOF forever.
func TestAbortDuringBackgroundChildTool(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := map[string]any{
			"choices": []map[string]any{{
				"delta": map[string]any{
					"tool_calls": []map[string]any{{
						"index": 0, "id": "call_1", "type": "function",
						"function": map[string]any{"name": "bash", "arguments": `{"command":"echo started; sleep 300 & wait"}`},
					}},
				},
			}},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		final := map[string]any{
			"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "tool_calls"}},
		}
		b, _ = json.Marshal(final)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
		<-r.Context().Done()
	}

	i, s := setupAbortTest(t, h)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = i.Prompt(s, textInput("run background thing"))
	}()

	waitUntil(t, 10*time.Second, func() bool {
		msgs, _ := i.Mgr.LoadMessages(s)
		for _, m := range msgs {
			for _, p := range m.Parts {
				if p.Type == "tool" && p.Tool == "bash" && p.State != nil && p.State.Status == "running" {
					return true
				}
			}
		}
		return false
	})

	start := time.Now()
	i.Abort(s.ID)
	select {
	case <-done:
		if el := time.Since(start); el > 5*time.Second {
			t.Fatalf("abort with background child took %v to take effect", el)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not return after abort with background child (pipe deadlock)")
	}
}

// The task tool part must expose metadata.sessionId while the subagent is
// still running, so the web UI can open the child session (opencode parity).
func TestTaskPartHasSessionIDWhileRunning(t *testing.T) {
	var calls int32
	childStarted := make(chan struct{})
	releaseChild := make(chan struct{})
	h := func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			// parent turn: spawn the subagent
			chunk := map[string]any{
				"choices": []map[string]any{{
					"delta": map[string]any{"tool_calls": []map[string]any{{
						"index": 0, "id": "call_1", "type": "function",
						"function": map[string]any{"name": "task", "arguments": `{"description":"Explore it","subagent_type":"general","prompt":"look around"}`},
					}}},
				}},
			}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			final := map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "tool_calls"}}}
			b, _ = json.Marshal(final)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		// child turn: hold the response open so the running window is wide
		close(childStarted)
		<-releaseChild
		txt := map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "child done"}, "finish_reason": "stop"}}}
		b, _ := json.Marshal(txt)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}

	i, s := setupAbortTest(t, h)

	done := make(chan struct{})
	go func() { defer close(done); _, _ = i.Prompt(s, textInput("spawn subagent")) }()

	<-childStarted
	var sawRunningWithID bool
	waitUntil(t, 10*time.Second, func() bool {
		msgs, _ := i.Mgr.LoadMessages(s)
		for _, m := range msgs {
			for _, p := range m.Parts {
				if p.Type == "tool" && p.Tool == "task" && p.State != nil {
					if p.State.Status == "running" && p.State.Metadata != nil && p.State.Metadata["sessionId"] != "" {
						sawRunningWithID = true
						return true
					}
				}
			}
		}
		return false
	})
	close(releaseChild)
	<-done
	if !sawRunningWithID {
		t.Fatal("task part never carried metadata.sessionId while running")
	}
	// final state must keep the id
	msgs, _ := i.Mgr.LoadMessages(s)
	found := false
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == "tool" && p.Tool == "task" && p.State != nil && p.State.Status == "completed" &&
				p.State.Metadata != nil && p.State.Metadata["sessionId"] != "" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("completed task part lost metadata.sessionId")
	}
}

// A subagent whose run yields no assistant text (provider error mid-child-run)
// must still surface the child session id on the task part — the web UI opens
// the child session instead of showing an empty result box.
func TestTaskPartKeepsSessionIDOnEmptyChildResult(t *testing.T) {
	var calls int32
	h := func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 2 {
			// child turn fails hard
			http.Error(w, `{"error":{"message":"provider exploded"}}`, http.StatusInternalServerError)
			return
		}
		if n == 1 {
			chunk := map[string]any{
				"choices": []map[string]any{{
					"delta": map[string]any{"tool_calls": []map[string]any{{
						"index": 0, "id": "call_1", "type": "function",
						"function": map[string]any{"name": "task", "arguments": `{"description":"Explore it","subagent_type":"general","prompt":"look around"}`},
					}}},
				}},
			}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			final := map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "tool_calls"}}}
			b, _ = json.Marshal(final)
			fmt.Fprintf(w, "data: %s\n\n", b)
		} else {
			txt := map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "done"}, "finish_reason": "stop"}}}
			b, _ := json.Marshal(txt)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}

	i, s := setupAbortTest(t, h)
	_, _ = i.Prompt(s, textInput("spawn failing subagent"))

	var part *session.ToolState
	waitUntil(t, 15*time.Second, func() bool {
		msgs, _ := i.Mgr.LoadMessages(s)
		for _, m := range msgs {
			for _, p := range m.Parts {
				if p.Type == "tool" && p.Tool == "task" && p.State != nil {
					part = p.State
					return true
				}
			}
		}
		return false
	})
	if part == nil {
		t.Fatal("no task part found")
	}
	if part.Metadata == nil || part.Metadata["sessionId"] == "" {
		t.Fatalf("task part lost sessionId (status=%s, metadata=%v)", part.Status, part.Metadata)
	}
}
