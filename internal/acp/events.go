package acp

import (
	"encoding/json"

	"github.com/Intellexie/intellexor/internal/bus"
)

// toToolKind mirrors opencode acp/tool.ts toToolKind.
func toToolKind(toolName string) string {
	switch toolName {
	case "bash", "shell":
		return "execute"
	case "webfetch":
		return "fetch"
	case "edit", "apply_patch", "patch", "write":
		return "edit"
	case "grep", "glob", "context", "invalid":
		return "search"
	case "read":
		return "read"
	case "task":
		return "think"
	default:
		return "other"
	}
}

var permissionOptions = []map[string]string{
	{"optionId": "once", "kind": "allow_once", "name": "Allow once"},
	{"optionId": "always", "kind": "allow_always", "name": "Always allow"},
	{"optionId": "reject", "kind": "reject_once", "name": "Reject"},
}

func (c *conn) handleEvent(ev bus.Event) {
	switch ev.Type {
	case "message.part.delta":
		field, _ := ev.Properties["field"].(string)
		if field != "text" && field != "reasoning" {
			return
		}
		text, _ := ev.Properties["delta"].(string)
		if text == "" {
			return
		}
		update := "agent_message_chunk"
		if field == "reasoning" {
			update = "agent_thought_chunk"
		}
		c.Notify("session/update", map[string]any{
			"sessionId": ev.Properties["sessionID"],
			"update": map[string]any{
				"sessionUpdate": update,
				"messageId":     ev.Properties["messageID"],
				"content":       map[string]any{"type": "text", "text": text},
			},
		})

	case "message.part.updated":
		part := anyMap(ev.Properties["part"])
		if part == nil || part["type"] != "tool" {
			return
		}
		callID, _ := part["callID"].(string)
		tool, _ := part["tool"].(string)
		state := anyMap(part["state"])
		status := ""
		if state != nil {
			status, _ = state["status"].(string)
		}
		if callID == "" {
			return
		}
		switch status {
		case "pending":
			c.Notify("session/update", map[string]any{
				"sessionId": ev.Properties["sessionID"],
				"update": map[string]any{
					"sessionUpdate": "tool_call",
					"toolCallId":    callID,
					"title":         toolTitle(tool, state),
					"kind":          toToolKind(tool),
					"status":        "pending",
					"rawInput":      state["input"],
				},
			})
		case "running":
			upd := map[string]any{
				"sessionUpdate": "tool_call_update",
				"toolCallId":    callID,
				"kind":          toToolKind(tool),
				"status":        "in_progress",
				"title":         toolTitle(tool, state),
				"rawInput":      state["input"],
			}
			if out := bashPreview(state); out != "" {
				upd["content"] = []map[string]any{{"type": "content", "content": map[string]any{"type": "text", "text": out}}}
			}
			c.Notify("session/update", map[string]any{"sessionId": ev.Properties["sessionID"], "update": upd})
		case "completed":
			c.Notify("session/update", map[string]any{
				"sessionId": ev.Properties["sessionID"],
				"update": map[string]any{
					"sessionUpdate": "tool_call_update",
					"toolCallId":    callID,
					"kind":          toToolKind(tool),
					"status":        "completed",
					"content":       []map[string]any{{"type": "content", "content": map[string]any{"type": "text", "text": outputText(state)}}},
					"rawOutput":     state["output"],
				},
			})
		case "error":
			c.Notify("session/update", map[string]any{
				"sessionId": ev.Properties["sessionID"],
				"update": map[string]any{
					"sessionUpdate": "tool_call_update",
					"toolCallId":    callID,
					"kind":          toToolKind(tool),
					"status":        "failed",
					"content":       []map[string]any{{"type": "content", "content": map[string]any{"type": "text", "text": outputText(state)}}},
				},
			})
		}

	case "permission.asked":
		sid, _ := ev.Properties["sessionID"].(string)
		if _, ok := c.sessions[sid]; !ok {
			return
		}
		go c.requestPermission(ev)

	case "session.error", "permission.replied", "question.rejected", "question.replied":
		// no ACP surface; prompt response carries errors
	}
}

func (c *conn) requestPermission(ev bus.Event) {
	permID, _ := ev.Properties["id"].(string)
	if permID == "" {
		return
	}
	callID := permID
	if tool := anyMap(ev.Properties["tool"]); tool != nil {
		if id, _ := tool["callID"].(string); id != "" {
			callID = id
		}
	}
	toolCall := map[string]any{
		"toolCallId": callID,
		"kind":       toToolKind(stringField(ev.Properties["permission"])),
	}
	if md, ok := ev.Properties["metadata"].(map[string]any); ok {
		toolCall["rawInput"] = md
	}
	result, err := c.Call("session/request_permission", map[string]any{
		"sessionId": ev.Properties["sessionID"],
		"toolCall":  toolCall,
		"options":   permissionOptions,
	})
	if err != nil {
		_ = c.srv.Instance(c.cwd).Perms.Reply(permID, "reject", "connection error")
		return
	}
	var resp struct {
		OptionID string `json:"optionId"`
	}
	_ = json.Unmarshal(result, &resp)
	reply := "reject"
	switch resp.OptionID {
	case "once", "allow_once":
		reply = "once"
	case "always", "allow_always":
		reply = "always"
	}
	_ = c.srv.Instance(c.cwd).Perms.Reply(permID, reply, "")
}

// toolTitle: opencode uses humanize tool input; keep it simple and stable.
func toolTitle(tool string, state map[string]any) string {
	if t, _ := state["title"].(string); t != "" {
		return t
	}
	if input, ok := state["input"].(map[string]any); ok {
		switch tool {
		case "bash":
			if c, _ := input["command"].(string); c != "" {
				return c
			}
		case "read", "edit", "write":
			if p, _ := input["filePath"].(string); p != "" {
				return p
			}
		case "glob":
			if p, _ := input["pattern"].(string); p != "" {
				return p
			}
		case "grep":
			if p, _ := input["pattern"].(string); p != "" {
				return p
			}
		}
	}
	return tool
}

func outputText(state map[string]any) string {
	if out, _ := state["output"].(string); out != "" {
		return out
	}
	if md, ok := state["metadata"].(map[string]any); ok {
		if out, _ := md["output"].(string); out != "" {
			return out
		}
	}
	return ""
}

func bashPreview(state map[string]any) string {
	if md, ok := state["metadata"].(map[string]any); ok {
		if out, _ := md["output"].(string); out != "" {
			return out
		}
	}
	return ""
}

// anyMap normalizes a hub property (may be a struct) into map[string]any.
func anyMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

func stringField(v any) string {
	s, _ := v.(string)
	return s
}
