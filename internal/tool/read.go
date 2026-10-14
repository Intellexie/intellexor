package tool

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Intellexie/intellexor/internal/session"
	"github.com/Intellexie/intellexor/internal/util"
)

const maxLineLength = 2000
const maxReadBytes = 50 * 1024
const defaultReadLimit = 2000
const sampleBytes = 4096

type ReadTool struct{}

func (ReadTool) Name() string { return "read" }

func (ReadTool) Description() string {
	return `Read a file or directory from the local filesystem. If the path does not exist, an error is returned.

Usage:
- The filePath parameter should be an absolute path.
- By default, this tool returns up to 2000 lines from the start of the file.
- The offset parameter is the line number to start from (1-indexed).
- To read later sections, call this tool again with a larger offset.
- Use the grep tool to find specific content in large files or files with long lines.
- If you are unsure of the correct file path, use the glob tool to look up filenames by glob pattern.
- Contents are returned with each line prefixed by its line number as 'N: content'.
- Binary files and attachments are rejected. This tool cannot read image files; use a dedicated approach for those.`
}

func (ReadTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"filePath": map[string]any{"type": "string", "description": "The absolute path to the file or directory to read"},
			"offset":   map[string]any{"type": "number", "description": "The line number to start reading from (1-indexed)"},
			"limit":    map[string]any{"type": "number", "description": "The maximum number of lines to read (defaults to 2000)"},
		},
		"required": []any{"filePath"},
	}
}

var imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".svg": true}
var binaryExts = map[string]bool{".zip": true, ".tar": true, ".gz": true, ".exe": true, ".dll": true, ".so": true, ".class": true, ".jar": true, ".war": true, ".7z": true, ".pdf": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true, ".ppt": true, ".pptx": true, ".bin": true, ".dat": true, ".obj": true, ".o": true, ".a": true, ".lib": true, ".wasm": true, ".pyc": true, ".pyo": true}

func imageMime(ext string) string {
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	}
	return ""
}

func isBinary(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	sample := b
	if len(sample) > sampleBytes {
		sample = sample[:sampleBytes]
	}
	nb := 0
	for _, c := range sample {
		if c == 0 {
			return true
		}
		if c < 9 || (c > 13 && c < 32) || c == 127 {
			nb++
		}
	}
	return float64(nb)/float64(len(sample)) > 0.3
}

func absPath(c *Ctx, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(c.Directory, p))
}

func relPattern(c *Ctx, abs string) string {
	rel, err := filepath.Rel(c.Worktree, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return abs
	}
	return filepath.ToSlash(rel)
}

func (ReadTool) Run(c *Ctx, args json.RawMessage) (Result, error) {
	var p struct {
		FilePath string   `json:"filePath"`
		Offset   *float64 `json:"offset"`
		Limit    *float64 `json:"limit"`
	}
	_ = json.Unmarshal(args, &p)
	if p.FilePath == "" {
		return Result{}, errors.New("filePath is required")
	}
	fp := absPath(c, p.FilePath)
	offset := 1
	if p.Offset != nil && *p.Offset >= 1 {
		offset = int(*p.Offset)
	}
	limit := defaultReadLimit
	if p.Limit != nil && *p.Limit >= 1 {
		limit = int(*p.Limit)
	}

	if c.Ask != nil {
		if err := c.Ask(AskInput{Permission: "read", Patterns: []string{relPattern(c, fp)}, Always: []string{"*"}, Metadata: map[string]any{}}); err != nil {
			return Result{}, err
		}
	}

	fi, err := os.Stat(fp)
	if err != nil {
		return Result{}, fmt.Errorf("File not found: %s", fp)
	}
	title := relPattern(c, fp)

	if fi.IsDir() {
		entries, err := os.ReadDir(fp)
		if err != nil {
			return Result{}, err
		}
		var lines []string
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			} else if e.Type()&os.ModeSymlink != 0 {
				if st, err := os.Stat(filepath.Join(fp, e.Name())); err == nil && st.IsDir() {
					name += "/"
				}
			}
			lines = append(lines, name)
		}
		sort.Strings(lines)
		total := len(lines)
		if offset-1 > total {
			return Result{}, fmt.Errorf("Offset %d is out of range for this directory (%d entries)", offset, total)
		}
		view := lines[offset-1:]
		if len(view) > limit {
			view = view[:limit]
		}
		var sb strings.Builder
		sb.WriteString("<path>" + fp + "</path>\n<type>directory</type>\n<entries>\n")
		sb.WriteString(strings.Join(view, "\n"))
		if len(view) == 0 {
			sb.WriteString("(empty)")
		}
		sb.WriteString("\n")
		if offset-1+len(view) < total {
			sb.WriteString(fmt.Sprintf("\n(Showing %d of %d entries. Use 'offset' parameter to read beyond entry %d)", len(view), total, offset-1+len(view)))
		} else {
			sb.WriteString(fmt.Sprintf("\n(%d entries)", total))
		}
		sb.WriteString("\n</entries>")
		return Result{Title: title, Metadata: map[string]any{"truncated": len(view) < total, "display": map[string]any{"type": "directory", "path": fp, "offset": offset, "totalEntries": total}}, Output: sb.String()}, nil
	}

	ext := strings.ToLower(filepath.Ext(fp))
	if binaryExts[ext] && ext != ".pdf" {
		return Result{}, fmt.Errorf("Cannot read binary file: %s", fp)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		return Result{}, err
	}

	// images become attachments
	if mime := imageMime(ext); mime != "" {
		return Result{
			Title:       title,
			Metadata:    map[string]any{"display": map[string]any{"type": "file", "path": fp}},
			Output:      "Image read successfully",
			Attachments: attachmentParts(c, mime, raw, filepath.Base(fp)),
		}, nil
	}
	if ext == ".pdf" {
		return Result{
			Title:       title,
			Metadata:    map[string]any{"display": map[string]any{"type": "file", "path": fp}},
			Output:      "PDF read successfully",
			Attachments: attachmentParts(c, "application/pdf", raw, filepath.Base(fp)),
		}, nil
	}
	if isBinary(raw) {
		return Result{}, fmt.Errorf("Cannot read binary file: %s", fp)
	}

	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	if offset > total+1 || offset < 1 {
		return Result{}, fmt.Errorf("Offset %d is out of range for this file (%d lines)", offset, total)
	}
	if offset == 0 {
		offset = 1
	}

	var sb strings.Builder
	sb.WriteString("<path>" + fp + "</path>\n<type>file</type>\n<content>\n")
	byteCount := 0
	end := offset - 1
	capped := false
	for i := offset - 1; i < total && i < offset-1+limit; i++ {
		line := lines[i]
		suffix := ""
		if len(line) > maxLineLength {
			suffix = fmt.Sprintf("...%d more chars in line", len(line)-maxLineLength)
			line = line[:maxLineLength]
		}
		chunk := fmt.Sprintf("%d: %s%s\n", i+1, line, suffix)
		if byteCount+len(chunk) > maxReadBytes {
			capped = true
			break
		}
		byteCount += len(chunk)
		end = i + 1
		sb.WriteString(chunk)
	}
	switch {
	case capped:
		sb.WriteString(fmt.Sprintf("\n(Output capped at 50 KB. Showing lines %d-%d. Use offset=%d to continue.)\n", offset, end, end+1))
	case end < total:
		sb.WriteString(fmt.Sprintf("\n(Showing lines %d-%d of %d. Use offset=%d to continue.)\n", offset, end, total, end+1))
	default:
		sb.WriteString(fmt.Sprintf("\n(End of file - total %d lines)\n", total))
	}
	sb.WriteString("</content>")

	previewLines := strings.Split(sb.String(), "\n")
	if len(previewLines) > 20 {
		previewLines = previewLines[:20]
	}
	meta := map[string]any{
		"truncated": capped,
		"display": map[string]any{
			"type": "file", "path": fp, "lineStart": offset, "lineEnd": end, "totalLines": total, "truncated": capped,
		},
	}
	_ = previewLines
	return Result{Title: title, Metadata: meta, Output: sb.String()}, nil
}

func attachmentParts(c *Ctx, mime string, data []byte, filename string) []session.Part {
	return []session.Part{{
		ID: util.ID("prt"), SessionID: c.SessionID, MessageID: c.MessageID, Type: "file",
		Mime: mime, URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), Filename: filename,
	}}
}
