package tool

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Intellexie/intellexor/internal/question"
	"github.com/Intellexie/intellexor/internal/session"
)

type QuestionTool struct {
	Svc *question.Service
}

func (QuestionTool) Name() string { return "question" }

func (QuestionTool) Description() string {
	return `Use this tool when you need to ask the user questions during execution. This allows you to:
1. Gather user preferences or requirements
2. Clarify ambiguous instructions
3. Get decisions on implementation choices as you work
4. Offer choices to the user about what direction to take.

Usage notes:
- Users will always be able to enter a custom answer, even when no options are listed
- Use multiSelect: true to allow multiple answers to be selected for a question
- If you recommend a specific option, make that the first option in the list and add "(Recommended)" at the end of the label`
}

func (QuestionTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"questions": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question": map[string]any{"type": "string", "description": "Complete question"},
						"header":   map[string]any{"type": "string", "description": "Very short label (max 30 chars)"},
						"options": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"label":       map[string]any{"type": "string", "description": "Display text (1-5 words, concise)"},
									"description": map[string]any{"type": "string", "description": "Explanation of choice"},
								},
								"required": []any{"label", "description"},
							},
							"description": "Available choices",
						},
						"multiple": map[string]any{"type": "boolean", "description": "Allow selecting multiple choices"},
					},
					"required": []any{"question", "header", "options"},
				},
				"description": "Questions to ask",
			},
		},
		"required": []any{"questions"},
	}
}

func (q QuestionTool) Run(c *Ctx, args json.RawMessage) (Result, error) {
	var p struct {
		Questions []struct {
			Question string `json:"question"`
			Header   string `json:"header"`
			Options  []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
			Multiple bool `json:"multiple"`
		} `json:"questions"`
	}
	_ = json.Unmarshal(args, &p)
	if len(p.Questions) == 0 {
		return Result{}, errors.New("questions are required")
	}
	var qs []question.Q
	for _, qq := range p.Questions {
		var opts []question.Option
		for _, o := range qq.Options {
			opts = append(opts, question.Option{Label: o.Label, Description: o.Description})
		}
		qs = append(qs, question.Q{Question: qq.Question, Header: qq.Header, Options: opts, Multiple: qq.Multiple})
	}
	toolRef := &session.ToolCallRef{MessageID: c.MessageID, CallID: c.CallID}
	answers, err := q.Svc.Ask(c.Ctx, c.SessionID, qs, toolRef)
	if err != nil {
		return Result{}, err
	}
	var parts []string
	for i, qq := range p.Questions {
		ans := "Unanswered"
		if i < len(answers) && len(answers[i]) > 0 {
			ans = strings.Join(answers[i], ", ")
		}
		parts = append(parts, fmt.Sprintf("\"%s\"=\"%s\"", qq.Question, ans))
	}
	out := fmt.Sprintf("User has answered your questions: %s. You can now continue with the user's answers in mind.", strings.Join(parts, ", "))
	flat := answers
	return Result{
		Title:    fmt.Sprintf("%d questions", len(p.Questions)),
		Metadata: map[string]any{"answers": flat},
		Output:   out,
	}, nil
}
