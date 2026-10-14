package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Intellexie/intellexor/internal/config"

	"github.com/Intellexie/intellexor/internal/provider"
	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/util"
)

var isGitRootCache = map[string]bool{}

func isGit(dir string) bool {
	if v, ok := isGitRootCache[dir]; ok {
		return v
	}
	_, err := os.Stat(filepath.Join(dir, ".git"))
	v := err == nil
	isGitRootCache[dir] = v
	return v
}

type runOpts struct {
	Async bool
}

func (i *Instance) Prompt(s *session.Session, in PromptInput) (session.WithParts, error) {
	return i.PromptCtx(context.Background(), s, in)
}

func (i *Instance) PromptCtx(parent context.Context, s *session.Session, in PromptInput) (session.WithParts, error) {
	l := i.lock(s.ID)
	l.Lock()
	defer l.Unlock()

	ctx, cancel := context.WithCancel(parent)
	i.setCancel(s.ID, cancel)
	defer func() {
		cancel()
		i.clearCancel(s.ID)
	}()

	return i.runLoop(ctx, s, in)
}

func (i *Instance) runLoop(ctx context.Context, s *session.Session, in PromptInput) (session.WithParts, error) {
	i.SetStatus(s.ID, StatusInfo{"type": "busy"})
	defer i.SetStatus(s.ID, StatusInfo{"type": "idle"})

	// re-read under the session lock so a concurrent revert/unrevert is seen
	i.refresh(s)

	agentName := in.Agent
	if agentName == "" {
		agentName = s.Agent
	}
	if agentName == "" {
		agentName = "build"
	}
	agent := i.Agent(agentName)

	providerID, modelID := resolveModelSel(in.Model, s.Model)
	if providerID == "" {
		if m := config.Merge(i.Directory).Model; m != "" {
			if p, mo, ok := strings.Cut(m, "/"); ok {
				providerID, modelID = p, mo
			}
		}
	}
	if providerID == "" {
		all, _ := provider.List(i.Directory)
		providerID = pickDefault(all)
		if providerID == "" {
			return session.WithParts{}, errors.New("no providers available. Configure a provider in settings")
		}
	}

	resolved, model, err := provider.Resolve(i.Directory, providerID, modelID)
	if err != nil {
		return session.WithParts{}, err
	}
	resolved.Client()

	// continuing a reverted session permanently discards the reverted turns
	if s.Revert != nil {
		i.purgeReverted(s)
	}

	// user message
	userMsg := session.UserMessage{
		ID:        in.MessageID,
		SessionID: s.ID,
		Role:      "user",
		Time:      session.UserMsgTime{Created: util.Now()},
		Agent:     agentName,
		Model:     session.UserModel{ProviderID: resolved.ID, ModelID: model.ID},
		Format:    in.Format,
		System:    in.System,
		Tools:     in.Tools,
	}
	if userMsg.ID == "" {
		userMsg.ID = util.ID("msg")
	}
	if err := i.Mgr.SaveMessage(s, userMsg); err != nil {
		return session.WithParts{}, err
	}
	i.Hub.Publish("message.updated", map[string]any{"sessionID": s.ID, "info": userMsg})

	var userParts []session.Part
	for _, raw := range in.Parts {
		var pi partInput
		if err := json.Unmarshal(raw, &pi); err != nil {
			continue
		}
		p := session.Part{
			ID: pi.ID, SessionID: s.ID, MessageID: userMsg.ID,
		}
		if p.ID == "" {
			p.ID = util.ID("prt")
		}
		switch pi.Type {
		case "file":
			p.Type = "file"
			p.Mime = pi.Mime
			p.URL = pi.URL
			p.Filename = pi.Filename
		case "agent":
			p.Type = "agent"
			p.Name = pi.Name
		case "subtask":
			p.Type = "subtask"
			p.Prompt = pi.Prompt
			p.Description = pi.Description
			p.Agent = pi.Agent
		default:
			p.Type = "text"
			p.Text = pi.Text
			if pi.Synthetic != nil {
				p.Synthetic = *pi.Synthetic
			}
			if pi.Ignored != nil {
				p.Ignored = *pi.Ignored
			}
		}
		userParts = append(userParts, p)
		_ = i.Mgr.SavePart(s, p)
		i.Hub.Publish("message.part.updated", map[string]any{"sessionID": s.ID, "part": p, "time": util.Now()})
	}

	// first-message title generation
	if firstUserMessage(i, s) {
		go i.generateTitle(s, userParts, resolved, model)
	}

	var asst session.AssistantMessage
	_ = asst
	var stepTools int
	maxSteps := 1000
	if agent.Steps > 0 {
		maxSteps = agent.Steps
	}

	for step := 0; step < maxSteps; step++ {
		if ctx.Err() != nil {
			break
		}
		asst = session.AssistantMessage{
			ID:         util.ID("msg"),
			SessionID:  s.ID,
			Role:       "assistant",
			ParentID:   userMsg.ID,
			Time:       session.MsgTime{Created: util.Now()},
			ModelID:    model.ID,
			ProviderID: resolved.ID,
			Mode:       agent.Mode,
			Agent:      agentName,
			Path:       session.Path{Cwd: i.Directory, Root: i.Worktree},
			Variant:    variantOf(in.Variant, s.Model),
		}
		_ = i.Mgr.SaveMessage(s, asst)
		i.Hub.Publish("message.updated", map[string]any{"sessionID": s.ID, "info": asst})

		p := newProcessor(i, s, &asst)
		p.runStep(ctx, resolved, model, agentName, agent, in.System)

		stepTools = p.toolsCalled

		finishStep(i, s, &asst, p)

		if p.failed || stepTools == 0 {
			break
		}
	}

	_ = stepTools
	msgs, _ := i.Mgr.LoadMessages(s)
	for j := len(msgs) - 1; j >= 0; j-- {
		if a, ok := msgs[j].Info.(session.AssistantMessage); ok && a.ID == asst.ID {
			return msgs[j], nil
		}
	}
	return session.WithParts{Info: asst}, nil
}

func variantOf(v string, sm *session.ModelRef) string {
	if v != "" {
		return v
	}
	if sm != nil {
		return sm.Variant
	}
	return ""
}

// ---- helpers ----

func resolveModelSel(sel *ModelSel, sm *session.ModelRef) (string, string) {
	if sel != nil && sel.ProviderID != "" && sel.ModelID != "" {
		return sel.ProviderID, sel.ModelID
	}
	if sm != nil && sm.ProviderID != "" && sm.ID != "" {
		return sm.ProviderID, sm.ID
	}
	return "", ""
}

func firstUserMessage(i *Instance, s *session.Session) bool {
	msgs, err := i.Mgr.LoadMessages(s)
	if err != nil {
		return true
	}
	n := 0
	for _, m := range msgs {
		if u, ok := m.Info.(session.UserMessage); ok && u.ID != "" {
			n++
		}
	}
	return n <= 1
}

func asstID(msgs []session.WithParts) string {
	for j := len(msgs) - 1; j >= 0; j-- {
		if a, ok := msgs[j].Info.(session.AssistantMessage); ok {
			return a.ID
		}
	}
	return ""
}

func messageIDOf(info any) string {
	switch t := info.(type) {
	case session.UserMessage:
		return t.ID
	case session.AssistantMessage:
		return t.ID
	}
	return ""
}

type partInput struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	ID          string `json:"id,omitempty"`
	Synthetic   *bool  `json:"synthetic,omitempty"`
	Ignored     *bool  `json:"ignored,omitempty"`
	Mime        string `json:"mime,omitempty"`
	URL         string `json:"url,omitempty"`
	Filename    string `json:"filename,omitempty"`
	Name        string `json:"name,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	Description string `json:"description,omitempty"`
	Agent       string `json:"agent,omitempty"`
}

// deterministic default provider pick: keyed providers first, local runtimes last
func pickDefault(all map[string]*provider.Resolved) string {
	pref := []string{"anthropic", "openai", "google", "openrouter", "deepseek", "xai", "groq", "mistral"}
	for _, p := range pref {
		if r, ok := all[p]; ok && r.APIKey != "" {
			return p
		}
	}
	var ids []string
	for id, r := range all {
		if r.APIKey != "" && id != "ollama" && id != "lmstudio" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		return ids[0]
	}
	for _, p := range []string{"ollama", "lmstudio"} {
		if _, ok := all[p]; ok {
			return p
		}
	}
	var rest []string
	for id := range all {
		rest = append(rest, id)
	}
	sort.Strings(rest)
	if len(rest) > 0 {
		return rest[0]
	}
	return ""
}
