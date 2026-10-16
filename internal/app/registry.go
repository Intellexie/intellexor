package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Intellexie/intellexor/internal/config"
	"github.com/Intellexie/intellexor/internal/provider"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/tool"
)

var (
	regOnce sync.Once
	regInst map[*Instance]*tool.Registry
	regMu   sync.Mutex
)

func registryFor(i *Instance) *tool.Registry {
	regMu.Lock()
	defer regMu.Unlock()
	if regInst == nil {
		regInst = map[*Instance]*tool.Registry{}
	}
	if r, ok := regInst[i]; ok {
		return r
	}
	todo := tool.TodoTool{Set: func(sessionID string, todos []map[string]any) {
		_ = i.Mgr.WriteKV("todo", sessionID, todos)
		i.Hub.Publish("todo.updated", map[string]any{"sessionID": sessionID, "todos": todos})
	}}
	boardTool := tool.BoardTool{OnUpdate: func() {
		// same broadcast the board HTTP routes send, so every connected
		// client refreshes when the agent moves a card
		i.Hub.Publish("board.updated", map[string]any{"directory": i.Directory})
	}}
	questionTool := tool.QuestionTool{Svc: i.Quest}
	r := tool.NewRegistry(
		tool.InvalidTool{},
		questionTool,
		tool.BashTool{},
		tool.ReadTool{},
		tool.GlobTool{},
		tool.GrepTool{},
		tool.EditTool{},
		tool.WriteTool{},
		tool.TaskTool{Agents: subagentInfos(i)},
		tool.WebFetchTool{},
		todo,
		tool.ApplyPatchTool{},
		boardTool,
	)
	regInst[i] = r
	return r
}

func subagentInfos(i *Instance) []tool.AgentInfo {
	var out []tool.AgentInfo
	for _, a := range i.Agents() {
		if a.Mode == "subagent" {
			out = append(out, tool.AgentInfo{Name: a.Name, Description: a.Description})
		}
	}
	return out
}

func activeToolNames(dir string, agent AgentDef) []string {
	names := []string{"invalid"}
	merged := config.Merge(dir)
	disabled := map[string]bool{}
	for _, d := range merged.DisabledTools {
		disabled[d] = true
	}
	applyPatch := false
	for _, n := range []string{"bash", "read", "glob", "grep", "edit", "write", "task", "webfetch", "todowrite", "question", "apply_patch", "board"} {
		if n == "apply_patch" {
			continue // only for gpt models
		}
		if disabled[n] {
			continue
		}
		names = append(names, n)
	}
	_ = applyPatch
	return names
}

func toolNamesForModel(modelID string, names []string) []string {
	// apply_patch for gpt-5 style models, drop edit/write (matches opencode)
	if strings.Contains(modelID, "gpt-") && !strings.Contains(modelID, "oss") && !strings.Contains(modelID, "gpt-4") {
		var out []string
		hasPatch := false
		for _, n := range names {
			if n == "edit" || n == "write" {
				continue
			}
			if n == "apply_patch" {
				hasPatch = true
			}
			out = append(out, n)
		}
		if !hasPatch {
			out = append(out, "apply_patch")
		}
		return out
	}
	return names
}

func instructionText(dir string) string {
	merged := config.Merge(dir)
	var blocks []string
	for _, path := range merged.Instructions {
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		b, err := os.ReadFile(path)
		if err == nil {
			blocks = append(blocks, "<instruction path=\""+filepath.Base(path)+"\">\n"+string(b)+"\n</instruction>")
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "AGENTS.md")); err == nil {
		blocks = append(blocks, "<instruction path=\"AGENTS.md\">\n"+string(b)+"\n</instruction>")
	}
	return strings.Join(blocks, "\n\n")
}

// ---- subagents ----

func (i *Instance) runSubagent(parentCtx context.Context, parent *session.Session, in tool.SubagentInput) (string, string, error) {
	agent := i.Agent(in.SubagentType)
	depth := parentDepth(i, parent)
	merged := config.Merge(i.Directory)
	maxDepth := 100
	if merged.SubagentDepth != nil {
		maxDepth = *merged.SubagentDepth
	}
	if depth > maxDepth {
		return "", "", fmt.Errorf("subagent depth limit reached")
	}

	child := &session.Session{
		ProjectID:  parent.ProjectID,
		Directory:  i.Directory,
		ParentID:   parent.ID,
		Title:      fmt.Sprintf("%s (@%s subagent)", in.Description, agent.Name),
		Agent:      agent.Name,
		Permission: append(parent.Permission, denyRules("todowrite"), denyRules("task")),
	}
	if in.Model != nil {
		child.Model = in.Model
	}
	if err := i.Mgr.Create(child); err != nil {
		return "", "", err
	}
	i.Hub.Publish("session.created", map[string]any{"sessionID": child.ID, "info": *child})
	if in.OnChild != nil {
		in.OnChild(child.ID)
	}

	// run synchronously with parent's ctx
	raw, _ := json.Marshal(map[string]any{"type": "text", "text": in.Prompt})
	_, err := i.PromptCtx(parentCtx, child, PromptInput{
		Agent: agent.Name,
		Parts: []json.RawMessage{raw},
	})
	if err != nil {
		return "", child.ID, err
	}
	text := lastAssistantText(i, child)
	return text, child.ID, nil
}

func denyRules(perm string) session.PermissionRule {
	return session.PermissionRule{Permission: perm, Pattern: "*", Action: "deny"}
}

func parentDepth(i *Instance, s *session.Session) int {
	d := 0
	cur := s
	for cur.ParentID != "" {
		p, err := i.Mgr.Find(cur.ParentID)
		if err != nil {
			break
		}
		d++
		cur = p
		if d > 20 {
			break
		}
	}
	return d + 1
}

func lastAssistantText(i *Instance, s *session.Session) string {
	msgs, _ := i.Mgr.LoadMessages(s)
	for j := len(msgs) - 1; j >= 0; j-- {
		if _, ok := msgs[j].Info.(session.AssistantMessage); !ok {
			continue
		}
		for k := len(msgs[j].Parts) - 1; k >= 0; k-- {
			p := msgs[j].Parts[k]
			if p.Type == "text" && !p.Synthetic && strings.TrimSpace(p.Text) != "" {
				return p.Text
			}
		}
	}
	return "Task completed"
}

// ---- title generation ----

const titleSystemPrompt = `You are a title generator. Your task is to generate a short, descriptive title for a coding session based on the user's request.

Rules:
- Use maximum 5-10 words
- Capture the main task or topic of the conversation
- Do not use quotes or special formatting
- Do not add explanations or additional text
- Respond with the title only`

// generateTitle sets a readable fallback title immediately, then asks the
// configured small model (config "small_model") for a proper title, falling
// back to the model used for the prompt itself. Only when both fail does the
// truncated first user message stay as the title.
func (i *Instance) generateTitle(s *session.Session, userParts []session.Part, resolved *provider.Resolved, model provider.Model) {
	text := ""
	for _, p := range userParts {
		if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
			text = strings.TrimSpace(p.Text)
			break
		}
	}
	if text == "" {
		return
	}
	fallback := shorten(text)
	set := func(t string) {
		s.Title = t
		_ = i.Mgr.Save(s)
		i.Hub.Publish("session.updated", map[string]any{"sessionID": s.ID, "info": *s})
	}
	// keep the sidebar readable while generation runs
	set(fallback)

	title := i.smallModelTitle(text, resolved, model)
	if title == "" {
		return
	}
	// re-read + write atomically so cost/token updates (and a possible manual
	// rename) made while generating are not clobbered by a stale save
	fresh, err := i.Mgr.Update(s.ID, func(u *session.Session) {
		if u.Title != fallback && !strings.HasPrefix(u.Title, "New session") {
			return // renamed by the user meanwhile
		}
		u.Title = title
	})
	if err != nil {
		return
	}
	if fresh.Title != title {
		return // renamed by the user meanwhile
	}
	i.Hub.Publish("session.updated", map[string]any{"sessionID": s.ID, "info": *fresh})
}

// smallModelTitle asks the small model for a session title. It prefers the
// "small_model" config entry ("provider/model"); when none is configured,
// unresolvable, or the request fails or comes back empty, it falls back to
// the model the prompt itself uses.
func (i *Instance) smallModelTitle(text string, resolved *provider.Resolved, model provider.Model) string {
	if sm := config.Merge(i.Directory).SmallModel; sm != "" {
		if pid, mid, ok := strings.Cut(sm, "/"); ok {
			r, m, err := provider.Resolve(i.Directory, pid, mid)
			if err != nil {
				// model not in the catalog; still honor the config if the provider exists
				if all, _ := provider.List(i.Directory); all[pid] != nil {
					r, m, err = all[pid], provider.Model{ID: mid}, nil
				}
			}
			if err == nil {
				if out := titleCompletion(text, r, m); out != "" {
					return out
				}
			}
		}
	}
	return titleCompletion(text, resolved, model)
}

// titleCompletion runs the title prompt against one model; "" on any failure.
func titleCompletion(text string, resolved *provider.Resolved, model provider.Model) string {
	if resolved == nil || model.ID == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := completeText(ctx, resolved, model, titleSystemPrompt,
		"Generate a title for this conversation:\n\n"+clip(text, 2000))
	if err != nil {
		return ""
	}
	return cleanTitle(out)
}

// completeText runs a single no-tools completion and returns the concatenated
// text output.
func completeText(ctx context.Context, resolved *provider.Resolved, model provider.Model, system, prompt string) (string, error) {
	req := provider.Request{
		Model:     model.ID,
		System:    system,
		Messages:  []provider.Msg{{Role: "user", Content: []provider.ContentBlock{{Type: "text", Text: prompt}}}},
		MaxTokens: 512,
	}
	ch, err := resolved.Client().Stream(ctx, req, nil)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for ev := range ch {
		switch ev.Type {
		case "text-delta":
			sb.WriteString(ev.Text)
		case "error":
			if ev.Err != nil {
				return "", ev.Err
			}
			return "", errors.New("completion failed")
		}
	}
	return sb.String(), nil
}

func cleanTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, "\"'`“”‘’")
	if len(s) >= 6 && strings.EqualFold(s[:6], "title:") {
		s = strings.TrimSpace(s[6:])
		s = strings.Trim(s, "\"'`“”‘’")
	}
	return shorten(s)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func shorten(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return string(r)
}
