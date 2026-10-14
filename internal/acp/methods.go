package acp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"intellexor/internal/app"
	"intellexor/internal/session"
	"intellexor/internal/store"
	"intellexor/internal/util"
)

var appVersion = "1.15.0"

// agent methods (client -> agent requests)

func initializeResult() map[string]any {
	return map[string]any{
		"protocolVersion": 1,
		"agentCapabilities": map[string]any{
			"loadSession": true,
			"promptCapabilities": map[string]any{
				"embeddedContext": true,
				"image":           true,
			},
		},
		"authMethods": []map[string]any{{
			"id":          "intellexor-auth",
			"name":        "Intellexor",
			"description": "Configure credentials in ~/.config/intellexor or opencode auth.json",
		}},
		"agentInfo": map[string]any{"name": "Intellexor", "version": appVersion},
	}
}

func (c *conn) request(method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return initializeResult(), nil
	case "authenticate":
		return nil, nil
	case "session/new":
		return c.newSession(params)
	case "session/load", "session/resume":
		return c.loadSession(params)
	case "session/list":
		return c.listSessions()
	case "session/set_model", "session/set_mode":
		return nil, nil
	case "session/prompt":
		return c.prompt(params)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + method}
	}
}

var acpModes = map[string]any{
	"current":   "build",
	"available": []map[string]string{{"id": "build", "name": "Build"}, {"id": "plan", "name": "Plan"}},
}

func (c *conn) newSession(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: -32602, Message: err.Error()}
	}
	cwd := p.Cwd
	if cwd == "" {
		cwd = c.cwd
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	inst := c.srv.Instance(cwd)
	sess := &session.Session{
		ID:        util.ID("ses"),
		ProjectID: store.ProjectID(inst.Directory),
		Directory: inst.Directory,
		Title:     "New session - " + time.Now().Format("2006-01-02 15:04"),
		Version:   appVersion,
	}
	if err := inst.Mgr.Create(sess); err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}
	c.smu.Lock()
	c.sessions[sess.ID] = sess
	c.smu.Unlock()
	return map[string]any{"sessionId": sess.ID, "modes": acpModes}, nil
}

func (c *conn) loadSession(params json.RawMessage) (any, *rpcError) {
	var p struct {
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: -32602, Message: err.Error()}
	}
	sess, err := c.srv.FindSession(p.SessionID)
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}
	c.smu.Lock()
	c.sessions[sess.ID] = sess
	c.smu.Unlock()

	inst := c.srv.Instance(sess.Directory)
	msgs, _ := inst.Mgr.LoadMessages(sess)
	for _, m := range msgs {
		info := msgInfo(m.Info)
		if info.Role != "user" && info.Role != "assistant" {
			continue
		}
		for _, part := range m.Parts {
			if part.Type != "text" && part.Type != "reasoning" {
				continue
			}
			update := "agent_message_chunk"
			if info.Role == "user" {
				update = "user_message_chunk"
			} else if part.Type == "reasoning" {
				update = "agent_thought_chunk"
			}
			c.Notify("session/update", map[string]any{
				"sessionId": sess.ID,
				"update": map[string]any{
					"sessionUpdate": update,
					"messageId":     info.ID,
					"content":       map[string]any{"type": "text", "text": part.Text},
				},
			})
		}
	}
	return map[string]any{"sessionId": sess.ID, "modes": acpModes}, nil
}

func (c *conn) listSessions() (any, *rpcError) {
	out := []map[string]any{}
	seen := map[string]bool{}
	for _, dir := range append([]string{c.srv.DefaultDir}, c.srv.InstanceDirs()...) {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		inst := c.srv.Instance(dir)
		for _, sess := range inst.Mgr.List(dir, true, store.ProjectID(dir)) {
			out = append(out, map[string]any{
				"sessionId": sess.ID,
				"cwd":       sess.Directory,
				"updatedAt": sess.Time.Updated,
			})
		}
	}
	return map[string]any{"sessions": out}, nil
}

func (c *conn) prompt(params json.RawMessage) (any, *rpcError) {
	var p struct {
		SessionID string         `json:"sessionId"`
		Prompt    []contentBlock `json:"prompt"`
		MessageID string         `json:"messageId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: -32602, Message: err.Error()}
	}
	sess, ok := c.sessionByID(p.SessionID)
	if !ok {
		return nil, &rpcError{Code: -32000, Message: fmt.Sprintf("session %q not found", p.SessionID)}
	}
	inst := c.srv.Instance(sess.Directory)

	ch := inst.Hub.Subscribe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range ch {
			if c.isClosed() {
				return
			}
			if sid, _ := ev.Properties["sessionID"].(string); sid != "" && sid != sess.ID {
				continue
			}
			c.handleEvent(ev)
		}
	}()

	in := app.PromptInput{MessageID: p.MessageID, Parts: acpPartsToParts(p.Prompt)}
	wp, err := inst.Prompt(sess, in)
	inst.Hub.Unsubscribe(ch)
	<-done

	if err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}
	info := assistantInfo(wp.Info)
	if info.Error != nil && info.Error.Name != "MessageAbortedError" && info.Error.Name != "MessageOutputLengthError" && info.Error.Name != "ContentFilterError" {
		msg := "agent failure"
		if m, ok := info.Error.Data["message"].(string); ok && m != "" {
			msg = m
		}
		return nil, &rpcError{Code: -32000, Message: info.Error.Name + ": " + msg, Data: info.Error.Data}
	}
	resp := map[string]any{"stopReason": stopReason(info.Error)}
	resp["usage"] = usageOf(info)
	return resp, nil
}

// stopReason mirrors opencode promptResponse error mapping.
func stopReason(err *session.NamedError) string {
	if err == nil {
		return "end_turn"
	}
	switch err.Name {
	case "MessageAbortedError":
		return "cancelled"
	case "MessageOutputLengthError":
		return "max_tokens"
	case "ContentFilterError":
		return "refusal"
	default:
		return "end_turn"
	}
}

func usageOf(info session.AssistantMessage) map[string]any {
	return map[string]any{
		"cost": info.Cost,
		"tokens": map[string]any{
			"input":      info.Tokens.Input,
			"output":     info.Tokens.Output,
			"reasoning":  info.Tokens.Reasoning,
			"cacheRead":  info.Tokens.Cache.Read,
			"cacheWrite": info.Tokens.Cache.Write,
		},
	}
}

// acpPartsToParts converts ACP content blocks to opencode prompt parts.
func acpPartsToParts(blocks []contentBlock) []json.RawMessage {
	var out []json.RawMessage
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, mustJSON(map[string]any{"type": "text", "text": b.Text}))
		case "image":
			out = append(out, mustJSON(map[string]any{
				"type": "file",
				"mime": b.MimeType,
				"url":  "data:" + b.MimeType + ";base64," + b.Data,
			}))
		case "resource":
			text := b.Text
			if text == "" {
				text = b.URI
			}
			out = append(out, mustJSON(map[string]any{"type": "text", "text": text}))
		case "resource_link":
			out = append(out, mustJSON(map[string]any{"type": "text", "text": b.URI}))
		}
	}
	return out
}

// msgInfo / assistantInfo: WithParts.Info is decoded as any — re-decode into concrete types.

func msgInfo(v any) session.UserMessage {
	b, _ := json.Marshal(v)
	var info session.UserMessage
	_ = json.Unmarshal(b, &info)
	return info
}

func assistantInfo(v any) session.AssistantMessage {
	b, _ := json.Marshal(v)
	var info session.AssistantMessage
	_ = json.Unmarshal(b, &info)
	return info
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
