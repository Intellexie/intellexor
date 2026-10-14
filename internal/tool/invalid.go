package tool

import (
	"encoding/json"
	"fmt"
)

type InvalidTool struct{}

func (InvalidTool) Name() string { return "invalid" }

func (InvalidTool) Description() string { return "Do not use" }

func (InvalidTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tool":  map[string]any{"type": "string", "description": "The tool name that was invalid"},
			"error": map[string]any{"type": "string", "description": "The validation error"},
		},
		"required": []any{"tool", "error"},
	}
}

func (InvalidTool) Run(c *Ctx, args json.RawMessage) (Result, error) {
	var p struct {
		Tool  string `json:"tool"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(args, &p)
	return Result{
		Title:    "Invalid Tool",
		Metadata: map[string]any{},
		Output:   fmt.Sprintf("The arguments provided to the tool are invalid: %s", p.Error),
	}, nil
}
