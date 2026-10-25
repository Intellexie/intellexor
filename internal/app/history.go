package app

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/Intellexie/intellexor/internal/provider"
	"github.com/Intellexie/intellexor/internal/session"
)

// BuildHistory converts stored messages/parts into provider wire messages.
// History replays from the newest summary assistant message onward
// (filterCompacted): everything a compaction summary covers is represented by
// the summary itself.
func BuildHistory(i *Instance, s *session.Session) []provider.Msg {
	msgs, err := i.Mgr.LoadMessages(s)
	if err != nil {
		return nil
	}
	return historyFrom(filterCompacted(msgs))
}

// historyFrom converts an already-loaded message slice into wire messages.
func historyFrom(msgs []session.WithParts) []provider.Msg {
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
			// A failed assistant turn with no tool results (mid-stream
			// connection reset, truncated SSE) must not be replayed: the next
			// request has to end with the last user message or tool output so
			// the model redoes the turn, not with a half-dead assistant
			// message it would try to continue. Turns that did complete tool
			// calls keep their tool_use/tool_result pairs — only the dangling
			// text of a tool-less failure is dropped.
			if info.Error != nil && info.Error.Name == "APIError" && !hasToolResult(m.Parts) {
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
						output := p.State.Output
						if p.State.Time != nil && p.State.Time.Compacted != nil {
							// output cleared by a prune pass (opencode)
							output = prunedToolOutput
						}
						blocks = append(blocks, provider.ContentBlock{Type: "tool_use", ID: p.CallID, Name: p.Tool, Input: mustMarshal(p.State.Input)})
						results = append(results, provider.ContentBlock{Type: "tool_result", ID: p.CallID, Name: p.Tool, Output: output})
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

// hasToolResult reports whether any tool part of the message reached a
// terminal state, meaning the message carries valid tool_use/tool_result
// pairs worth replaying even though the turn itself errored afterwards.
func hasToolResult(parts []session.Part) bool {
	for _, p := range parts {
		if p.Type == "tool" && p.State != nil &&
			(p.State.Status == "completed" || p.State.Status == "error") {
			return true
		}
	}
	return false
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
