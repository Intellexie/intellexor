package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBackoffSchedule(t *testing.T) {
	SetRetryBackoffForTest(5*time.Second, 10*time.Minute)
	defer SetRetryBackoffForTest(5*time.Second, 10*time.Minute)

	want := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		80 * time.Second,
		160 * time.Second,
		320 * time.Second,
		10 * time.Minute, // 640s capped
		10 * time.Minute,
		10 * time.Minute, // far beyond overflow territory
	}
	for i, w := range want {
		if got := backoff(i + 1); got != w {
			t.Errorf("backoff(attempt %d) = %v, want %v", i+1, got, w)
		}
	}
}

// A retryable failure (HTTP 503) must be retried until the provider recovers:
// the stream succeeds on a later attempt and every failure is reported via
// onRetry with the next attempt time.
func TestStreamRetriesUntilSuccess(t *testing.T) {
	SetRetryBackoffForTest(time.Millisecond, 2*time.Millisecond)
	defer SetRetryBackoffForTest(5*time.Second, 10*time.Minute)

	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			http.Error(w, "overloaded", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := map[string]any{"choices": []map[string]any{{
			"delta": map[string]any{"content": "recovered"}, "finish_reason": "stop",
		}}}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	c := &Client{Resolved: &Resolved{ID: "test", Protocol: "openai", BaseURL: ts.URL}}
	var retries []int
	ch, err := c.Stream(context.Background(), Request{Model: "m"}, func(attempt int, serr *StreamError, next time.Time) {
		retries = append(retries, attempt)
		if !serr.IsRetryable {
			t.Errorf("retry %d: expected retryable error, got %q", attempt, serr.Message)
		}
		if !next.After(time.Now().Add(-time.Second)) {
			t.Errorf("retry %d: next time %v not in the future", attempt, next)
		}
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}

	var sawText, sawFinish bool
	for ev := range ch {
		switch ev.Type {
		case "text-delta":
			sawText = true
		case "finish":
			sawFinish = true
		case "error":
			t.Fatalf("unexpected error event: %s", ev.Err.Message)
		}
	}
	if !sawText || !sawFinish {
		t.Fatalf("expected text + finish after recovery, got text=%v finish=%v", sawText, sawFinish)
	}
	if len(retries) != 2 || retries[0] != 1 || retries[1] != 2 {
		t.Fatalf("expected onRetry for attempts 1 and 2, got %v", retries)
	}
}

// The retry budget bounds the patience: a permanently stalled endpoint must
// surface its error once the budget is spent instead of retrying forever and
// keeping the session busy.
func TestStreamGivesUpAfterRetryBudget(t *testing.T) {
	SetRetryBackoffForTest(time.Millisecond, 2*time.Millisecond)
	defer SetRetryBackoffForTest(5*time.Second, 10*time.Minute)
	SetRetryBudgetForTest(300 * time.Millisecond)
	defer SetRetryBudgetForTest(5 * time.Minute)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	c := &Client{Resolved: &Resolved{ID: "test", Protocol: "openai", BaseURL: ts.URL}}
	start := time.Now()
	ch, err := c.Stream(context.Background(), Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	var gotErr *StreamError
	for ev := range ch {
		if ev.Type == "error" {
			gotErr = ev.Err
		}
	}
	if gotErr == nil {
		t.Fatal("expected an error event after the budget was spent")
	}
	if !gotErr.IsRetryable {
		t.Errorf("expected the surfaced error to stay retryable, got %q", gotErr.Message)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("giving up took %v, expected ~the budget", elapsed)
	}
}

// Retries are unbounded, but aborting the request must cut the backoff sleep
// short instead of waiting for the provider forever. The abort surfaces as an
// "Aborted" error event on the stream channel.
func TestStreamRetryAbortsDuringBackoff(t *testing.T) {
	SetRetryBackoffForTest(50*time.Millisecond, 50*time.Millisecond)
	defer SetRetryBackoffForTest(5*time.Second, 10*time.Minute)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{Resolved: &Resolved{ID: "test", Protocol: "openai", BaseURL: ts.URL}}
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	done := make(chan string, 1)
	go func() {
		ch, err := c.Stream(ctx, Request{Model: "m"}, nil)
		if err != nil {
			done <- "sync error: " + err.Error()
			return
		}
		for ev := range ch {
			if ev.Type == "error" {
				done <- ev.Err.Message
				return
			}
		}
		done <- ""
	}()
	select {
	case msg := <-done:
		if msg != "Aborted" {
			t.Fatalf("expected Aborted error event after cancel, got %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stream did not return after abort")
	}
}

// A retryable error that arrives AFTER partial output was streamed must be
// passed through, not retried: a retry would replay and duplicate the text.
func TestStreamNoRetryAfterPartialOutput(t *testing.T) {
	SetRetryBackoffForTest(time.Millisecond, time.Millisecond)
	defer SetRetryBackoffForTest(5*time.Second, 10*time.Minute)

	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) > 1 {
			t.Error("provider hit more than once: partial output was retried")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := map[string]any{"choices": []map[string]any{{
			"delta": map[string]any{"content": "partial "},
		}}}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		errChunk := map[string]any{"error": map[string]any{"message": "overloaded", "type": "server_error"}}
		b, _ = json.Marshal(errChunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}))
	defer ts.Close()

	c := &Client{Resolved: &Resolved{ID: "test", Protocol: "openai", BaseURL: ts.URL}}
	ch, err := c.Stream(context.Background(), Request{Model: "m"}, func(int, *StreamError, time.Time) {
		t.Error("onRetry invoked after partial output")
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	var gotErr *StreamError
	var text string
	for ev := range ch {
		switch ev.Type {
		case "text-delta":
			text += ev.Text
		case "error":
			gotErr = ev.Err
		}
	}
	if gotErr == nil || gotErr.Message != "overloaded" {
		t.Fatalf("expected the overload error to pass through, got %v (text=%q)", gotErr, text)
	}
	if text != "partial " {
		t.Fatalf("expected partial text to be forwarded, got %q", text)
	}
}
