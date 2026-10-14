package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/Intellexie/intellexor/internal/bus"
	"github.com/Intellexie/intellexor/internal/config"
	"github.com/Intellexie/intellexor/internal/permission"
	"github.com/Intellexie/intellexor/internal/question"
	"github.com/Intellexie/intellexor/internal/session"
)

type Instance struct {
	Directory string
	Worktree  string
	Hub       *bus.Hub

	Mgr       *session.Manager
	Perms     *permission.Service
	Quest     *question.Service
	mu        sync.Mutex
	statuses  map[string]StatusInfo
	locks     map[string]*sync.Mutex
	cancels   map[string]context.CancelFunc
	BaseRules []session.PermissionRule
	UserRules []session.PermissionRule
}

func toRules(rs []config.Rule) []session.PermissionRule {
	var out []session.PermissionRule
	for _, r := range rs {
		out = append(out, session.PermissionRule(r))
	}
	return out
}

// AskChain composes the ruleset for a permission ask in opencode's order:
// defaults, agent builtins, user config — last match wins.
func (i *Instance) AskChain(agentRules []session.PermissionRule) []session.PermissionRule {
	return permission.Merge(i.BaseRules, agentRules, i.UserRules)
}

type StatusInfo map[string]any

type PromptInput struct {
	MessageID string            `json:"messageID,omitempty"`
	Model     *ModelSel         `json:"model,omitempty"`
	Agent     string            `json:"agent,omitempty"`
	NoReply   bool              `json:"noReply,omitempty"`
	Tools     map[string]bool   `json:"tools,omitempty"`
	Format    json.RawMessage   `json:"format,omitempty"`
	System    string            `json:"system,omitempty"`
	Variant   string            `json:"variant,omitempty"`
	Parts     []json.RawMessage `json:"parts"`
}

type ModelSel struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

func New(dir string) *Instance {
	abs, _ := filepath.Abs(dir)
	hub := bus.NewHub()
	mgr := session.NewManager()
	merged := config.Merge(abs)
	inst := &Instance{
		BaseRules: baseRules(),
		UserRules: toRules(merged.PermissionRules),
		Directory: abs,
		Worktree:  gitRoot(abs),
		Hub:       hub,
		Mgr:       mgr,
		Perms:     permission.New(hub, nil),
		Quest:     question.New(hub),
		statuses:  map[string]StatusInfo{},
		locks:     map[string]*sync.Mutex{},
		cancels:   map[string]context.CancelFunc{},
	}
	return inst
}

func gitRoot(dir string) string {
	cur := dir
	for {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return dir
		}
		cur = parent
	}
}

func (i *Instance) lock(sessionID string) *sync.Mutex {
	i.mu.Lock()
	defer i.mu.Unlock()
	l, ok := i.locks[sessionID]
	if !ok {
		l = &sync.Mutex{}
		i.locks[sessionID] = l
	}
	return l
}

func (i *Instance) setCancel(sessionID string, fn context.CancelFunc) {
	i.mu.Lock()
	i.cancels[sessionID] = fn
	i.mu.Unlock()
}

func (i *Instance) clearCancel(sessionID string) {
	i.mu.Lock()
	delete(i.cancels, sessionID)
	i.mu.Unlock()
}

func (i *Instance) Abort(sessionID string) {
	i.mu.Lock()
	fn := i.cancels[sessionID]
	i.mu.Unlock()
	if fn != nil {
		fn()
	}
	for _, p := range i.Perms.Pending(sessionID) {
		i.Perms.Reject(context.Background(), p.ID, "Aborted")
	}
}

func (i *Instance) SetStatus(sessionID string, st StatusInfo) {
	i.mu.Lock()
	if st["type"] == "idle" {
		delete(i.statuses, sessionID)
	} else {
		i.statuses[sessionID] = st
	}
	i.mu.Unlock()
	i.Hub.Publish("session.status", map[string]any{"sessionID": sessionID, "status": st})
	if st["type"] == "idle" {
		i.Hub.Publish("session.idle", map[string]any{"sessionID": sessionID})
	}
}

func (i *Instance) Statuses() map[string]StatusInfo {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := map[string]StatusInfo{}
	for k, v := range i.statuses {
		out[k] = v
	}
	return out
}
