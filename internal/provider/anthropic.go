package provider

import (
	"context"
	"encoding/json"
	"strings"
)

func (c *Client) anthropicHeaders() map[string]string {
	h := map[string]string{
		"x-api-key":         c.Resolved.APIKey,
		"anthropic-version": "2023-06-01",
	}
	if c.Resolved.OAuth != nil && c.Resolved.OAuth.Access != "" {
		delete(h, "x-api-key")
		h["Authorization"] = "Bearer " + c.Resolved.OAuth.Access
	}
	for k, v := range c.Resolved.Headers {
		h[k] = v
	}
	return h
}

type antBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Signature    string          `json:"signature,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      any             `json:"content,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	Source       *antSource      `json:"source,omitempty"`
	CacheControl map[string]any  `json:"cache_control,omitempty"`
}

type antSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type antMsg struct {
	Role    string     `json:"role"`
	Content []antBlock `json:"content"`
}

func (c *Client) streamAnthropic(ctx context.Context, req Request) (<-chan StreamEvent, error) {
	var messages []antMsg
	for _, m := range req.Messages {
		var blocks []antBlock
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				blocks = append(blocks, antBlock{Type: "text", Text: b.Text})
			case "thinking":
				if b.Signature != "" {
					blocks = append(blocks, antBlock{Type: "thinking", Thinking: b.Thinking, Signature: b.Signature})
				}
			case "tool_use":
				blocks = append(blocks, antBlock{Type: "tool_use", ID: b.ID, Name: b.Name, Input: rawOrEmpty(b.Input)})
			case "tool_result":
				blocks = append(blocks, antBlock{Type: "tool_result", ToolUseID: b.ID, Content: []antBlock{{Type: "text", Text: b.Output}}, IsError: b.IsError})
			case "image":
				blocks = append(blocks, antBlock{Type: "image", Source: &antSource{Type: "base64", MediaType: b.MediaType, Data: b.Data}})
			}
		}
		if len(blocks) == 0 {
			continue
		}
		// tool_result blocks must be in a user message
		role := m.Role
		allTR := true
		for _, b := range m.Content {
			if b.Type != "tool_result" {
				allTR = false
			}
		}
		if allTR {
			role = "user"
		}
		if len(messages) > 0 && messages[len(messages)-1].Role == role {
			messages[len(messages)-1].Content = append(messages[len(messages)-1].Content, blocks...)
		} else {
			messages = append(messages, antMsg{Role: role, Content: blocks})
		}
	}
	if len(messages) == 0 {
		messages = []antMsg{{Role: "user", Content: []antBlock{{Type: "text", Text: "(empty)"}}}}
	}
	tools := []map[string]any{}
	for _, t := range req.Tools {
		tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": rawOrEmpty(t.Parameters)})
	}
	body := map[string]any{
		"model":      req.Model,
		"max_tokens": maxInt(req.MaxTokens, 8192),
		"messages":   messages,
		"stream":     true,
	}
	if req.System != "" {
		body["system"] = []antBlock{{Type: "text", Text: req.System, CacheControl: map[string]any{"type": "ephemeral"}}}
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	} else if !strings.Contains(req.Model, "-thinking") && !req.Reasoning {
		body["temperature"] = 0
	}
	if req.Reasoning {
		budget := maxInt(req.MaxTokens/2, 1024)
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
		delete(body, "temperature")
	}

	out := make(chan StreamEvent, 256)
	go func() {
		defer close(out)
		type blkState struct {
			kind string
			id   string
			name string
			args strings.Builder
			sig  string
		}
		blocks := map[int]*blkState{}
		var usage Usage
		var finish string
		err := postSSE(ctx, c.Resolved.BaseURL+"/messages", c.anthropicHeaders(), body, c.streamIdle(), func(event string, data []byte) (bool, error) {
			var v map[string]json.RawMessage
			if err := json.Unmarshal(data, &v); err != nil {
				return true, nil
			}
			typ := event
			if t, ok := v["type"]; ok {
				_ = json.Unmarshal(t, &typ)
			}
			switch typ {
			case "message_start":
				var ms struct {
					Message struct {
						Usage struct {
							InputTokens              float64 `json:"input_tokens"`
							OutputTokens             float64 `json:"output_tokens"`
							CacheReadInputTokens     float64 `json:"cache_read_input_tokens"`
							CacheCreationInputTokens float64 `json:"cache_creation_input_tokens"`
						} `json:"usage"`
					} `json:"message"`
				}
				_ = json.Unmarshal(data, &ms)
				usage.Input = ms.Message.Usage.InputTokens
				usage.CacheRead = ms.Message.Usage.CacheReadInputTokens
				usage.CacheWrite = ms.Message.Usage.CacheCreationInputTokens
				usage.Output = ms.Message.Usage.OutputTokens
			case "content_block_start":
				var cb struct {
					Index        int `json:"index"`
					ContentBlock struct {
						Type string `json:"type"`
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"content_block"`
				}
				_ = json.Unmarshal(data, &cb)
				st := &blkState{kind: cb.ContentBlock.Type, id: cb.ContentBlock.ID, name: cb.ContentBlock.Name}
				blocks[cb.Index] = st
				switch st.kind {
				case "text":
					out <- StreamEvent{Type: "text-start", ID: itoa(cb.Index)}
				case "thinking", "redacted_thinking":
					out <- StreamEvent{Type: "reasoning-start", ID: "r" + itoa(cb.Index)}
				}
			case "content_block_delta":
				var d struct {
					Index int `json:"index"`
					Delta struct {
						Type        string `json:"type"`
						Text        string `json:"text"`
						Thinking    string `json:"thinking"`
						Signature   string `json:"signature"`
						PartialJSON string `json:"partial_json"`
					} `json:"delta"`
				}
				_ = json.Unmarshal(data, &d)
				st := blocks[d.Index]
				switch d.Delta.Type {
				case "text_delta":
					out <- StreamEvent{Type: "text-delta", ID: itoa(d.Index), Text: d.Delta.Text}
				case "thinking_delta":
					out <- StreamEvent{Type: "reasoning-delta", ID: "r" + itoa(d.Index), Text: d.Delta.Thinking}
				case "signature_delta":
					if st != nil {
						st.sig += d.Delta.Signature
					}
				case "input_json_delta":
					if st != nil {
						st.args.WriteString(d.Delta.PartialJSON)
					}
				}
			case "content_block_stop":
				var s struct {
					Index int `json:"index"`
				}
				_ = json.Unmarshal(data, &s)
				st := blocks[s.Index]
				if st == nil {
					return true, nil
				}
				switch st.kind {
				case "text":
					out <- StreamEvent{Type: "text-end", ID: itoa(s.Index)}
				case "thinking", "redacted_thinking":
					out <- StreamEvent{Type: "reasoning-end", ID: "r" + itoa(s.Index), Text: st.sig}
				case "tool_use":
					args := strings.TrimSpace(st.args.String())
					if args == "" {
						args = "{}"
					}
					out <- StreamEvent{Type: "tool-call", ID: st.id, Name: st.name, Input: json.RawMessage(args)}
				}
			case "message_delta":
				var d struct {
					Delta struct {
						StopReason string `json:"stop_reason"`
					} `json:"delta"`
					Usage struct {
						OutputTokens float64 `json:"output_tokens"`
					} `json:"usage"`
				}
				_ = json.Unmarshal(data, &d)
				if d.Delta.StopReason != "" {
					finish = d.Delta.StopReason
				}
				if d.Usage.OutputTokens > 0 {
					usage.Output = d.Usage.OutputTokens
				}
			case "message_stop":
				u := usage
				out <- StreamEvent{Type: "finish", Finish: mapAntFinish(finish), Usage: &u}
			case "error":
				var e struct {
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}
				_ = json.Unmarshal(data, &e)
				out <- StreamEvent{Type: "error", Err: &StreamError{Message: e.Error.Message, IsRetryable: e.Error.Type == "overloaded_error" || e.Error.Type == "api_error"}}
			}
			return true, nil
		})
		if err != nil && ctx.Err() == nil {
			out <- StreamEvent{Type: "error", Err: toStreamError(err)}
		}
	}()
	return out, nil
}

func mapAntFinish(f string) string {
	switch f {
	case "tool_use":
		return "tool-calls"
	case "max_tokens":
		return "length"
	case "end_turn", "stop_sequence":
		return "stop"
	}
	if f == "" {
		return ""
	}
	return f
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func rawOrEmpty(r json.RawMessage) json.RawMessage {
	if len(r) == 0 {
		return json.RawMessage(`{}`)
	}
	return r
}
