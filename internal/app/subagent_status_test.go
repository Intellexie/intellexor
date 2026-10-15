package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The web UI shows a progress indicator on a task row while the subagent
// session is still ongoing. The indicator's data source is the session status
// map (GET /session/status) plus session.status / session.idle events for the
// child session — both must expose the child while it runs and clear when it
// finishes.
func TestSubagentSessionStatusExposedWhileRunning(t *testing.T) {
	childStarted := make(chan struct{})
	releaseChild := make(chan struct{})
	var childOnce sync.Once
	var apiCalls int32
	h := func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("title generator")) {
			respondEmptyStop(w)
			return
		}
		n := atomic.AddInt32(&apiCalls, 1)
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
		childOnce.Do(func() { close(childStarted) })
		<-releaseChild
		txt := map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "child done"}, "finish_reason": "stop"}}}
		b, _ := json.Marshal(txt)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}

	i, s := setupAbortTest(t, h)

	// collect the events the SSE stream would deliver
	var mu sync.Mutex
	childBusyEvent := false
	childIdleEvent := false
	ch := i.Hub.Subscribe()
	defer i.Hub.Unsubscribe(ch)
	go func() {
		for ev := range ch {
			if ev.Type != "session.status" && ev.Type != "session.idle" {
				continue
			}
			sid, _ := ev.Properties["sessionID"].(string)
			if sid == "" || sid == s.ID {
				continue // only the child's events matter here
			}
			mu.Lock()
			if ev.Type == "session.idle" {
				childIdleEvent = true
			} else if st, ok := ev.Properties["status"].(StatusInfo); ok {
				if tp, _ := st["type"].(string); tp == "busy" {
					childBusyEvent = true
				}
			}
			mu.Unlock()
		}
	}()

	done := make(chan struct{})
	go func() { defer close(done); _, _ = i.Prompt(s, textInput("spawn subagent")) }()

	<-childStarted
	var childID string
	waitUntil(t, 10*time.Second, func() bool {
		msgs, _ := i.Mgr.LoadMessages(s)
		for _, m := range msgs {
			for _, p := range m.Parts {
				if p.Type == "tool" && p.Tool == "task" && p.State != nil && p.State.Metadata != nil {
					if id, _ := p.State.Metadata["sessionId"].(string); id != "" {
						childID = id
						return true
					}
				}
			}
		}
		return false
	})
	if childID == "" {
		t.Fatal("task part never carried metadata.sessionId")
	}

	// while the subagent is ongoing, its session must be busy — this is what
	// GET /session/status returns and what drives the row's progress indicator
	waitUntil(t, 5*time.Second, func() bool {
		st, ok := i.Statuses()[childID]
		return ok && st["type"] != "idle"
	})
	if st, ok := i.Statuses()[childID]; !ok || st["type"] != "busy" {
		t.Fatalf("child session status while running = %v, %v; want busy", st, ok)
	}
	mu.Lock()
	busySeen := childBusyEvent
	mu.Unlock()
	if !busySeen {
		t.Fatal("no session.status busy event published for the child session")
	}

	close(releaseChild)
	<-done

	// once the subagent finishes, the status must be gone (indicator clears)
	waitUntil(t, 5*time.Second, func() bool {
		_, ok := i.Statuses()[childID]
		return !ok
	})
	if _, ok := i.Statuses()[childID]; ok {
		t.Fatal("child session status still present after the subagent finished")
	}
	mu.Lock()
	idleSeen := childIdleEvent
	mu.Unlock()
	if !idleSeen {
		t.Fatal("no session.idle event published for the child session")
	}
}
