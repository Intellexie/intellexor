package tool

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"intellexor/internal/diff"
	"intellexor/internal/session"
	"intellexor/internal/util"
)

// EditTool applies exact-match replacements with fuzzy fallbacks.
type EditTool struct{}

func (EditTool) Name() string { return "edit" }

func (EditTool) Description() string {
	return `Performs exact string replacements in files.

Usage:
- You must use your Read tool at least once in the conversation before editing. This tool will error if you attempt an edit without reading the file.
- When editing text from Read tool output, ensure you preserve the exact indentation (tabs/spaces) as it appears AFTER the line number prefix. The line number prefix format is: line number + colon + space (e.g. '1: '). Everything after that is the actual file content to match. Never include any part of the line number prefix in the oldString or newString.
- ALWAYS prefer editing existing files in the codebase. NEVER write new files unless explicitly required.
- Only use emojis if the user explicitly requests it. Avoid adding emojis to files unless asked.
- The edit will FAIL if 'oldString' is not found in the file with an error "oldString not found in content".
- The edit will FAIL if 'oldString' is found multiple times in the file with an error "Found multiple matches for oldString. Provide more surrounding context to make the match unique." Either provide a larger string with more surrounding context to make it unique or use 'replaceAll' to change every instance of 'oldString'.
- Use 'replaceAll' for replacing and renaming strings across the file. This parameter is useful if you want to rename a variable for instance.`
}

func (EditTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"filePath":   map[string]any{"type": "string", "description": "The absolute path to the file to modify"},
			"oldString":  map[string]any{"type": "string", "description": "The text to replace"},
			"newString":  map[string]any{"type": "string", "description": "The text to replace it with (must be different from oldString)"},
			"replaceAll": map[string]any{"type": "boolean", "description": "Replace all occurrences of oldString (default false)"},
		},
		"required": []any{"filePath", "oldString", "newString"},
	}
}

func detectEOL(s string) string {
	if strings.Contains(s, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

func applyEdit(content, oldStr, newStr string, replaceAll bool) (string, error) {
	if oldStr == newStr {
		return "", errors.New("No changes to apply: oldString and newString are identical.")
	}
	if oldStr == "" {
		return "", errors.New("oldString cannot be empty when editing an existing file. Provide the exact text to replace.")
	}
	// 1. simple exact match
	if idx := strings.Index(content, oldStr); idx >= 0 {
		if !replaceAll && strings.Index(content[idx+1:], oldStr) >= 0 {
			return "", errors.New("Found multiple matches for oldString. Provide more surrounding context to make the match unique.")
		}
		if replaceAll {
			return strings.ReplaceAll(content, oldStr, newStr), nil
		}
		return content[:idx] + newStr + content[idx+len(oldStr):], nil
	}
	// 2. line-trimmed matching
	oldLines := strings.Split(oldStr, "\n")
	newLines := strings.Split(newStr, "\n")
	contentLines := strings.Split(content, "\n")
	matches := 0
	result := make([]string, 0, len(contentLines))
	i := 0
	for i < len(contentLines) {
		if i+len(oldLines) <= len(contentLines) && linesTrimmedEqual(contentLines[i:i+len(oldLines)], oldLines) {
			matches++
			if replaceAll {
				result = append(result, newLines...)
				i += len(oldLines)
				continue
			}
			result = append(result, contentLines[i:i+len(oldLines)]...)
			i++
			continue
		}
		result = append(result, contentLines[i])
		i++
	}
	if matches == 0 {
		return "", errors.New("Could not find oldString in the file. It must match exactly, including whitespace, indentation, and line endings.")
	}
	if !replaceAll && matches > 1 {
		return "", errors.New("Found multiple matches for oldString. Provide more surrounding context to make the match unique.")
	}
	return strings.Join(result, "\n"), nil
}

func linesTrimmedEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimSpace(a[i]) != strings.TrimSpace(b[i]) {
			return false
		}
	}
	return true
}

func (EditTool) Run(c *Ctx, args json.RawMessage) (Result, error) {
	return applyEditTool(c, args, false)
}

func applyEditTool(c *Ctx, args json.RawMessage, isWrite bool) (Result, error) {
	var p struct {
		FilePath   string `json:"filePath"`
		OldString  string `json:"oldString"`
		NewString  string `json:"newString"`
		Content    string `json:"content"`
		ReplaceAll bool   `json:"replaceAll"`
	}
	_ = json.Unmarshal(args, &p)
	if p.FilePath == "" {
		return Result{}, errors.New("filePath is required")
	}
	fp := absPath(c, p.FilePath)
	title := relPattern(c, fp)

	var oldContent string
	exists := true
	fi, err := os.Stat(fp)
	if err == nil && fi.IsDir() {
		return Result{}, fmt.Errorf("Path is a directory, not a file: %s", fp)
	}
	if err == nil {
		b, err := os.ReadFile(fp)
		if err != nil {
			return Result{}, err
		}
		oldContent = string(b)
	} else {
		exists = false
	}

	var newContent string
	if isWrite {
		newContent = p.Content
		if exists && oldContent == newContent {
			return Result{}, errors.New("No changes to apply: content is identical to existing file.")
		}
	} else {
		if !exists {
			return Result{}, fmt.Errorf("File %s not found", fp)
		}
		eol := detectEOL(oldContent)
		normalized := strings.ReplaceAll(oldContent, "\r\n", "\n")
		updated, err := applyEdit(normalized, strings.ReplaceAll(p.OldString, "\r\n", "\n"), strings.ReplaceAll(p.NewString, "\r\n", "\n"), p.ReplaceAll)
		if err != nil {
			return Result{}, err
		}
		if eol == "\r\n" {
			updated = strings.ReplaceAll(updated, "\n", "\r\n")
		}
		newContent = updated
	}

	patch, additions, deletions := diff.Patch(title, strings.ReplaceAll(oldContent, "\r\n", "\n"), strings.ReplaceAll(newContent, "\r\n", "\n"))

	if c.Ask != nil {
		if err := c.Ask(AskInput{
			Permission: "edit",
			Patterns:   []string{title},
			Always:     []string{"*"},
			Metadata:   map[string]any{"filepath": fp, "diff": patch},
		}); err != nil {
			return Result{}, err
		}
	}

	if c.Backup != nil {
		_ = c.Backup(fp)
	}
	if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(fp, []byte(newContent), 0o644); err != nil {
		return Result{}, err
	}

	filediff := session.FileDiff{File: fp, Patch: patch, Additions: additions, Deletions: deletions}
	if exists {
		filediff.Status = "modified"
	} else {
		filediff.Status = "added"
	}
	md := map[string]any{"diff": patch, "filediff": filediff}
	if isWrite {
		md["filepath"] = fp
		md["exists"] = exists
	}
	out := "Edit applied successfully."
	if isWrite {
		out = "Wrote file successfully."
	}
	return Result{Title: title, Metadata: md, Output: out}, nil
}

type WriteTool struct{}

func (WriteTool) Name() string { return "write" }

func (WriteTool) Description() string {
	return `Writes a file to the local filesystem.

Usage:
- This tool will overwrite the existing file if there is one at the provided path.
- If this is an existing file, you MUST use the Read tool first to read the file's contents. This tool will fail if you did not read the file first.
- ALWAYS prefer editing existing files in the codebase. NEVER write new files unless explicitly required.
- NEVER proactively create documentation files (*.md) or README files. Only create documentation files if explicitly requested by the User.
- Only use emojis if the user explicitly requests it. Avoid writing emojis to files unless asked.`
}

func (WriteTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content":  map[string]any{"type": "string", "description": "The content to write to the file"},
			"filePath": map[string]any{"type": "string", "description": "The absolute path to the file to write (must be absolute, not relative)"},
		},
		"required": []any{"content", "filePath"},
	}
}

func (WriteTool) Run(c *Ctx, args json.RawMessage) (Result, error) {
	return applyEditTool(c, args, true)
}

var _ = util.Now
