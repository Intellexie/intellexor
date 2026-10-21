package provider

import (
	"encoding/json"
	"sync"
)

type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

type Limit struct {
	Context float64 `json:"context"`
	Output  float64 `json:"output"`
}

type Model struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	ReleaseDate string            `json:"release_date,omitempty"`
	ToolCall    bool              `json:"tool_call"`
	Attachment  bool              `json:"attachment"`
	Reasoning   bool              `json:"reasoning"`
	Temperature bool              `json:"temperature"`
	Cost        Cost              `json:"cost"`
	Limit       Limit             `json:"limit"`
	Options     map[string]any    `json:"options,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Interleaved *bool             `json:"interleaved,omitempty"`
}

type CatProvider struct {
	Name   string                     `json:"name"`
	Env    []string                   `json:"env"`
	Npm    string                     `json:"npm"`
	URL    string                     `json:"url"`
	Models map[string]rawCatalogModel `json:"models"`
}

type rawCatalogModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ReleaseDate string `json:"release_date"`
	ToolCall    *bool  `json:"tool_call"`
	Reasoning   *bool  `json:"reasoning"`
	Attachment  *bool  `json:"attachment"`
	Temperature *bool  `json:"temperature"`
	Cost        struct {
		Input      *float64 `json:"input"`
		Output     *float64 `json:"output"`
		CacheRead  *float64 `json:"cache_read"`
		CacheWrite *float64 `json:"cache_write"`
	} `json:"cost"`
	Limit struct {
		Context *float64 `json:"context"`
		Output  *float64 `json:"output"`
	} `json:"limit"`
	Modalities *struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
}

func (m rawCatalogModel) toModel(id string) Model {
	mo := Model{ID: id, Name: m.Name, ReleaseDate: m.ReleaseDate}
	if m.ToolCall != nil {
		mo.ToolCall = *m.ToolCall
	}
	if m.Reasoning != nil {
		mo.Reasoning = *m.Reasoning
	}
	if m.Attachment != nil {
		mo.Attachment = *m.Attachment
	}
	mo.Temperature = true
	if m.Temperature != nil {
		mo.Temperature = *m.Temperature
	}
	if m.Cost.Input != nil {
		mo.Cost.Input = *m.Cost.Input
	}
	if m.Cost.Output != nil {
		mo.Cost.Output = *m.Cost.Output
	}
	if m.Cost.CacheRead != nil {
		mo.Cost.CacheRead = *m.Cost.CacheRead
	}
	if m.Cost.CacheWrite != nil {
		mo.Cost.CacheWrite = *m.Cost.CacheWrite
	}
	if m.Limit.Context != nil {
		mo.Limit.Context = *m.Limit.Context
	}
	if m.Limit.Output != nil {
		mo.Limit.Output = *m.Limit.Output
	}
	return mo
}

type Catalog struct {
	Providers map[string]CatProvider
}

var (
	catMu   sync.Mutex
	catalog *Catalog
)

// CatalogNow returns the built-in catalog. It is intentionally empty:
// providers and models come from user config (intellexor.json /
// opencode.json) only. The previous models.dev remote fetch and its
// data-dir cache were removed — re-add them if a shared catalog is
// wanted again.
func CatalogNow() *Catalog {
	catMu.Lock()
	defer catMu.Unlock()
	if catalog == nil {
		var c Catalog
		if err := json.Unmarshal([]byte(fallbackCatalog), &c); err == nil {
			catalog = &c
		}
	}
	return catalog
}

func (c *Catalog) ModelModels(providerID string) map[string]Model {
	out := map[string]Model{}
	cp, ok := c.Providers[providerID]
	if !ok {
		return out
	}
	for k, m := range cp.Models {
		out[k] = m.toModel(k)
	}
	return out
}
