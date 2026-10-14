package app

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/Intellexie/intellexor/internal/provider"
	"github.com/Intellexie/intellexor/internal/session"
)

// BuildHistory converts stored messages/parts into provider wire messages.
func BuildHistory(i *Instance, s *session.Session) []provider.Msg {
	msgs, err := i.Mgr.LoadMessages(s)
	if err != nil {
		return nil
	}
	var out []provider.Msg
	for _, m := range msgs {
		switch info := m.Info.(type) {
		case session.UserMessage:
			var blocks []provider.ContentBlock
			for _, p := range m.Parts {
				switch p.Type {
				case "text":
					if p.Ignored {
						continue
					}
					if strings.TrimSpace(p.Text) == "" {
						continue
					}
					blocks = append(blocks, provider.ContentBlock{Type: "text", Text: p.Text})
				case "file":
					blocks = append(blocks, fileBlock(p)...)
				}
			}
			if info.System != "" {
				blocks = append([]provider.ContentBlock{{Type: "text", Text: info.System}}, blocks...)
			}
			if len(blocks) > 0 {
				out = append(out, provider.Msg{Role: "user", Content: blocks})
			}
		case session.AssistantMessage:
			if info.Error != nil && info.Error.Name == "MessageAbortedError" {
				continue
			}
			var blocks []provider.ContentBlock
			var results []provider.ContentBlock
			for _, p := range m.Parts {
				switch p.Type {
				case "text":
					if !p.Synthetic && strings.TrimSpace(p.Text) != "" && !p.Ignored {
						blocks = append(blocks, provider.ContentBlock{Type: "text", Text: p.Text})
					}
				case "tool":
					if p.State == nil {
						continue
					}
					switch p.State.Status {
					case "completed":
						blocks = append(blocks, provider.ContentBlock{Type: "tool_use", ID: p.CallID, Name: p.Tool, Input: mustMarshal(p.State.Input)})
						results = append(results, provider.ContentBlock{Type: "tool_result", ID: p.CallID, Name: p.Tool, Output: p.State.Output})
					case "error":
						blocks = append(blocks, provider.ContentBlock{Type: "tool_use", ID: p.CallID, Name: p.Tool, Input: mustMarshal(p.State.Input)})
						results = append(results, provider.ContentBlock{Type: "tool_result", ID: p.CallID, Name: p.Tool, Output: p.State.Error, IsError: true})
					}
				}
			}
			if len(blocks) > 0 {
				out = append(out, provider.Msg{Role: "assistant", Content: blocks})
			}
			if len(results) > 0 {
				out = append(out, provider.Msg{Role: "user", Content: results})
			}
		}
	}
	return out
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func fileBlock(p session.Part) []provider.ContentBlock {
	if p.Mime == "" || p.URL == "" {
		return nil
	}
	i := strings.Index(p.URL, ",")
	data := p.URL
	if i >= 0 {
		data = p.URL[i+1:]
	}
	if strings.HasPrefix(p.Mime, "image/") {
		return []provider.ContentBlock{{Type: "image", MediaType: p.Mime, Data: data}}
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err == nil && looksText(p.Mime) {
		return []provider.ContentBlock{{Type: "text", Text: "<attached_files name=\"" + p.Filename + "\">\n" + string(raw) + "\n</attached_files>"}}
	}
	return nil
}

func looksText(mime string) bool {
	return strings.HasPrefix(mime, "text/") || strings.Contains(mime, "json") || strings.Contains(mime, "javascript") || strings.Contains(mime, "xml")
}
