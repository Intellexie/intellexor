package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type ModelCfg struct {
	ID          string         `json:"id,omitempty"`
	Name        string         `json:"name,omitempty"`
	Cost        *CostCfg       `json:"cost,omitempty"`
	Limit       *LimitCfg      `json:"limit,omitempty"`
	ToolCall    *bool          `json:"tool_call,omitempty"`
	Reasoning   *bool          `json:"reasoning,omitempty"`
	Attachment  *bool          `json:"attachment,omitempty"`
	Temperature *bool          `json:"temperature,omitempty"`
	Options     map[string]any `json:"options,omitempty"`
	Modalities  map[string]any `json:"modalities,omitempty"`
}

type CostCfg struct {
	Input      *float64 `json:"input,omitempty"`
	Output     *float64 `json:"output,omitempty"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
}

type LimitCfg struct {
	Context *float64 `json:"context,omitempty"`
	Output  *float64 `json:"output,omitempty"`
}

type ProviderCfg struct {
	Name    string                    `json:"name,omitempty"`
	Npm     string                    `json:"npm,omitempty"`
	API     string                    `json:"api,omitempty"`
	Env     []string                  `json:"env,omitempty"`
	Options map[string]any            `json:"options,omitempty"`
	Models  map[string]map[string]any `json:"models,omitempty"`
}

type Info struct {
	Schema            string                 `json:"$schema,omitempty"`
	PermissionRules   []Rule                 `json:"-"`
	Shell             string                 `json:"shell,omitempty"`
	LogLevel          string                 `json:"logLevel,omitempty"`
	Model             string                 `json:"model,omitempty"`
	SmallModel        string                 `json:"small_model,omitempty"`
	Username          string                 `json:"username,omitempty"`
	Autoshare         *bool                  `json:"autoshare,omitempty"`
	Autoupdate        *bool                  `json:"autoupdate,omitempty"`
	DisabledProviders []string               `json:"disabled_providers,omitempty"`
	EnabledProviders  []string               `json:"enabled_providers,omitempty"`
	Instructions      []string               `json:"instructions,omitempty"`
	Provider          map[string]ProviderCfg `json:"provider,omitempty"`
	Permission        map[string]any         `json:"permission,omitempty"`
	Compaction        *CompactionCfg         `json:"compaction,omitempty"`
	ToolOutput        *ToolOutputCfg         `json:"tool_output,omitempty"`
	DisabledTools     []string               `json:"disabled_tools,omitempty"`
	Commands          map[string]CommandCfg  `json:"commands,omitempty"`
	Agent             map[string]AgentCfg    `json:"agent,omitempty"`
	SubagentDepth     *int                   `json:"subagent_depth,omitempty"`
	MCP               map[string]any         `json:"mcp,omitempty"`
	Share             string                 `json:"share,omitempty"`
	Extra             map[string]any         `json:"-"`
}

type CompactionCfg struct {
	Auto     *bool    `json:"auto,omitempty"`
	Reserved *float64 `json:"reserved,omitempty"`
}

type ToolOutputCfg struct {
	MaxLines *int `json:"max_lines,omitempty"`
	MaxBytes *int `json:"max_bytes,omitempty"`
}

type CommandCfg struct {
	Description string `json:"description,omitempty"`
	Template    string `json:"template,omitempty"`
	Agent       string `json:"agent,omitempty"`
	Model       string `json:"model,omitempty"`
	Subtask     *bool  `json:"subtask,omitempty"`
}

type AgentCfg struct {
	Description string          `json:"description,omitempty"`
	Model       string          `json:"model,omitempty"`
	Prompt      string          `json:"prompt,omitempty"`
	Mode        string          `json:"mode,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Permission  map[string]any  `json:"permission,omitempty"`
	Steps       *int            `json:"steps,omitempty"`
	Hidden      *bool           `json:"hidden,omitempty"`
	Color       string          `json:"color,omitempty"`
	Tools       map[string]bool `json:"tools,omitempty"`
}

// Permission rules from config (opencode format)
type Rule struct {
	Permission string `json:"permission"`
	Pattern    string `json:"pattern"`
	Action     string `json:"action"`
}

func ParsePermission(m map[string]any) []Rule {
	var rules []Rule
	for perm, v := range m {
		switch t := v.(type) {
		case string:
			rules = append(rules, Rule{Permission: perm, Pattern: "*", Action: t})
		case map[string]any:
			for pattern, action := range t {
				if s, ok := action.(string); ok {
					rules = append(rules, Rule{Permission: perm, Pattern: pattern, Action: s})
				}
			}
		}
	}
	return rules
}

// ---- jsonc ----

func StripJSONC(src string) string {
	var out strings.Builder
	inStr := false
	inLine := false
	inBlock := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inLine {
			if c == '\n' {
				inLine = false
				out.WriteByte(c)
			}
			continue
		}
		if inBlock {
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				i++
			}
			continue
		}
		if inStr {
			out.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				i++
				out.WriteByte(src[i])
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			out.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(src) {
			if src[i+1] == '/' {
				inLine = true
				i++
				continue
			}
			if src[i+1] == '*' {
				inBlock = true
				i++
				continue
			}
		}
		out.WriteByte(c)
	}
	// strip trailing commas
	s := out.String()
	var clean strings.Builder
	inStr = false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			clean.WriteByte(c)
			if c == '\\' && i+1 < len(s) {
				i++
				clean.WriteByte(s[i])
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			clean.WriteByte(c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue
			}
		}
		clean.WriteByte(c)
	}
	return clean.String()
}

func LoadFile(path string) (*Info, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var info Info
	if err := json.Unmarshal([]byte(StripJSONC(string(b))), &info); err != nil {
		return nil, err
	}
	var extra map[string]any
	_ = json.Unmarshal([]byte(StripJSONC(string(b))), &extra)
	info.Extra = extra
	info.PermissionRules = ParsePermissionOrdered(string(b))
	return &info, nil
}

// ParsePermissionOrdered converts the "permission" config section into rules
// preserving JSON key order (opencode is last-match-wins, so order matters:
// write "*" first, specific patterns after).
func ParsePermissionOrdered(src string) []Rule {
	dec := json.NewDecoder(strings.NewReader(StripJSONC(src)))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil
		}
		key := keyTok.(string)
		if key != "permission" {
			var v any
			_ = dec.Decode(&v)
			continue
		}
		valTok, err := dec.Token()
		if err != nil {
			return nil
		}
		if s, ok := valTok.(string); ok {
			return []Rule{{Permission: key, Pattern: "*", Action: s}}
		}
		if d, ok := valTok.(json.Delim); !ok || d != '{' {
			return nil
		}
		var rules []Rule
		for dec.More() {
			toolTok, err := dec.Token()
			if err != nil {
				return rules
			}
			tool := toolTok.(string)
			t2, err := dec.Token()
			if err != nil {
				return rules
			}
			if s, ok := t2.(string); ok {
				rules = append(rules, Rule{Permission: tool, Pattern: "*", Action: s})
				continue
			}
			if d, ok := t2.(json.Delim); ok && d == '{' {
				for dec.More() {
					patTok, err := dec.Token()
					if err != nil {
						break
					}
					if d, ok := patTok.(json.Delim); ok && d == '}' {
						break
					}
					pattern, ok := patTok.(string)
					if !ok {
						break
					}
					var action any
					if err := dec.Decode(&action); err != nil {
						break
					}
					if a, ok := action.(string); ok {
						rules = append(rules, Rule{Permission: tool, Pattern: pattern, Action: a})
					}
				}
				if t3, err := dec.Token(); err != nil {
					return rules
				} else if _, ok := t3.(json.Delim); !ok {
					return rules
				}
			}
		}
		if _, err := dec.Token(); err != nil { // consume closing '}'
			return rules
		}
		return rules
	}
	return nil
}

var mu sync.Mutex
var globalCache *Info
var globalKey string

// Global config: ~/.config/intellexor/opencode.json | intellexor.json, plus legacy opencode dirs
func configDirOf() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "intellexor")
}

func Global() *Info {
	mu.Lock()
	defer mu.Unlock()
	home, _ := os.UserHomeDir()
	candidates := []string{}
	if home != "" {
		configBase := filepath.Join(home, ".config")
		if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
			configBase = v
		}
		candidates = append(candidates,
			filepath.Join(configDirOf(), "intellexor.json"),
			filepath.Join(configDirOf(), "opencode.json"),
			filepath.Join(configBase, "opencode", "opencode.json"),
			filepath.Join(home, "opencode.json"),
			// container convention: config mounted at /app (docker alias)
			"/app/intellexor.json",
			"/app/opencode.json",
		)
	}
	key := strings.Join(candidates, "|")
	if globalCache != nil && globalKey == key {
		return globalCache
	}
	merged := &Info{Provider: map[string]ProviderCfg{}, Agent: map[string]AgentCfg{}, Commands: map[string]CommandCfg{}}
	for _, c := range candidates {
		if info, err := LoadFile(c); err == nil {
			merge(merged, info)
		}
	}
	globalCache = merged
	globalKey = key
	return merged
}

func InvalidateGlobal() {
	mu.Lock()
	globalCache = nil
	mu.Unlock()
}

// Project config: <dir>/opencode.json(.c) / intellexor.json(.c), merged upward to root
func Project(dir string) *Info {
	out := &Info{Provider: map[string]ProviderCfg{}, Agent: map[string]AgentCfg{}, Commands: map[string]CommandCfg{}}
	var chain []*Info
	cur := dir
	for {
		for _, name := range []string{"intellexor.json", "intellexor.jsonc", "opencode.json", "opencode.jsonc"} {
			if info, err := LoadFile(filepath.Join(cur, name)); err == nil {
				chain = append([]*Info{info}, chain...)
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	for _, c := range chain {
		merge(out, c)
	}
	return out
}

// Merged global + project config for a directory
func Merge(dir string) *Info {
	out := &Info{Provider: map[string]ProviderCfg{}, Agent: map[string]AgentCfg{}, Commands: map[string]CommandCfg{}}
	merge(out, Global())
	merge(out, Project(dir))
	return out
}

func merge(dst, src *Info) {
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.SmallModel != "" {
		dst.SmallModel = src.SmallModel
	}
	if src.Shell != "" {
		dst.Shell = src.Shell
	}
	if src.Username != "" {
		dst.Username = src.Username
	}
	if src.Autoshare != nil {
		dst.Autoshare = src.Autoshare
	}
	if src.Autoupdate != nil {
		dst.Autoupdate = src.Autoupdate
	}
	if src.SubagentDepth != nil {
		dst.SubagentDepth = src.SubagentDepth
	}
	if src.Share != "" {
		dst.Share = src.Share
	}
	if len(src.PermissionRules) > 0 {
		dst.PermissionRules = append(dst.PermissionRules, src.PermissionRules...)
	}
	if len(src.Instructions) > 0 {
		dst.Instructions = append(dst.Instructions, src.Instructions...)
	}
	if len(src.DisabledProviders) > 0 {
		dst.DisabledProviders = append(dst.DisabledProviders, src.DisabledProviders...)
	}
	if len(src.EnabledProviders) > 0 {
		dst.EnabledProviders = append(dst.EnabledProviders, src.EnabledProviders...)
	}
	if len(src.DisabledTools) > 0 {
		dst.DisabledTools = append(dst.DisabledTools, src.DisabledTools...)
	}
	if src.Compaction != nil {
		dst.Compaction = src.Compaction
	}
	if src.ToolOutput != nil {
		dst.ToolOutput = src.ToolOutput
	}
	for k, v := range src.Permission {
		if dst.Permission == nil {
			dst.Permission = map[string]any{}
		}
		dst.Permission[k] = v
	}
	for k, v := range src.Provider {
		dst.Provider[k] = v
	}
	for k, v := range src.Agent {
		dst.Agent[k] = v
	}
	for k, v := range src.Commands {
		dst.Commands[k] = v
	}
	for k, v := range src.MCP {
		if dst.MCP == nil {
			dst.MCP = map[string]any{}
		}
		dst.MCP[k] = v
	}
	for k, v := range src.Extra {
		if dst.Extra == nil {
			dst.Extra = map[string]any{}
		}
		dst.Extra[k] = v
	}
}
