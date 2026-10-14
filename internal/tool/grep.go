package tool

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var skipDirs = map[string]bool{"node_modules": true, ".git": true, "dist": true, "build": true, "target": true, "__pycache__": true, ".venv": true, "venv": true, ".idea": true, ".next": true, ".nuxt": true}

type GrepTool struct{}

func (GrepTool) Name() string { return "grep" }

func (GrepTool) Description() string {
	return `Fast content search tool that works with any codebase size
- Searches file contents using regular expressions
- Supports full regex syntax (eg. "log.*Error", "function\s+\w+", etc.)
- Filter files by pattern with the include parameter (eg. "*.js", "*.{ts,tsx}")
- Returns file paths and line numbers with matching lines of found files
- Use this tool when you need to find files containing specific patterns
- If you need to identify/count the number of matches within files, use the Bash tool with 'rg' (ripgrep) directly. Do NOT use 'grep'.
- When you are doing an open ended search that may require multiple rounds of globbing and grepping, use the Agent tool instead`
}

func (GrepTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string", "description": "The regex pattern to search for in file contents"},
			"path":    map[string]any{"type": "string", "description": "The directory to search in. Defaults to the current working directory."},
			"include": map[string]any{"type": "string", "description": "File pattern to include in the search (e.g. \"*.js\", \"*.{ts,tsx}\")"},
		},
		"required": []any{"pattern"},
	}
}

type grepMatch struct {
	file   string
	lineNo int
	text   string
}

func globToReg(glob string) *regexp.Regexp {
	// translate simple glob to regex on file names
	var sb strings.Builder
	sb.WriteString("^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				sb.WriteString(".*")
				i++
				if i+1 < len(glob) && glob[i+1] == '.' {
					i++
				}
				continue
			}
			sb.WriteString("[^.]*")
		case '?':
			sb.WriteString("[^.]")
		case '.':
			sb.WriteString("\\.")
		case '{':
			end := strings.IndexByte(glob[i:], '}')
			if end >= 0 {
				opts := strings.Split(glob[i+1:i+end], ",")
				sb.WriteString("(?:")
				for j, o := range opts {
					if j > 0 {
						sb.WriteString("|")
					}
					sb.WriteString(regexp.QuoteMeta(strings.ReplaceAll(o, ".", "\\.")))
				}
				sb.WriteString(")")
				i += end
				continue
			}
			sb.WriteString("\\{")
		default:
			if strings.ContainsAny(string(c), `+()|^$[]\`) {
				sb.WriteString("\\" + string(c))
			} else {
				sb.WriteString(string(c))
			}
		}
	}
	sb.WriteString("$")
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return nil
	}
	return re
}

func walkFiles(root string, fn func(path string, d fs.DirEntry) bool) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && p != root {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if !fn(p, d) {
			return filepath.SkipDir
		}
		return nil
	})
}

func (GrepTool) Run(c *Ctx, args json.RawMessage) (Result, error) {
	var p struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Include string `json:"include"`
	}
	_ = json.Unmarshal(args, &p)
	if p.Pattern == "" {
		return Result{}, errors.New("pattern is required")
	}
	re, err := regexp.Compile(p.Pattern)
	if err != nil {
		return Result{}, fmt.Errorf("Invalid pattern: %s", err)
	}
	var incRe *regexp.Regexp
	if p.Include != "" {
		incRe = globToReg(p.Include)
	}

	search := c.Directory
	if p.Path != "" {
		search = absPath(c, p.Path)
		fi, err := os.Stat(search)
		if err != nil {
			return Result{}, fmt.Errorf("Path not found: %s", search)
		}
		if !fi.IsDir() {
			search = filepath.Dir(search)
		}
	}

	if c.Ask != nil {
		if err := c.Ask(AskInput{Permission: "grep", Patterns: []string{p.Pattern}, Always: []string{"*"}, Metadata: map[string]any{"pattern": p.Pattern, "path": search}}); err != nil {
			return Result{}, err
		}
	}

	const limit = 100
	var mu sync.Mutex
	var matches []grepMatch
	found := false
	deadline := time.Now().Add(20 * time.Second)
	walkFiles(search, func(path string, d fs.DirEntry) bool {
		if incRe != nil && !incRe.MatchString(d.Name()) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		f, err := os.Open(path)
		if err != nil {
			return true
		}
		defer f.Close()
		buf := make([]byte, 64*1024)
		n, err := f.Read(buf)
		if err != nil && n == 0 {
			return true
		}
		if isBinary(buf[:n]) {
			return true
		}
		f.Seek(0, 0)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			line := sc.Text()
			if re.MatchString(line) {
				found = true
				mu.Lock()
				matches = append(matches, grepMatch{path, lineNo, line})
				mu.Unlock()
				if len(matches) >= limit {
					return false
				}
			}
		}
		return true
	})

	if !found && len(matches) == 0 {
		return Result{Title: p.Pattern, Metadata: map[string]any{"matches": 0, "truncated": false}, Output: "No files found"}, nil
	}
	truncated := len(matches) >= limit

	var sb strings.Builder
	if truncated {
		sb.WriteString(fmt.Sprintf("Found %d matches (more matches available)\n", len(matches)))
	} else {
		sb.WriteString(fmt.Sprintf("Found %d matches\n", len(matches)))
	}
	byFile := map[string][]grepMatch{}
	var order []string
	for _, m := range matches {
		if _, ok := byFile[m.file]; !ok {
			order = append(order, m.file)
		}
		byFile[m.file] = append(byFile[m.file], m)
	}
	sort.Strings(order)
	for _, f := range order {
		sb.WriteString("\n" + f + ":\n")
		for _, m := range byFile[f] {
			sb.WriteString(fmt.Sprintf("  Line %d: %s\n", m.lineNo, strings.TrimSpace(m.text)))
		}
	}
	if truncated {
		sb.WriteString("\n(Results truncated. Consider using a more specific path or pattern.)")
	}
	return Result{Title: p.Pattern, Metadata: map[string]any{"matches": len(matches), "truncated": truncated}, Output: sb.String()}, nil
}
