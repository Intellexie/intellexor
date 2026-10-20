package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Intellexie/intellexor/internal/provider"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

const (
	MaxLines = 2000
	MaxBytes = 50 * 1024
)

type AskInput struct {
	Permission string
	Patterns   []string
	Metadata   map[string]any
	Always     []string
}

type SubagentInput struct {
	Description   string
	Prompt        string
	SubagentType  string
	TaskID        string
	ParentSession string
	Directory     string
	Model         *session.ModelRef
	// OnChild is invoked as soon as the subagent session is created, so the
	// tool part can expose metadata.sessionId while still running.
	OnChild func(childID string)
}

type Result struct {
	Title       string
	Metadata    map[string]any
	Output      string
	Attachments []session.Part
}

type Ctx struct {
	Ctx       context.Context
	SessionID string
	MessageID string
	CallID    string
	Directory string
	Worktree  string
	Agent     string
	// Timezone is the device timezone of the user's web UI client (IANA
	// name, recorded on the session from the X-Timezone header). Tools that
	// report times should format them in this zone; empty means unknown
	// (e.g. ACP clients), in which case the server's local zone is the
	// best available approximation.
	Timezone string
	Config   any
	Depth    int

	Ask      func(AskInput) error
	Meta     func(title string, metadata map[string]any)
	Subagent func(SubagentInput) (string, string, error)
	// Backup records the pre-edit content of path before it is modified,
	// enabling revert-to-message to restore the workspace.
	Backup func(path string) error
}

type Tool interface {
	Name() string
	Description() string
	Schema() map[string]any
	Run(c *Ctx, args json.RawMessage) (Result, error)
}

type Registry struct {
	tools map[string]Tool
}

func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{tools: map[string]Tool{}}
	for _, t := range tools {
		r.tools[t.Name()] = t
	}
	return r
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) Defs(names []string) []provider.ToolDef {
	var out []provider.ToolDef
	for _, n := range names {
		t, ok := r.tools[n]
		if !ok {
			continue
		}
		params, _ := json.Marshal(t.Schema())
		out = append(out, provider.ToolDef{Name: t.Name(), Description: t.Description(), Parameters: params})
	}
	return out
}

// RunWithValidation decodes args; failures mirror opencode's InvalidArgumentsError
func (r *Registry) RunWithValidation(c *Ctx, name string, args json.RawMessage) (Result, error) {
	t, ok := r.tools[name]
	if !ok {
		// invalid tool path
		it, _ := r.tools["invalid"]
		return it.Run(c, mustJSON(map[string]any{"tool": name, "error": "Tool " + name + " does not exist"}))
	}
	var m map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &m); err != nil || m == nil {
			m = map[string]any{}
		}
	}
	if err := validate(name, t.Schema(), m); err != nil {
		it, _ := r.tools["invalid"]
		return it.Run(c, mustJSON(map[string]any{"tool": name, "error": err.Error()}))
	}
	res, err := t.Run(c, args)
	if err != nil {
		return res, err
	}
	res.Output = TruncateOutput(c, res.Output, res.Metadata)
	return res, nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// basic required/type validation from json schema
func validate(name string, schema map[string]any, args map[string]any) error {
	props, _ := schema["properties"].(map[string]any)
	req, _ := schema["required"].([]any)
	for _, r := range req {
		key, _ := r.(string)
		v, ok := args[key]
		if !ok || v == nil || v == "" {
			return fmt.Errorf("The %s tool was called with invalid arguments: missing required parameter '%s'.\nPlease rewrite the tool input to match the expected schema.", name, key)
		}
	}
	for k, v := range args {
		p, ok := props[k].(map[string]any)
		if !ok {
			continue
		}
		typ, _ := p["type"].(string)
		switch typ {
		case "string":
			if _, ok := v.(string); !ok && v != nil {
				return fmt.Errorf("The %s tool was called with invalid arguments: parameter '%s' must be a string.\nPlease rewrite the tool input to match the expected schema.", name, k)
			}
		case "number", "integer":
			switch v.(type) {
			case float64, int, nil:
			default:
				return fmt.Errorf("The %s tool was called with invalid arguments: parameter '%s' must be a number.\nPlease rewrite the tool input to match the expected schema.", name, k)
			}
		case "boolean":
			if _, ok := v.(bool); !ok && v != nil {
				return fmt.Errorf("The %s tool was called with invalid arguments: parameter '%s' must be a boolean.\nPlease rewrite the tool input to match the expected schema.", name, k)
			}
		}
	}
	return nil
}

// Head-direction truncation of tool output per opencode's generic wrapper
func TruncateOutput(c *Ctx, output string, metadata map[string]any) string {
	if metadata != nil {
		if t, ok := metadata["truncated"].(bool); ok && t {
			return output
		}
	}
	lines := strings.Split(output, "\n")
	if len(lines) <= MaxLines && len(output) <= MaxBytes {
		return output
	}
	kept := lines
	if len(kept) > MaxLines {
		kept = kept[:MaxLines]
	}
	joined := strings.Join(kept, "\n")
	removed := len(lines) - len(kept)
	hint := "Use more specific parameters, or Grep/Read on the saved file at "
	if c != nil && c.Subagent != nil {
		hint = "Delegate processing of the saved file to a Task/explore subagent, or use Grep/Read on the saved file at "
	}
	tmp := filepath.Join(store.DataDir(), "tool-output")
	_ = os.MkdirAll(tmp, 0o755)
	outFile := filepath.Join(tmp, "tool_"+util.ID("out"))
	_ = os.WriteFile(outFile, []byte(output), 0o644)
	truncated := joined + fmt.Sprintf("\n\n...%d lines truncated...\n\nFull output saved to: %s\n%s%s", removed, outFile, hint, outFile)
	if metadata != nil {
		metadata["truncated"] = true
		metadata["outputPath"] = outFile
	}
	return truncated
}
