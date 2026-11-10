package tool

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Intellexie/intellexor/internal/store"
	"github.com/Intellexie/intellexor/internal/util"
)

// AttachTool sends a file to the user: the file is copied into
// <DataDir>/attachments and served by GET /attachment/{name}, where the web
// UI renders it as an attachment (inline image preview, color-coded text
// preview or a download). Desktop mode works unchanged — the webview's
// scheme handler routes /attachment/* to the same http.Handler, and binary
// downloads go base64 through the Api bridge.
type AttachTool struct{}

func (AttachTool) Name() string { return "attach" }

func (AttachTool) Description() string {
	return `- Sends a file to the user: the file is copied into the agent's shared
  attachments directory and shows up in the chat as an attachment.
- Images render as an inline preview, text and code files as a color-coded
  preview, anything else as a regular download.
- Use this whenever the user asks you for a file: a report, a generated
  artifact, an export, a patch, a screenshot, a dataset, etc.
- The path must point to an existing regular file; relative paths resolve
  against the working directory.`
}

func (AttachTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Path of the file to send to the user"},
			"description": map[string]any{
				"type":        "string",
				"description": "Optional short description shown next to the attachment",
			},
		},
		"required": []any{"path"},
	}
}

// AttachmentsDir is where files sent via the attach tool are stored; the
// attachment HTTP route serves exclusively from here.
func AttachmentsDir() string { return filepath.Join(store.DataDir(), "attachments") }

func (AttachTool) Run(c *Ctx, args json.RawMessage) (Result, error) {
	var p struct {
		Path        string `json:"path"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(args, &p)
	if p.Path == "" {
		return Result{}, errors.New("path is required")
	}
	src := absPath(c, p.Path)
	fi, err := os.Stat(src)
	if err != nil {
		return Result{}, errors.New("attach: file not found: " + src)
	}
	if fi.IsDir() {
		return Result{}, errors.New("attach: path is a directory, not a file: " + src)
	}

	if err := os.MkdirAll(AttachmentsDir(), 0o755); err != nil {
		return Result{}, errors.New("attach: could not create attachments directory: " + err.Error())
	}
	stored, err := copyIntoAttachments(src)
	if err != nil {
		return Result{}, errors.New("attach: " + err.Error())
	}

	name := filepath.Base(src)
	mimeType := util.MimeOfFile(src)
	md := map[string]any{
		"attachment": map[string]any{
			"name":        name,
			"stored":      stored,
			"mime":        mimeType,
			"kind":        util.AttachmentKind(mimeType),
			"size":        fi.Size(),
			"url":         "/attachment/" + stored,
			"description": p.Description,
		},
	}
	title := "Sent " + name
	if p.Description != "" {
		title += ": " + p.Description
	}
	out := fmt.Sprintf("File sent to the user as an attachment: %s (%s, %d bytes).", name, mimeType, fi.Size())
	return Result{Title: title, Metadata: md, Output: out}, nil
}

// copyIntoAttachments copies src under a unique, collision-safe name
// (<8 random hex chars>_<basename>) and returns the stored file name. The
// random prefix is what GET /attachment/{name} strips to recover the
// original name for the Content-Disposition header.
func copyIntoAttachments(src string) (string, error) {
	base := filepath.Base(src)
	base = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 {
			return '_'
		}
		return r
	}, base)
	if base == "" || base == "." || base == ".." {
		base = "file"
	}
	for attempt := 0; attempt < 5; attempt++ {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		stored := hex.EncodeToString(b[:]) + "_" + base
		dst := filepath.Join(AttachmentsDir(), stored)
		if _, err := os.Stat(dst); err == nil {
			continue // name collision — draw a new prefix
		}
		in, err := os.Open(src)
		if err != nil {
			return "", err
		}
		out, err := os.Create(dst)
		if err != nil {
			in.Close()
			return "", err
		}
		if _, err := io.Copy(out, in); err != nil {
			in.Close()
			out.Close()
			return "", err
		}
		if err := out.Close(); err != nil {
			in.Close()
			return "", err
		}
		in.Close()
		return stored, nil
	}
	return "", errors.New("could not find a unique attachment name")
}
