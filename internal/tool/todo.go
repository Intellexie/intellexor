package tool

import (
	"encoding/json"
	"fmt"
)

type TodoTool struct {
	Set func(sessionID string, todos []map[string]any)
}

func (TodoTool) Name() string { return "todowrite" }

func (TodoTool) Description() string {
	return `Create and maintain a structured task list for the current coding session. Tracks progress, organizes multi-step work, and surfaces status to the user.

## When to use
Use proactively when:
- The task requires 3+ distinct steps or actions
- The work is non-trivial and benefits from planning
- The user provides multiple tasks (numbered or comma-separated) or explicitly asks for a todo list
- New instructions arrive - capture them as todos
- You start a task - mark it 'in_progress' (only one at a time) before working
- You finish a task - mark it 'completed' and add any follow-ups discovered during the work

## States
- 'pending' - not started
- 'in_progress' - actively working (exactly ONE at a time)
- 'completed' - finished successfully
- 'cancelled' - no longer needed

## Rules
- Update status in real time; don't batch completions
- Preserve user-provided commands verbatim (flags, args, order)
- Items should be specific and actionable; break large work into smaller steps`
}

func (TodoTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"todos": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content":  map[string]any{"type": "string", "description": "Brief description of the task"},
						"status":   map[string]any{"type": "string", "description": "Current status of the task: pending, in_progress, completed, cancelled"},
						"priority": map[string]any{"type": "string", "description": "Priority level of the task: high, medium, low"},
					},
					"required": []any{"content", "status", "priority"},
				},
				"description": "The updated todo list",
			},
		},
		"required": []any{"todos"},
	}
}

func (t TodoTool) Run(c *Ctx, args json.RawMessage) (Result, error) {
	var p struct {
		Todos []map[string]any `json:"todos"`
	}
	_ = json.Unmarshal(args, &p)
	if c.Ask != nil {
		if err := c.Ask(AskInput{Permission: "todowrite", Patterns: []string{"*"}, Always: []string{"*"}, Metadata: map[string]any{}}); err != nil {
			return Result{}, err
		}
	}
	if t.Set != nil {
		t.Set(c.SessionID, p.Todos)
	}
	out, _ := json.MarshalIndent(p.Todos, "", "  ")
	remaining := 0
	for _, td := range p.Todos {
		if s, _ := td["status"].(string); s != "completed" {
			remaining++
		}
	}
	return Result{
		Title:    fmt.Sprintf("%d todos", remaining),
		Metadata: map[string]any{"todos": p.Todos},
		Output:   string(out),
	}, nil
}
