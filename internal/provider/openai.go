package provider

import (
	"context"
	"encoding/json"
	"strings"
)

type oaiMsg struct {
	Role       string  `json:"role"`
	Content    any     `json:"content"` // string or []oaiPart
	ToolCalls  []oaiTC `json:"tool_calls,omitempty"`
	ToolCallID string  `json:"tool_call_id,omitempty"`
}

type oaiTC struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

func (c *Client) openaiHeaders() map[string]string {
	h := map[string]string{}
	if c.Resolved.APIKey != "" {
		h["Authorization"] = "Bearer " + c.Resolved.APIKey
	}
	for k, v := range c.Resolved.Headers {
		h[k] = v
	}
	return h
}

func (c *Client) streamOpenAI(ctx context.Context, req Request) (<-chan StreamEvent, error) {
	var messages []oaiMsg
	if req.System != "" {
		messages = append(messages, oaiMsg{Role: "system", Content: req.System})
	}
	// group: assistant text + tool calls in one message; tool results as role=tool
	var cur *oaiMsg
	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			var parts []oaiPart
			var toolMsgs []oaiMsg
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					parts = append(parts, oaiPart{Type: "text", Text: b.Text})
				case "image":
					parts = append(parts, oaiPart{Type: "image_url", ImageURL: &struct {
						URL string `json:"url"`
					}{URL: "data:" + b.MediaType + ";base64," + b.Data}})
				case "tool_result":
					toolMsgs = append(toolMsgs, oaiMsg{Role: "tool", ToolCallID: b.ID, Content: b.Output})
				}
			}
			if cur != nil {
				messages = append(messages, *cur)
				cur = nil
			}
			for _, t := range toolMsgs {
				messages = append(messages, t)
			}
			if len(parts) > 0 {
				if len(parts) == 1 && parts[0].Type == "text" {
					messages = append(messages, oaiMsg{Role: "user", Content: parts[0].Text})
				} else {
					messages = append(messages, oaiMsg{Role: "user", Content: parts})
				}
			}
		case "assistant":
			if cur == nil {
				cur = &oaiMsg{Role: "assistant", Content: ""}
			}
			var texts []string
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					texts = append(texts, b.Text)
				case "tool_use":
					tc := oaiTC{ID: b.ID, Type: "function"}
					tc.Function.Name = b.Name
					tc.Function.Arguments = string(rawOrEmpty(b.Input))
					cur.ToolCalls = append(cur.ToolCalls, tc)
				}
			}
			if len(texts) > 0 {
				cur.Content = strings.Join(texts, "")
			}
		}
	}
	if cur != nil {
		messages = append(messages, *cur)
	}

	tools := []map[string]any{}
	for _, t := range req.Tools {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"parameters":  rawOrEmpty(t.Parameters),
		}})
	}
	body := map[string]any{
		"model":          req.Model,
		"messages":       messages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if req.MaxTokens > 0 {
		body["max_completion_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	} else {
		body["temperature"] = 0
	}
	if req.Reasoning {
		body["reasoning_effort"] = "medium"
	}
	for k, v := range req.Extra {
		body[k] = v
	}

	url := strings.TrimRight(c.Resolved.BaseURL, "/") + "/chat/completions"
	out := make(chan StreamEvent, 256)
	go func() {
		defer close(out)
		textStarted := false
		reasoningStarted := false
		type tcState struct {
			id   string
			name string
			args strings.Builder
		}
		tcs := map[int]*tcState{}
		var tcOrder []int
		var usage Usage
		var finish string

		closeText := func() {
			if textStarted {
				out <- StreamEvent{Type: "text-end", ID: "0"}
				textStarted = false
			}
		}
		closeReasoning := func() {
			if reasoningStarted {
				out <- StreamEvent{Type: "reasoning-end", ID: "0"}
				reasoningStarted = false
			}
		}
		emitToolCalls := func() {
			for _, idx := range tcOrder {
				st := tcs[idx]
				if st == nil || st.id == "" {
					continue
				}
				args := strings.TrimSpace(st.args.String())
				if args == "" {
					args = "{}"
				}
				if !json.Valid([]byte(args)) {
					args = "{}"
				}
				out <- StreamEvent{Type: "tool-call", ID: st.id, Name: st.name, Input: json.RawMessage(args)}
			}
			tcs = map[int]*tcState{}
			tcOrder = nil
		}

		err := postSSE(ctx, url, c.openaiHeaders(), body, func(event string, data []byte) (bool, error) {
			var chunk struct {
				Choices []struct {
					Delta struct {
						Role             string `json:"role"`
						Content          string `json:"content"`
						ReasoningContent string `json:"reasoning_content"`
						ToolCalls        []struct {
							Index    int    `json:"index"`
							ID       string `json:"id"`
							Function struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens        float64 `json:"prompt_tokens"`
					CompletionTokens    float64 `json:"completion_tokens"`
					PromptTokensDetails struct {
						CachedTokens float64 `json:"cached_tokens"`
					} `json:"prompt_tokens_details"`
					CompletionTokensDetails struct {
						ReasoningTokens float64 `json:"reasoning_tokens"`
					} `json:"completion_tokens_details"`
				} `json:"usage"`
				Error *struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(data, &chunk); err != nil {
				return true, nil
			}
			if chunk.Error != nil {
				out <- StreamEvent{Type: "error", Err: &StreamError{Message: chunk.Error.Message, IsRetryable: true}}
				return true, nil
			}
			if chunk.Usage != nil {
				usage.Input = chunk.Usage.PromptTokens - chunk.Usage.PromptTokensDetails.CachedTokens
				usage.CacheRead = chunk.Usage.PromptTokensDetails.CachedTokens
				usage.Output = chunk.Usage.CompletionTokens
				usage.Reasoning = chunk.Usage.CompletionTokensDetails.ReasoningTokens
			}
			for _, ch := range chunk.Choices {
				if ch.Delta.ReasoningContent != "" {
					closeText()
					if !reasoningStarted {
						out <- StreamEvent{Type: "reasoning-start", ID: "0"}
						reasoningStarted = true
					}
					out <- StreamEvent{Type: "reasoning-delta", ID: "0", Text: ch.Delta.ReasoningContent}
				}
				if ch.Delta.Content != "" {
					closeReasoning()
					if !textStarted {
						out <- StreamEvent{Type: "text-start", ID: "0"}
						textStarted = true
					}
					out <- StreamEvent{Type: "text-delta", ID: "0", Text: ch.Delta.Content}
				}
				for _, tc := range ch.Delta.ToolCalls {
					st, ok := tcs[tc.Index]
					if !ok {
						closeText()
						closeReasoning()
						st = &tcState{}
						tcs[tc.Index] = st
						tcOrder = append(tcOrder, tc.Index)
					}
					if tc.ID != "" {
						st.id = tc.ID
					}
					if tc.Function.Name != "" {
						st.name = tc.Function.Name
					}
					st.args.WriteString(tc.Function.Arguments)
				}
				if ch.FinishReason != nil && *ch.FinishReason != "" {
					finish = *ch.FinishReason
				}
			}
			return true, nil
		})
		closeText()
		closeReasoning()
		emitToolCalls()
		if err != nil && ctx.Err() == nil {
			out <- StreamEvent{Type: "error", Err: toStreamError(err)}
			return
		}
		u := usage
		out <- StreamEvent{Type: "finish", Finish: finish, Usage: &u}
	}()
	return out, nil
}
