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

// Stream with retries, invoking onRetry(attempt, err, next time)
func (c *Client) Stream(ctx context.Context, req Request, onRetry func(attempt int, err *StreamError, next time.Time)) (<-chan StreamEvent, error) {
	var last *StreamError
	for attempt := 1; attempt <= 4; attempt++ {
		ch, err := c.streamOnce(ctx, req)
		if err == nil {
			return ch, nil
		}
		last = toStreamError(err)
		if !last.IsRetryable || ctx.Err() != nil {
			break
		}
		if attempt == 4 {
			break
		}
		delay := time.Duration(1<<uint(attempt-1)) * time.Second
		next := time.Now().Add(delay)
		if onRetry != nil {
			onRetry(attempt, last, next)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, &StreamError{Message: "Aborted", IsRetryable: false}
		case <-timer.C:
		}
	}
	out := make(chan StreamEvent, 1)
	out <- StreamEvent{Type: "error", Err: last}
	close(out)
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
