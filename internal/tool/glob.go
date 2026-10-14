package tool

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type GlobTool struct{}

func (GlobTool) Name() string { return "glob" }

func (GlobTool) Description() string {
	return `- Fast file pattern matching tool that works with any codebase size
- Supports glob patterns like "**/*.js" or "src/**/*.ts"
- Returns matching file paths
- Use this tool when you need to find files by name patterns
- When you are doing an open ended search that may require multiple rounds of globbing and grepping, use the Agent tool instead
- You have the capability to call multiple tools in a single response. It is always better to speculatively perform multiple searches as a batch that are potentially useful.`
}

func (GlobTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string", "description": "The glob pattern to match files against"},
			"path":    map[string]any{"type": "string", "description": "The directory to search in. If not specified, the current working directory will be used. IMPORTANT: Omit this field to use the default directory. DO NOT enter \"undefined\" or \"null\" - simply omit it for the default behavior. Must be a valid directory path if provided."},
		},
		"required": []any{"pattern"},
	}
}

// doublestar-style match: pattern segments, "**" matches any depth
func matchSegments(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if matchSegments(pattern[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, _ := filepath.Match(pattern[0], name[0])
	return ok && matchSegments(pattern[1:], name[1:])
}

func matchPath(pattern, rel string) bool {
	pat := strings.Split(filepath.ToSlash(pattern), "/")
	nm := strings.Split(filepath.ToSlash(rel), "/")
	return matchSegments(pat, nm)
}

func (GlobTool) Run(c *Ctx, args json.RawMessage) (Result, error) {
	var p struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	_ = json.Unmarshal(args, &p)
	if p.Pattern == "" {
		return Result{}, errors.New("pattern is required")
	}
	search := c.Directory
	if p.Path != "" {
		search = absPath(c, p.Path)
		fi, err := os.Stat(search)
		if err != nil {
			return Result{}, errors.New("glob path must be a directory: " + search)
		}
		if !fi.IsDir() {
			return Result{}, errors.New("glob path must be a directory")
		}
	}

	if c.Ask != nil {
		if err := c.Ask(AskInput{Permission: "glob", Patterns: []string{p.Pattern}, Always: []string{"*"}, Metadata: map[string]any{"pattern": p.Pattern, "path": search}}); err != nil {
			return Result{}, err
		}
	}

	const limit = 100
	var files []string
	base := search
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && path != base {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			rel = path
		}
		if matchPath(p.Pattern, rel) {
			files = append(files, filepath.Clean(filepath.Join(base, rel)))
			if len(files) >= limit {
				return filepath.SkipAll
			}
		}
		return nil
	})
	sort.Strings(files)

	title := relPattern(c, search)
	if len(files) == 0 {
		return Result{Title: title, Metadata: map[string]any{"count": 0, "truncated": false}, Output: "No files found"}, nil
	}
	out := strings.Join(files, "\n")
	truncated := len(files) >= limit
	if truncated {
		out += "\n\n(Results are truncated: showing first 100 results. Consider using a more specific path or pattern.)"
	}
	return Result{Title: title, Metadata: map[string]any{"count": len(files), "truncated": truncated}, Output: out}, nil
}
