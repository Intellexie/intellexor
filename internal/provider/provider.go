package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"intellexor/internal/auth"
	"intellexor/internal/config"
)

// Wire shape (v1 Provider.Info)
type Info struct {
	ID      string           `json:"id"`
	Name    string           `json:"name"`
	Source  string           `json:"source"`
	Env     []string         `json:"env"`
	Key     string           `json:"key,omitempty"`
	Options map[string]any   `json:"options"`
	Models  map[string]Model `json:"models"`
}

type Resolved struct {
	ID       string
	Name     string
	Npm      string
	Protocol string // anthropic | openai | gemini
	APIKey   string
	BaseURL  string
	Headers  map[string]string
	Models   map[string]Model
	Env      []string
	Source   string // env | config | custom | api | wellknown
	OAuth    *auth.Info
}

func envKeys(envs []string) (string, bool) {
	for _, e := range envs {
		if v := os.Getenv(e); v != "" {
			return v, true
		}
	}
	return "", false
}

// Focus: OpenAI-compatible (chat/completions) is the default wire protocol for
// every provider — matching vLLM / one-api style deployments. Only an explicit
// api override in config switches protocol.
func protocolFor(npm string, url string, override string) string {
	switch override {
	case "anthropic", "gemini", "openai":
		return override
	}
	return "openai"
}

func defaultBaseURL(protocol string) string {
	switch protocol {
	case "anthropic":
		return "https://api.anthropic.com/v1"
	case "gemini":
		return "https://generativelanguage.googleapis.com/v1beta"
	default:
		return "https://api.openai.com/v1"
	}
}

// List all resolvable providers for a directory's merged config
func List(dir string) (map[string]*Resolved, string) {
	cfg := config.Merge(dir)
	g := config.Global()
	cat := CatalogNow()
	auths := auth.All()

	connected := map[string]*Resolved{}
	var order []string
	defaults := map[string]string{}

	add := func(id string, r *Resolved) {
		if _, ok := connected[id]; !ok {
			order = append(order, id)
		}
		connected[id] = r
	}

	disabled := map[string]bool{}
	for _, d := range cfg.DisabledProviders {
		disabled[d] = true
	}
	for _, d := range g.DisabledProviders {
		disabled[d] = true
	}

	enabledFilter := map[string]bool{}
	for _, e := range append(cfg.EnabledProviders, g.EnabledProviders...) {
		enabledFilter[e] = true
	}

	// catalog providers
	for id, cp := range cat.Providers {
		if disabled[id] {
			continue
		}
		if len(enabledFilter) > 0 && !enabledFilter[id] {
			// config- and env-provided ones still get a chance below
		}
		r := &Resolved{
			ID: id, Name: cp.Name, Npm: cp.Npm,
			Protocol: protocolFor(cp.Npm, cp.URL, ""),
			Models:   cat.ModelModels(id),
			Env:      cp.Env,
		}
		if r.Protocol == "openai" && cp.URL != "" {
			r.BaseURL = cp.URL
		}
		if cp.Npm == "@openrouter/ai-sdk-provider" && r.BaseURL == "" {
			r.BaseURL = "https://openrouter.ai/api/v1"
		}
		if r.BaseURL == "" {
			r.BaseURL = defaultBaseURL(r.Protocol)
		}
		// key precedence: config > auth.json > env; auth beats env per opencode semantics
		if key, ok := envKeys(cp.Env); ok {
			r.APIKey = key
			r.Source = "env"
		}
		if a, ok := auths[id]; ok {
			if a.Key != "" {
				r.APIKey = a.Key
				r.Source = "api"
			} else if a.Access != "" {
				r.OAuth = &a
				r.APIKey = a.Access
				r.Source = "wellknown"
			}
			if a.Metadata != nil {
				if bu, ok := a.Metadata["baseURL"].(string); ok && bu != "" {
					r.BaseURL = bu
				}
			}
		}
		if pc, ok := cfg.Provider[id]; ok {
			if pc.Name != "" {
				r.Name = pc.Name
			}
			if pc.API != "" {
				r.Protocol = protocolFor(pc.Npm, "", pc.API)
			}
			if pc.Npm != "" {
				r.Npm = pc.Npm
			}
			applyOptions(r, pc.Options)
			for mid, mm := range pc.Models {
				mergeModel(r.Models, mid, mm)
			}
		}
		if len(enabledFilter) > 0 && !enabledFilter[id] {
			// provider not in explicit allow-list: still available if it has a key, matching opencode "enabled_providers"
		}
		if r.APIKey != "" || id == "ollama" || id == "lmstudio" {
			add(id, r)
		}
	}

	// providers declared only in config
	for _, src := range []*config.Info{g, cfg} {
		for id, pc := range src.Provider {
			if disabled[id] {
				continue
			}
			if _, ok := connected[id]; ok {
				continue
			}
			r := &Resolved{ID: id, Name: id, Models: map[string]Model{}, Source: "config"}
			if pc.Name != "" {
				r.Name = pc.Name
			}
			r.Protocol = protocolFor(pc.Npm, "", pc.API)
			r.BaseURL = defaultBaseURL(r.Protocol)
			if pc.Npm == "@ai-sdk/openai-compatible" || pc.Npm == "@openrouter/ai-sdk-provider" {
				// url comes from options
			}
			applyOptions(r, pc.Options)
			for mid, mm := range pc.Models {
				mergeModel(r.Models, mid, mm)
			}
			if r.APIKey == "" {
				if key, ok := envKeys(r.Env); ok {
					r.APIKey = key
					r.Source = "env"
				}
			}
			connected[id] = r
			order = append(order, id)
		}
	}

	// defaults from config model / small_model
	for _, src := range []*config.Info{g, cfg} {
		if src.Model != "" {
			if p, m, ok := strings.Cut(src.Model, "/"); ok {
				defaults[p] = m
			}
		}
	}
	return connected, strings.Join(order, ",")
}

func applyOptions(r *Resolved, opts map[string]any) {
	if opts == nil {
		return
	}
	if v, ok := opts["apiKey"].(string); ok && v != "" {
		r.APIKey = v
		r.Source = "config"
	}
	if v, ok := opts["baseURL"].(string); ok && v != "" {
		r.BaseURL = v
	}
	if v, ok := opts["api"].(string); ok && v != "" {
		r.BaseURL = v
	}
	if hs, ok := opts["headers"].(map[string]any); ok {
		if r.Headers == nil {
			r.Headers = map[string]string{}
		}
		for k, v := range hs {
			if s, ok := v.(string); ok {
				r.Headers[k] = s
			}
		}
	}
}

func fnum(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

func mergeModel(models map[string]Model, id string, mm map[string]any) {
	m, ok := models[id]
	if !ok {
		m = Model{ID: id, Name: id}
	}
	if v, ok := mm["name"].(string); ok && v != "" {
		m.Name = v
	}
	if v, ok := mm["tool_call"].(bool); ok {
		m.ToolCall = v
	}
	if v, ok := mm["reasoning"].(bool); ok {
		m.Reasoning = v
	}
	if v, ok := mm["attachment"].(bool); ok {
		m.Attachment = v
	}
	if v, ok := mm["options"].(map[string]any); ok {
		if m.Options == nil {
			m.Options = map[string]any{}
		}
		for k, v := range v {
			m.Options[k] = v
		}
	}
	if cost, ok := mm["cost"].(map[string]any); ok {
		if v, ok := fnum(cost["input"]); ok {
			m.Cost.Input = v
		}
		if v, ok := fnum(cost["output"]); ok {
			m.Cost.Output = v
		}
		if v, ok := fnum(cost["cache_read"]); ok {
			m.Cost.CacheRead = v
		}
		if v, ok := fnum(cost["cache_write"]); ok {
			m.Cost.CacheWrite = v
		}
	}
	if limit, ok := mm["limit"].(map[string]any); ok {
		if v, ok := fnum(limit["context"]); ok {
			m.Limit.Context = v
		}
		if v, ok := fnum(limit["output"]); ok {
			m.Limit.Output = v
		}
	}
	models[id] = m
}

func (r *Resolved) GetModel(modelID string) (Model, error) {
	if m, ok := r.Models[modelID]; ok {
		return m, nil
	}
	return Model{}, fmt.Errorf("No model found for %s/%s", r.ID, modelID)
}

// Resolve picks provider+model for a session; errors mirror opencode messages
func Resolve(dir, providerID, modelID string) (*Resolved, Model, error) {
	all, _ := List(dir)
	r, ok := all[providerID]
	if !ok {
		if modelID == "" {
			return nil, Model{}, fmt.Errorf("No provider matches: %s", providerID)
		}
		return nil, Model{}, fmt.Errorf("No provider matches: %s", providerID)
	}
	if modelID == "" {
		for _, id := range []string{"claude-sonnet-4-5", "gpt-5", "gemini-2.5-flash"} {
			if m, err := r.GetModel(id); err == nil {
				return r, m, nil
			}
		}
		for _, m := range r.Models {
			return r, m, nil
		}
		return nil, Model{}, fmt.Errorf("No models found for provider: %s", providerID)
	}
	m, err := r.GetModel(modelID)
	if err != nil {
		return nil, Model{}, err
	}
	return r, m, nil
}

func DecodeKey(a auth.Info) string { return a.Key }

// b64 helpers for images
func MustB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// SortedKeys orders providers: keyed providers first (alphabetical), local
// runtimes (ollama/lmstudio) last.
func SortedKeys(m map[string]*Resolved) []string {
	var keyed, rest, local []string
	for id, r := range m {
		switch {
		case id == "ollama" || id == "lmstudio":
			local = append(local, id)
		case r.APIKey != "":
			keyed = append(keyed, id)
		default:
			rest = append(rest, id)
		}
	}
	sort.Strings(keyed)
	sort.Strings(rest)
	sort.Strings(local)
	var out []string
	out = append(out, keyed...)
	out = append(out, rest...)
	out = append(out, local...)
	return out
}
