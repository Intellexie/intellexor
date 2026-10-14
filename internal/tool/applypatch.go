package tool

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Intellexie/intellexor/internal/diff"
)

type ApplyPatchTool struct{}

func (ApplyPatchTool) Name() string { return "apply_patch" }

func (ApplyPatchTool) Description() string {
	return `Use the apply_patch tool to edit files (it's for coding specifically). Patch format:

*** Begin Patch
*** Add File: path/to/file
+added contents
*** Update File: path/to/file
@@
-context
-removed
+added
*** Delete File: path/to/file
*** End Patch`
}

func (ApplyPatchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"patchText": map[string]any{"type": "string", "description": "The full patch text that describes all changes to be made"},
		},
		"required": []any{"patchText"},
	}
}

type patchFile struct {
	kind     string // add | update | delete | move
	path     string
	moveTo   string
	addLines []string
	delLines []string
	chunks   [][]string // update ops rows: kind + text
}

func parsePatch(patch string) ([]patchFile, error) {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(patch, "\r\n", "\n"), "\n"), "\n")
	var files []patchFile
	i := 0
	inPatch := false
	for i < len(lines) {
		l := lines[i]
		switch {
		case strings.HasPrefix(l, "*** Begin Patch"):
			inPatch = true
			i++
		case strings.HasPrefix(l, "*** End Patch"):
			i++
		case strings.HasPrefix(l, "*** Add File: "):
			path := strings.TrimPrefix(l, "*** Add File: ")
			i++
			pf := patchFile{kind: "add", path: path}
			for i < len(lines) && !strings.HasPrefix(lines[i], "***") {
				if strings.HasPrefix(lines[i], "+") {
					pf.addLines = append(pf.addLines, lines[i][1:])
				}
				i++
			}
			files = append(files, pf)
		case strings.HasPrefix(l, "*** Delete File: "):
			path := strings.TrimPrefix(l, "*** Delete File: ")
			i++
			files = append(files, patchFile{kind: "delete", path: path, delLines: nil})
		case strings.HasPrefix(l, "*** Update File: "):
			path := strings.TrimPrefix(l, "*** Update File: ")
			pf := patchFile{kind: "update", path: path}
			i++
			for i < len(lines) && strings.HasPrefix(lines[i], "*** Move to: ") {
				pf.moveTo = strings.TrimPrefix(lines[i], "*** Move to: ")
				i++
			}
			for i < len(lines) && !strings.HasPrefix(lines[i], "***") {
				row := lines[i]
				if row == "@@" || strings.HasPrefix(row, "@@") {
					i++
					continue
				}
				if len(row) > 0 && (row[0] == '+' || row[0] == '-' || row[0] == ' ') {
					pf.chunks = append(pf.chunks, []string{string(row[0]), row[1:]})
				}
				i++
			}
			files = append(files, pf)
		default:
			i++
		}
	}
	if !inPatch && len(files) == 0 {
		return nil, errors.New("apply_patch verification failed: no patch envelope found")
	}
	if len(files) == 0 {
		return nil, errors.New("patch rejected: empty patch")
	}
	return files, nil
}

func applyUpdate(content string, chunks [][]string) (string, error) {
	cur := strings.Split(content, "\n")
	pos := 0
	for _, ch := range chunks {
		kind, text := ch[0], ch[1]
		switch kind {
		case " ":
			// context: advance, tolerant
			if pos < len(cur) && cur[pos] == text {
				pos++
			} else {
				found := -1
				for i := pos; i < len(cur) && i < pos+32; i++ {
					if cur[i] == text {
						found = i
						break
					}
				}
				if found >= 0 {
					pos = found + 1
				}
			}
		case "-":
			if pos < len(cur) && cur[pos] == text {
				cur = append(cur[:pos], cur[pos+1:]...)
			} else {
				found := -1
				for i := 0; i < len(cur); i++ {
					if cur[i] == text {
						found = i
						break
					}
				}
				if found == -1 {
					return "", fmt.Errorf("apply_patch verification failed: could not apply chunk. Expected to find '%s'", text)
				}
				cur = append(cur[:found], cur[found+1:]...)
				pos = found
			}
		case "+":
			cur = append(cur[:pos], append([]string{text}, cur[:pos]...)...)
			pos++
		}
	}
	return strings.Join(cur, "\n"), nil
}

func (ApplyPatchTool) Run(c *Ctx, args json.RawMessage) (Result, error) {
	var p struct {
		PatchText string `json:"patchText"`
	}
	_ = json.Unmarshal(args, &p)
	if p.PatchText == "" {
		return Result{}, errors.New("patchText is required")
	}
	files, err := parsePatch(p.PatchText)
	if err != nil {
		return Result{}, err
	}

	var patterns []string
	for _, f := range files {
		patterns = append(patterns, filepath.ToSlash(f.path))
	}

	if c.Ask != nil {
		if err := c.Ask(AskInput{Permission: "edit", Patterns: patterns, Always: []string{"*"}, Metadata: map[string]any{"files": patterns}}); err != nil {
			return Result{}, err
		}
	}

	var metas []map[string]any
	var totalDiff strings.Builder
	var summary []string
	backup := func(p string) {
		if c.Backup != nil {
			_ = c.Backup(p)
		}
	}

	for _, f := range files {
		abs := absPath(c, f.path)
		rel := relPattern(c, abs)
		switch f.kind {
		case "add":
			old := ""
			if b, err := os.ReadFile(abs); err == nil {
				old = string(b)
			}
			newContent := strings.Join(f.addLines, "\n")
			patch, add, del := diff.Patch(rel, old, newContent)
			backup(abs)
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return Result{}, err
			}
			if err := os.WriteFile(abs, []byte(newContent), 0o644); err != nil {
				return Result{}, err
			}
			totalDiff.WriteString(patch + "\n")
			metas = append(metas, map[string]any{"filePath": abs, "relativePath": rel, "type": "add", "patch": patch, "additions": add, "deletions": del})
			summary = append(summary, "A "+rel)
		case "update":
			b, err := os.ReadFile(abs)
			if err != nil {
				return Result{}, fmt.Errorf("Failed to read file to update: %s", f.path)
			}
			old := strings.ReplaceAll(string(b), "\r\n", "\n")
			newContent, err := applyUpdate(old, f.chunks)
			if err != nil {
				return Result{}, err
			}
			if f.moveTo != "" {
				newAbs := absPath(c, f.moveTo)
				newRel := relPattern(c, newAbs)
				backup(abs)
				backup(newAbs)
				if err := os.MkdirAll(filepath.Dir(newAbs), 0o755); err != nil {
					return Result{}, err
				}
				if err := os.WriteFile(newAbs, []byte(newContent), 0o644); err != nil {
					return Result{}, err
				}
				_ = os.Remove(abs)
				patch, add, del := diff.Patch(newRel, old, "")
				totalDiff.WriteString(patch + "\n")
				metas = append(metas, map[string]any{"filePath": abs, "relativePath": rel, "type": "move", "movePath": newAbs, "patch": patch, "additions": add, "deletions": del})
				summary = append(summary, "M "+newRel)
			} else {
				patch, add, del := diff.Patch(rel, old, newContent)
				backup(abs)
				if err := os.WriteFile(abs, []byte(newContent), 0o644); err != nil {
					return Result{}, err
				}
				totalDiff.WriteString(patch + "\n")
				metas = append(metas, map[string]any{"filePath": abs, "relativePath": rel, "type": "update", "patch": patch, "additions": add, "deletions": del})
				summary = append(summary, "M "+rel)
			}
		case "delete":
			patch, add, del := diff.Patch(rel, "", "")
			_ = patch
			b, _ := os.ReadFile(abs)
			patch2, a2, d2 := diff.Patch(rel, strings.ReplaceAll(string(b), "\r\n", "\n"), "")
			_ = add
			_ = del
			totalDiff.WriteString(patch2 + "\n")
			backup(abs)
			if err := os.Remove(abs); err != nil {
				return Result{}, err
			}
			metas = append(metas, map[string]any{"filePath": abs, "relativePath": rel, "type": "delete", "patch": patch2, "additions": a2, "deletions": d2})
			summary = append(summary, "D "+rel)
		}
	}

	out := "Success. Updated the following files:\n" + strings.Join(summary, "\n")
	return Result{
		Title:       out,
		Metadata:    map[string]any{"diff": strings.TrimSpace(totalDiff.String()), "files": metas},
		Output:      out,
		Attachments: nil,
	}, nil
}
