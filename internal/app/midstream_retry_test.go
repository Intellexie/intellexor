package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Intellexie/intellexor/internal/provider"
	"github.com/Intellexie/intellexor/internal/session"
)

// TestMidStreamResetRetriesAndContinues covers the production failure
// "read tcp ...: connection reset by peer": the provider drops the connection
// mid-stream after partial text was already forwarded. The turn must not die —
// the step is retried with a fresh assistant message, the resent request ends
// with the last user message (the dead partial turn is excluded from
// history), and the run completes with the retry's content.
func TestMidStreamResetRetriesAndContinues(t *testing.T) {
	provider.SetRetryBackoffForTest(time.Millisecond, 2*time.Millisecond)
	defer provider.SetRetryBackoffForTest(5*time.Second, 10*time.Minute)

	var calls int32
	var mu sync.Mutex
	var retryBody string
	h := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "title generator") {
			respondEmptyStop(w)
			return
		}
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			// stream one partial token, then kill the connection mid-stream
			w.Header().Set("Content-Type", "text/event-stream")
			chunk := map[string]any{"choices": []map[string]any{{
				"delta": map[string]any{"content": "partial half-sentence"},
			}}}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack failed: %v", err)
				return
			}
			conn.Close()
		default:
			mu.Lock()
			retryBody = string(body)
			mu.Unlock()
			chunk := map[string]any{"choices": []map[string]any{{
				"delta": map[string]any{"content": "recovered full answer"}, "finish_reason": "stop",
			}}}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			w.(http.Flusher).Flush()
		}
	}

	i, s := setupAbortTest(t, h)
	done := make(chan *session.AssistantMessage, 1)
	go func() {
		wp, _ := i.Prompt(s, textInput("hello"))
		if a, ok := wp.Info.(session.AssistantMessage); ok {
			done <- &a
		} else {
			done <- nil
		}
	}()
	select {
	case asst := <-done:
		if asst == nil {
			t.Fatal("no assistant message returned")
		}
		if asst.Error != nil {
			t.Fatalf("turn died on the mid-stream reset: %v", asst.Error.Data["message"])
		}
		if asst.Finish != "stop" {
			t.Fatalf("expected finish=stop after the retry, got %q", asst.Finish)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("prompt did not return after the mid-stream reset")
	}

	// the resent request must end with the user message — the dead partial
	// assistant turn must not be replayed into history
	mu.Lock()
	body := retryBody
	mu.Unlock()
	if body == "" {
		t.Fatal("no retry request captured")
	}
	if strings.Contains(body, "partial half-sentence") {
		t.Error("retry request replayed the dead partial assistant text")
	}
	if !strings.Contains(body, `"role":"user"`) || !strings.Contains(body, "hello") {
		t.Error("retry request does not carry the user message")
	}

	// the transcript keeps the failed attempt (its partial text and a retry
	// marker) and the retry's text — without duplicating the partial
	msgs, _ := i.Mgr.LoadMessages(s)
	retryPart, partialText, finalText := false, false, false
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == "retry" {
				retryPart = true
			}
			if p.Type == "text" {
				if strings.Contains(p.Text, "partial half-sentence") {
					partialText = true
				}
				if strings.Contains(p.Text, "recovered full answer") {
					finalText = true
				}
			}
		}
	}
	if !retryPart {
		t.Error("no retry part recorded in the transcript")
	}
	if !partialText {
		t.Error("partial text of the failed attempt vanished from the transcript")
	}
	if !finalText {
		t.Error("retry text missing from the transcript")
	}
}

// A clean EOF mid-stream (server closes without [DONE]/finish_reason) must
// surface a retryable error too, not a bogus finish that silently accepts the
// truncated turn.
func TestCleanEOFMidStreamIsRetryable(t *testing.T) {
	provider.SetRetryBackoffForTest(time.Millisecond, 2*time.Millisecond)
	defer provider.SetRetryBackoffForTest(5*time.Second, 10*time.Minute)

	var calls int32
	h := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "title generator") {
			respondEmptyStop(w)
			return
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			chunk := map[string]any{"choices": []map[string]any{{
				"delta": map[string]any{"content": "cut off mid"},
			}}}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
			// no [DONE], no finish_reason: hijack + close = clean EOF
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack failed: %v", err)
				return
			}
			conn.Close()
			return
		}
		chunk := map[string]any{"choices": []map[string]any{{
			"delta": map[string]any{"content": "done"}, "finish_reason": "stop",
		}}}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}

	i, s := setupAbortTest(t, h)
	done := make(chan *session.AssistantMessage, 1)
	go func() {
		wp, _ := i.Prompt(s, textInput("hello"))
		if a, ok := wp.Info.(session.AssistantMessage); ok {
			done <- &a
		} else {
			done <- nil
		}
	}()
	select {
	case asst := <-done:
		if asst == nil {
			t.Fatal("no assistant message returned")
		}
		if asst.Error != nil {
			t.Fatalf("clean EOF was not retried: %v", asst.Error.Data["message"])
		}
		if asst.Finish != "stop" {
			t.Fatalf("expected finish=stop after the retry, got %q", asst.Finish)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("prompt did not return after the clean-EOF cut")
	}
}

// When the provider keeps resetting mid-stream, the retries must stay bounded:
// after maxStepRetries the turn ends with the retryable error instead of
// looping forever.
func TestMidStreamResetGivesUpAfterMaxRetries(t *testing.T) {
	provider.SetRetryBackoffForTest(time.Millisecond, 2*time.Millisecond)
	defer provider.SetRetryBackoffForTest(5*time.Second, 10*time.Minute)

	h := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "title generator") {
			respondEmptyStop(w)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := map[string]any{"choices": []map[string]any{{
			"delta": map[string]any{"content": "partial"},
		}}}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack failed: %v", err)
			return
		}
		conn.Close()
	}

	i, s := setupAbortTest(t, h)
	done := make(chan *session.AssistantMessage, 1)
	go func() {
		wp, _ := i.Prompt(s, textInput("hello"))
		if a, ok := wp.Info.(session.AssistantMessage); ok {
			done <- &a
		} else {
			done <- nil
		}
	}()
	select {
	case asst := <-done:
		if asst == nil {
			t.Fatal("no assistant message returned")
		}
		if asst.Error == nil {
			t.Fatal("expected the turn to end with an error after the retries")
		}
		if name, _ := asst.Error.Data["isRetryable"].(bool); !name {
			t.Errorf("expected the surfaced error to stay retryable, got %v", asst.Error.Data)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("prompt did not return: retries are not bounded")
	}
}
