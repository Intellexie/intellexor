package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Intellexie/intellexor/internal/board"
	"github.com/Intellexie/intellexor/internal/provider"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
)

// vllmSSE mimics a real vLLM SSE response for a reasoning model making a tool
// call: reasoning_content deltas first, then the tool call streamed
// incrementally across chunks, then finish_reason, then a usage-only chunk
// (stream_options.include_usage) with an EMPTY choices array, then [DONE].
func vllmToolCallStep(w http.ResponseWriter, fl http.Flusher, callID string) {
	// reasoning phase (vLLM emits delta.reasoning_content for thinking models)
	for _, tok := range []string{"Let me ", "check the ", "task... "} {
		chunk := map[string]any{"choices": []map[string]any{{
			"delta": map[string]any{"reasoning_content": tok},
		}}}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	fl.Flush()
	// tool call: first chunk carries id+name, later chunks carry argument pieces
	first := map[string]any{"choices": []map[string]any{{
		"delta": map[string]any{"tool_calls": []map[string]any{{
			"index": 0, "id": callID, "type": "function",
			"function": map[string]any{"name": "board", "arguments": ""},
		}}},
	}}}
	b, _ := json.Marshal(first)
	fmt.Fprintf(w, "data: %s\n\n", b)
	for _, piece := range []string{`{"action"`, `:"move"`, `,"column"`, `:"done"`, `}`} {
		chunk := map[string]any{"choices": []map[string]any{{
			"delta": map[string]any{"tool_calls": []map[string]any{{
				"index":    0,
				"function": map[string]any{"arguments": piece},
			}}},
		}}}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	fl.Flush()
	final := map[string]any{"choices": []map[string]any{{
		"delta": map[string]any{}, "finish_reason": "tool_calls",
	}}}
	b, _ = json.Marshal(final)
	fmt.Fprintf(w, "data: %s\n\n", b)
	// usage-only chunk: empty choices (include_usage)
	usage := map[string]any{"choices": []any{}, "usage": map[string]any{
		"prompt_tokens": 100, "completion_tokens": 20,
		"prompt_tokens_details": map[string]any{"cached_tokens": 0},
	}}
	b, _ = json.Marshal(usage)
	fmt.Fprintf(w, "data: %s\n\n", b)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	fl.Flush()
}

// TestVLLMStyleBoardMoveThenFinish reproduces the production flow against a
// vLLM-shaped stream: reasoning + incremental tool call, then a normal second
// step. The agent must finish (not freeze) with the card in done.
func TestVLLMStyleBoardMoveThenFinish(t *testing.T) {
	var calls int32
	h := func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "title generator") {
			respondEmptyStop(w)
			return
		}
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			vllmToolCallStep(w, fl, "chatcmpl-tool-1")
		default:
			chunk := map[string]any{"choices": []map[string]any{{
				"delta": map[string]any{"content": "All done."}, "finish_reason": "stop",
			}}}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			fl.Flush()
		}
	}

	i, s := setupAbortTest(t, h)
	card := linkCard(t, i, s, "Ship it", "inprogress")

	done := make(chan error, 1)
	go func() { _, err := i.Prompt(s, textInput("finish the card")); done <- err }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("prompt failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("agent froze: prompt did not finish within 10s")
	}

	cards, _ := board.Get(store.ProjectID(i.Directory))
	for _, c := range cards {
		if c.ID == card.ID && c.Column != "done" {
			t.Fatalf("card not moved to done: %s", c.Column)
		}
	}
}

// TestStalledStreamAfterToolCallRecovers is the regression test for the
// freeze: after the board tool completes, the NEXT provider request opens a
// connection but never sends a byte (hung vLLM / dead proxy / silent NAT
// drop). The agent must NOT freeze forever — the idle watchdog must abandon
// the stream, surface a retryable error on the assistant message, and let
// the prompt return, with the card in done and the tool part completed.
func TestStalledStreamAfterToolCallRecovers(t *testing.T) {
	provider.SetStreamIdleTimeoutForTest(500 * time.Millisecond)
	defer provider.SetStreamIdleTimeoutForTest(3 * time.Minute)

	var calls int32
	h := func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "title generator") {
			respondEmptyStop(w)
			return
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			vllmToolCallStep(w, fl, "chatcmpl-tool-1")
			return
		}
		// second step: accept the request, then go silent forever
		// (no bytes, no [DONE], connection kept open)
		<-r.Context().Done()
	}

	i, s := setupAbortTest(t, h)
	card := linkCard(t, i, s, "Ship it", "inprogress")

	done := make(chan *session.AssistantMessage, 1)
	go func() {
		wp, _ := i.Prompt(s, textInput("finish the card"))
		if a, ok := wp.Info.(session.AssistantMessage); ok {
			done <- &a
		} else {
			done <- nil
		}
	}()

	var asst *session.AssistantMessage
	select {
	case asst = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("agent froze: prompt did not return after the stream stalled")
	}
	if asst == nil {
		t.Fatal("no assistant message returned")
	}
	if asst.Error == nil {
		t.Fatalf("expected a stall error on the assistant message, got finish=%q", asst.Finish)
	}
	if !strings.Contains(asst.Error.Name, "APIError") {
		t.Fatalf("expected APIError, got %q", asst.Error.Name)
	}
	if msg, _ := asst.Error.Data["message"].(string); !strings.Contains(msg, "stalled") {
		t.Fatalf("expected the stall message, got %q", msg)
	}

	// the card DID move and the tool part IS completed — the work before the
	// stall is preserved
	cards, _ := board.Get(store.ProjectID(i.Directory))
	moved := false
	for _, c := range cards {
		if c.ID == card.ID && c.Column == "done" {
			moved = true
		}
	}
	if !moved {
		t.Fatal("card was not moved to done")
	}
	msgs, _ := i.Mgr.LoadMessages(s)
	found := false
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == "tool" && p.Tool == "board" && p.State != nil && p.State.Status == "completed" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("no completed board tool part stored")
	}
}

// A stream that stalls MID-body (bytes flowed, then silence) must also be
// abandoned instead of freezing the loop.
func TestStalledMidStreamRecovers(t *testing.T) {
	provider.SetStreamIdleTimeoutForTest(500 * time.Millisecond)
	defer provider.SetStreamIdleTimeoutForTest(3 * time.Minute)

	h := func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "title generator") {
			respondEmptyStop(w)
			return
		}
		chunk := map[string]any{"choices": []map[string]any{{
			"delta": map[string]any{"content": "partial "},
		}}}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
		// then silence forever
		<-r.Context().Done()
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
		if asst == nil || asst.Error == nil {
			t.Fatal("expected a stall error on the assistant message")
		}
		if msg, _ := asst.Error.Data["message"].(string); !strings.Contains(msg, "stalled") {
			t.Fatalf("expected the stall message, got %q", msg)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("agent froze: prompt did not return after the mid-stream stall")
	}
}
