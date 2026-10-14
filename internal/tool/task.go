package tool

import (
	"encoding/json"
	"errors"
	"strings"
)

type TaskTool struct {
	Agents []AgentInfo // non-primary agents for description
}

type AgentInfo struct {
	Name        string
	Description string
}

func (TaskTool) Name() string { return "task" }

func (t TaskTool) Description() string {
	base := `Launch a new agent to handle complex, multistep tasks autonomously.

When using the Task tool, you must specify a subagent_type parameter to select which agent type to use.

When NOT to use the Task tool:
- If you want to read a specific file path, use the Read or Glob tool instead of the Task tool, to find the match more quickly
- If you are searching for a specific class definition like "class Foo", use the Grep tool instead, to find the match more quickly
- If you are searching for code within a specific file or set of 2-3 files, use the Read tool instead of the Task tool, to find the match more quickly
- If no available agent is a good fit for the task, use other tools directly

Usage notes:
1. Launch multiple agents concurrently whenever possible, to maximize performance; to do that, use a single message with multiple tool uses
2. Once you have delegated work to an agent, do not duplicate that work yourself. Continue with non-overlapping tasks, or wait for the result.
3. When the agent is done, it will return a single message back to you. The result returned by the agent is not visible to the user. To show the user the result, you should send a text message back to the user with a concise summary of the result. The output includes a task_id you can reuse later to continue the same subagent session.
4. Each agent invocation starts with a fresh context unless you provide task_id to resume the same subagent session (which continues with its previous messages and tool outputs). When starting fresh, your prompt should contain a highly detailed task description for the agent to perform autonomously and you should specify exactly what information the agent should return back to you in its final and only message to you.
5. The agent's outputs should generally be trusted
6. Clearly tell the agent whether you expect it to write code or just to do research (search, file reads, web fetches, etc.), since it is not aware of the user's intent. Tell it how to verify its work if possible (e.g., relevant test commands).
7. If the agent description mentions that it should be used proactively, then you should try your best to use it without the user having to ask for it first. Use your judgement.`
	if len(t.Agents) > 0 {
		var sb strings.Builder
		sb.WriteString("\n\nAvailable agent types and the tools they have access to:")
		for _, a := range t.Agents {
			sb.WriteString("\n- " + a.Name + ": " + a.Description)
		}
		sb.WriteString("\n")
		base += sb.String()
	}
	return base
}

func (TaskTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"description":   map[string]any{"type": "string", "description": "A short (3-5 words) description of the task"},
			"prompt":        map[string]any{"type": "string", "description": "The task for the agent to perform"},
			"subagent_type": map[string]any{"type": "string", "description": "The type of specialized agent to use for this task"},
			"task_id":       map[string]any{"type": "string", "description": "This should only be set if you mean to resume a previous task (you can pass a prior task_id and the task will continue the same subagent session as before instead of creating a fresh one)"},
		},
		"required": []any{"description", "prompt", "subagent_type"},
	}
}

func (t TaskTool) Run(c *Ctx, args json.RawMessage) (Result, error) {
	var p struct {
		Description  string `json:"description"`
		Prompt       string `json:"prompt"`
		SubagentType string `json:"subagent_type"`
		TaskID       string `json:"task_id"`
	}
	_ = json.Unmarshal(args, &p)
	if p.Prompt == "" || p.SubagentType == "" {
		return Result{}, errors.New("prompt and subagent_type are required")
	}
	if c.Subagent == nil {
		return Result{}, errors.New("task tool unavailable")
	}
	if c.Ask != nil {
		if err := c.Ask(AskInput{Permission: "task", Patterns: []string{p.SubagentType}, Always: []string{"*"}, Metadata: map[string]any{"description": p.Description, "subagent_type": p.SubagentType}}); err != nil {
			return Result{}, err
		}
	}
	text, childID, err := c.Subagent(SubagentInput{
		Description:   p.Description,
		Prompt:        p.Prompt,
		SubagentType:  p.SubagentType,
		TaskID:        p.TaskID,
		ParentSession: c.SessionID,
		OnChild: func(id string) {
			if c.Meta != nil {
				c.Meta("", map[string]any{"sessionId": id, "parentSessionId": c.SessionID})
			}
		},
	})
	md := map[string]any{"sessionId": childID, "parentSessionId": c.SessionID}
	if err != nil {
		if childID != "" {
			return Result{Title: p.Description, Metadata: md, Output: ""}, err
		}
		return Result{}, err
	}
	var out strings.Builder
	out.WriteString("<task id=\"" + childID + "\" state=\"completed\">\n")
	out.WriteString("<task_result>\n")
	out.WriteString(strings.TrimSpace(text))
	out.WriteString("\n</task_result>\n</task>")
	return Result{Title: p.Description, Metadata: md, Output: out.String()}, nil
}
