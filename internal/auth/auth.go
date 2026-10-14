package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"intellexor/internal/store"
)

type Info struct {
	Type     string         `json:"type"`
	Key      string         `json:"key,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	// oauth shape
	Access    string `json:"access,omitempty"`
	Refresh   string `json:"refresh,omitempty"`
	Expires   int64  `json:"expires,omitempty"`
	AccountID string `json:"accountId,omitempty"`
}

var mu sync.Mutex

func loadFile(path string) map[string]Info {
	m := map[string]Info{}
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func All() map[string]Info {
	mu.Lock()
	defer mu.Unlock()
	out := map[string]Info{}
	for _, p := range store.AuthSearchPaths() {
		for k, v := range loadFile(p) {
			if _, ok := out[k]; !ok {
				out[k] = v
			}
		}
	}
	return out
}

func Get(providerID string) (Info, bool) {
	m, ok := All()[providerID]
	return m, ok
}

func primaryAuthPath() string {
	p := store.AuthSearchPaths()[0]
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	return p
}

func Put(providerID string, info Info) error {
	mu.Lock()
	defer mu.Unlock()
	path := primaryAuthPath()
	m := loadFile(path)
	m[providerID] = info
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func Delete(providerID string) error {
	mu.Lock()
	defer mu.Unlock()
	path := primaryAuthPath()
	m := loadFile(path)
	delete(m, providerID)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
