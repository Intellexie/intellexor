package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type ContentBlock struct {
	Type      string          `json:"type"` // text | thinking | tool_use | tool_result | image
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`     // tool call id (tool_use) / result ref
	Name      string          `json:"name,omitempty"`   // tool name
	Input     json.RawMessage `json:"input,omitempty"`  // tool_use args
	Output    string          `json:"output,omitempty"` // tool_result text
	IsError   bool            `json:"is_error,omitempty"`
	MediaType string          `json:"media_type,omitempty"` // image
	Data      string          `json:"data,omitempty"`       // image base64
}

type Msg struct {
	Role    string         `json:"role"` // user | assistant
	Content []ContentBlock `json:"content"`
}

type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type Usage struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	Reasoning  float64 `json:"reasoning"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

type Request struct {
	Model       string
	System      string
	Messages    []Msg
	Tools       []ToolDef
	MaxTokens   int
	Temperature *float64
	Reasoning   bool           // ask for thinking/reasoning effort
	Extra       map[string]any // merged into the request body (model.options, e.g. thinking_token_budget)
}

type StreamError struct {
	Message      string `json:"message"`
	StatusCode   int    `json:"statusCode,omitempty"`
	IsRetryable  bool   `json:"isRetryable"`
	ResponseBody string `json:"responseBody,omitempty"`
}

func (e *StreamError) Error() string { return e.Message }

type StreamEvent struct {
	Type   string // text-start | text-delta | text-end | reasoning-start | reasoning-delta | reasoning-end | tool-call | finish | error
	ID     string
	Name   string
	Text   string
	Input  json.RawMessage
	Finish string
	Usage  *Usage
	Err    *StreamError
}

type Client struct {
	Resolved *Resolved
}

func (r *Resolved) Client() *Client { return &Client{Resolved: r} }

// SetStreamIdleTimeoutForTest overrides the idle window (tests only; the app
// test suites are not parallel, so a plain var write is fine).
func SetStreamIdleTimeoutForTest(d time.Duration) { streamIdleTimeout = d }

// streamIdle is the idle-watchdog window for this provider's streams. Local
// runtimes are exempt: a cold ollama/lmstudio model load makes no bytes for
// minutes and looks exactly like a stalled connection. Everything else gets
// the default, so a hung proxy or silently-dropped connection errors out
// (retryable) instead of freezing the agent loop forever.
func (c *Client) streamIdle() time.Duration {
	switch c.Resolved.ID {
	case "ollama", "lmstudio":
		return 0
	}
	return streamIdleTimeout
}

type httpError struct {
	status int
	body   string
}

func (h *httpError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", h.status, truncate(h.body, 2000))
}

func isRetryable(status int) bool {
	return status == 408 || status == 409 || status == 429 || status >= 500
}

func toStreamError(err error) *StreamError {
	if se, ok := err.(*StreamError); ok {
		return se
	}
	if he, ok := err.(*httpError); ok {
		return &StreamError{
			Message:      fmt.Sprintf("Unexpected error, request failed with status %d: %s", he.status, truncate(he.body, 500)),
			StatusCode:   he.status,
			IsRetryable:  isRetryable(he.status),
			ResponseBody: truncate(he.body, 2000),
		}
	}
	msg := err.Error()
	return &StreamError{Message: msg, IsRetryable: strings.Contains(msg, "connection reset") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "EOF")}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Retry backoff for retryable stream failures: doubling from retryBaseDelay,
// capped at retryMaxDelay (5s, 10s, 20s, 40s, 80s, ..., 10m, 10m, ...). The
// retries are patient — a flaky provider is waited out instead of failing
// the turn — but not unbounded: once retryBudget has been spent since the
// first attempt, the last error is surfaced so the turn ends (a permanently
// hung endpoint must not keep the session busy forever). Aborting the
// session (ctx.Done) always wins over the backoff sleep.
var (
	retryBaseDelay = 5 * time.Second
	retryMaxDelay  = 10 * time.Minute
	retryBudget    = 5 * time.Minute
)

// SetRetryBackoffForTest overrides the backoff bounds (tests only; the app
// test suites are not parallel, so a plain var write is fine).
func SetRetryBackoffForTest(base, max time.Duration) {
	retryBaseDelay, retryMaxDelay = base, max
}

// SetRetryBudgetForTest overrides the total retry budget (tests only).
func SetRetryBudgetForTest(d time.Duration) { retryBudget = d }

// Backoff exposes the retry delay schedule to the app-level step retry. A
// mid-stream failure (after partial output was forwarded) cannot be retried
// inside Stream without duplicating the text already on the wire, so the
// prompt loop retries the whole step and reuses this schedule.
func Backoff(attempt int) time.Duration { return backoff(attempt) }

// backoff returns the wait before retry after the given 1-based attempt.
// Doubled multiplicatively but clamped, so it is overflow-safe for any
// attempt count.
func backoff(attempt int) time.Duration {
	delay := retryBaseDelay
	for i := 1; i < attempt && delay < retryMaxDelay; i++ {
		delay *= 2
	}
	if delay > retryMaxDelay {
		delay = retryMaxDelay
	}
	return delay
}

// Stream runs the request with retries, invoking onRetry(attempt, err, next
// time) before each backoff sleep.
//
// The protocol handlers do their HTTP work in a background goroutine and
// report failures as "error" events, so the retry loop pumps each attempt's
// channel: a retryable error is retried with exponential backoff (5s
// doubling, capped at 10m) as long as nothing has been forwarded to the
// consumer yet. Once partial output was streamed, a retry would replay and
// duplicate it, so the error is passed through instead. The retries are
// bounded by retryBudget, spent from the first attempt: once it is used up
// the last error is surfaced, so a permanently hung endpoint ends the turn
// (with an error) instead of keeping the session busy forever. The loop
// also exits on success, a non-retryable error, an error after partial
// output, or an aborted request (abort always wins over the backoff sleep).
func (c *Client) Stream(ctx context.Context, req Request, onRetry func(attempt int, err *StreamError, next time.Time)) (<-chan StreamEvent, error) {
	out := make(chan StreamEvent, 256)
	go func() {
		defer close(out)
		start := time.Now()
		for attempt := 1; ; attempt++ {
			var last *StreamError
			ch, err := c.streamOnce(ctx, req)
			if err != nil {
				// synchronous failure (request build): nothing was streamed
				last = toStreamError(err)
				if !last.IsRetryable || ctx.Err() != nil {
					out <- StreamEvent{Type: "error", Err: last}
					return
				}
			} else {
				forwarded := false
				for ev := range ch {
					if ev.Type == "error" {
						if ev.Err != nil && ev.Err.IsRetryable && ctx.Err() == nil && !forwarded {
							last = ev.Err
							break
						}
						out <- ev
						return
					}
					forwarded = true
					out <- ev
				}
				if last == nil {
					return // clean finish: every event forwarded
				}
			}
			// retryable failure with nothing on the wire yet: give up when
			// the budget is spent (the error must reach the consumer so the
			// turn ends), otherwise back off and retry
			if time.Since(start) >= retryBudget {
				out <- StreamEvent{Type: "error", Err: last}
				return
			}
			delay := backoff(attempt)
			if onRetry != nil {
				onRetry(attempt, last, time.Now().Add(delay))
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				out <- StreamEvent{Type: "error", Err: &StreamError{Message: "Aborted", IsRetryable: false}}
				return
			case <-timer.C:
			}
		}
	}()
	return out, nil
}

func (c *Client) streamOnce(ctx context.Context, req Request) (<-chan StreamEvent, error) {
	switch c.Resolved.Protocol {
	case "anthropic":
		return c.streamAnthropic(ctx, req)
	case "gemini":
		return c.streamGemini(ctx, req)
	default:
		return c.streamOpenAI(ctx, req)
	}
}
