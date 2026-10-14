package provider

import (
	"context"
	"encoding/json"
	"strings"
)

func (c *Client) streamGemini(ctx context.Context, req Request) (<-chan StreamEvent, error) {
	type part struct {
		Text       string `json:"text,omitempty"`
		InlineData *struct {
			MimeType string `json:"mimeType"`
			Data     string `json:"data"`
		} `json:"inlineData,omitempty"`
		FunctionCall *struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"functionCall,omitempty"`
		FunctionResponse *struct {
			Name     string          `json:"name"`
			Response json.RawMessage `json:"response"`
		} `json:"functionResponse,omitempty"`
	}
	type content struct {
		Role  string `json:"role"`
		Parts []part `json:"parts"`
	}
	var contents []content
	var cur *content
	role := ""
	start := func(r string) {
		if cur != nil && role == r {
			return
		}
		if cur != nil {
			contents = append(contents, *cur)
		}
		cur = &content{Role: r}
		role = r
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				start("user")
				cur.Parts = append(cur.Parts, part{Text: b.Text})
			case "image":
				start("user")
				cur.Parts = append(cur.Parts, part{InlineData: &struct {
					MimeType string `json:"mimeType"`
					Data     string `json:"data"`
				}{MimeType: b.MediaType, Data: b.Data}})
			case "tool_result":
				start("user")
				cur.Parts = append(cur.Parts, part{FunctionResponse: &struct {
					Name     string          `json:"name"`
					Response json.RawMessage `json:"response"`
				}{Name: b.Name, Response: json.RawMessage(`{"result":` + jsonString(b.Output) + `}`)}})
			case "tool_use":
				start("model")
				cur.Parts = append(cur.Parts, part{FunctionCall: &struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				}{Name: b.Name, Args: rawOrEmpty(b.Input)}})
			}
		}
	}
	if cur != nil {
		contents = append(contents, *cur)
	}

	tools := []map[string]any{}
	var declarations []map[string]any
	for _, t := range req.Tools {
		declarations = append(declarations, map[string]any{"name": t.Name, "description": t.Description, "parameters": geminiSchema(rawOrEmpty(t.Parameters))})
	}
	if len(declarations) > 0 {
		tools = append(tools, map[string]any{"functionDeclarations": declarations})
	}
	body := map[string]any{
		"contents": contents,
		"stream":   nil,
	}
	delete(body, "stream")
	if req.System != "" {
		body["systemInstruction"] = map[string]any{"parts": []part{{Text: req.System}}}
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	gen := map[string]any{}
	if req.MaxTokens > 0 {
		gen["maxOutputTokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		gen["temperature"] = *req.Temperature
	}
	if req.Reasoning {
		gen["thinkingConfig"] = map[string]any{"thinkingBudget": 4096}
	}
	if len(gen) > 0 {
		body["generationConfig"] = gen
	}

	url := strings.TrimRight(c.Resolved.BaseURL, "/") + "/models/" + req.Model + ":streamGenerateContent?alt=sse&key=" + c.Resolved.APIKey
	out := make(chan StreamEvent, 256)
	go func() {
		defer close(out)
		textOpen := false
		var usage Usage
		var finish string
		err := postSSE(ctx, url, map[string]string{}, body, func(event string, data []byte) (bool, error) {
			var chunk struct {
				Candidates []struct {
					Content struct {
						Parts []struct {
							Text         string `json:"text"`
							Thought      bool   `json:"thought"`
							FunctionCall *struct {
								Name string          `json:"name"`
								Args json.RawMessage `json:"args"`
							} `json:"functionCall"`
						} `json:"parts"`
					} `json:"content"`
					FinishReason string `json:"finishReason"`
				} `json:"candidates"`
				UsageMetadata struct {
					PromptTokenCount        float64 `json:"promptTokenCount"`
					CandidatesTokenCount    float64 `json:"candidatesTokenCount"`
					ThoughtsTokenCount      float64 `json:"thoughtsTokenCount"`
					CachedContentTokenCount float64 `json:"cachedContentTokenCount"`
				} `json:"usageMetadata"`
			}
			if err := json.Unmarshal(data, &chunk); err != nil {
				return true, nil
			}
			usage.Input = chunk.UsageMetadata.PromptTokenCount - chunk.UsageMetadata.CachedContentTokenCount
			usage.CacheRead = chunk.UsageMetadata.CachedContentTokenCount
			usage.Output = chunk.UsageMetadata.CandidatesTokenCount
			usage.Reasoning = chunk.UsageMetadata.ThoughtsTokenCount
			for _, cand := range chunk.Candidates {
				for _, p := range cand.Content.Parts {
					if p.Text != "" {
						if !textOpen {
							out <- StreamEvent{Type: "text-start", ID: "0"}
							textOpen = true
						}
						out <- StreamEvent{Type: "text-delta", ID: "0", Text: p.Text}
					}
					if p.FunctionCall != nil {
						if textOpen {
							out <- StreamEvent{Type: "text-end", ID: "0"}
							textOpen = false
						}
						out <- StreamEvent{Type: "tool-call", ID: "call_" + strings.ToLower(p.FunctionCall.Name), Name: p.FunctionCall.Name, Input: rawOrEmpty(p.FunctionCall.Args)}
					}
				}
				if cand.FinishReason != "" {
					finish = cand.FinishReason
				}
			}
			return true, nil
		})
		if textOpen {
			out <- StreamEvent{Type: "text-end", ID: "0"}
		}
		if err != nil && ctx.Err() == nil {
			out <- StreamEvent{Type: "error", Err: toStreamError(err)}
			return
		}
		u := usage
		f := finish
		switch finish {
		case "STOP":
			f = "stop"
		case "MAX_TOKENS":
			f = "length"
		case "TOOL_CALL", "MALFORMED_FUNCTION_CALL":
			f = "tool-calls"
		}
		out <- StreamEvent{Type: "finish", Finish: f, Usage: &u}
	}()
	return out, nil
}

// gemini needs sanitized json schema (no formats/refs)
func geminiSchema(raw json.RawMessage) any {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return map[string]any{"type": "object"}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{"type": "object"}
	}
	delete(m, "$schema")
	return m
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
