package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type WebFetchTool struct{}

func (WebFetchTool) Name() string { return "webfetch" }

func (WebFetchTool) Description() string {
	return `- Fetches content from a specified URL
- Takes a URL and optional format as input
- Fetches the URL content, converts to requested format (markdown by default)
- Returns the content in the specified format
- Use this tool when you need to retrieve and analyze web content

Usage notes:
  - IMPORTANT: if another tool is present that offers better web fetching capabilities, is more targeted to the task, or has fewer restrictions, prefer using that tool instead of this one.
  - The URL must be a fully-formed valid URL
  - HTTP URLs will be automatically upgraded to HTTPS
  - Format options: "markdown" (default), "text", or "html"
  - This tool is read-only and does not modify any files
  - Results may be summarized if the content is very large`
}

func (WebFetchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url":     map[string]any{"type": "string", "description": "The URL to fetch content from"},
			"format":  map[string]any{"type": "string", "enum": []any{"text", "markdown", "html"}, "description": "The format to return the content in (text, markdown, or html). Defaults to markdown.", "default": "markdown"},
			"timeout": map[string]any{"type": "number", "description": "Optional timeout in seconds (max 120)"},
		},
		"required": []any{"url"},
	}
}

var reScript = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
var reTags = regexp.MustCompile(`(?s)<[^>]+>`)
var reBlank = regexp.MustCompile(`\n{3,}`)

func htmlToText(html string) string {
	s := reScript.ReplaceAllString(html, "")
	s = reTags.ReplaceAllString(s, "")
	s = unescapeHTML(s)
	s = reBlank.ReplaceAllString(strings.TrimSpace(s)+"\n", "\n\n")
	return strings.TrimSpace(s)
}

func htmlToMarkdown(html string) string {
	s := reScript.ReplaceAllString(html, "")
	// headings
	for i := 1; i <= 6; i++ {
		re := regexp.MustCompile(fmt.Sprintf(`(?is)<h%d[^>]*>(.*?)</h%d>`, i, i))
		s = re.ReplaceAllString(s, "\n\n"+strings.Repeat("#", i)+" $1\n\n")
	}
	reLi := regexp.MustCompile(`(?is)<li[^>]*>(.*?)</li>`)
	s = reLi.ReplaceAllString(s, "\n- $1")
	reP := regexp.MustCompile(`(?is)</p>|<br\s*/?>`)
	s = reP.ReplaceAllString(s, "\n\n")
	reCode := regexp.MustCompile(`(?is)<(code|pre)[^>]*>(.*?)</(code|pre)>`)
	s = reCode.ReplaceAllString(s, "\n```\n$2\n```\n")
	reLink := regexp.MustCompile(`(?is)<a[^>]+href="([^"]*)"[^>]*>(.*?)</a>`)
	s = reLink.ReplaceAllString(s, "[$2]($1)")
	reBold := regexp.MustCompile(`(?is)<(strong|b)>(.*?)</(strong|b)>`)
	s = reBold.ReplaceAllString(s, "**$2**")
	reEm := regexp.MustCompile(`(?is)<(em|i)>(.*?)</(em|i)>`)
	s = reEm.ReplaceAllString(s, "*$2*")
	s = reTags.ReplaceAllString(s, "")
	s = unescapeHTML(s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = strings.Join(lines, "\n")
	s = reBlank.ReplaceAllString(strings.TrimSpace(s)+"\n", "\n\n")
	return strings.TrimSpace(s)
}

var entities = map[string]string{"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": `"`, "&#39;": "'", "&apos;": "'", "&nbsp;": " ", "&mdash;": "—", "&ndash;": "–", "&hellip;": "…", "&rsquo;": "’", "&lsquo;": "‘", "&ldquo;": `“`, "&rdquo;": `”`}

func unescapeHTML(s string) string {
	re := regexp.MustCompile(`&(#x?[0-9a-fA-F]+|[a-zA-Z]+);`)
	return re.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := entities[m]; ok {
			return v
		}
		return m
	})
}

func (WebFetchTool) Run(c *Ctx, args json.RawMessage) (Result, error) {
	var p struct {
		URL     string `json:"url"`
		Format  string `json:"format"`
		Timeout int64  `json:"timeout"`
	}
	_ = json.Unmarshal(args, &p)
	if p.URL == "" {
		return Result{}, fmt.Errorf("url is required")
	}
	if p.Format == "" {
		p.Format = "markdown"
	}
	timeout := p.Timeout * 1000
	if timeout == 0 {
		timeout = 30_000
	}
	if timeout > 120_000 {
		timeout = 120_000
	}
	if c.Ask != nil {
		if err := c.Ask(AskInput{Permission: "webfetch", Patterns: []string{p.URL}, Always: []string{"*"}, Metadata: map[string]any{"url": p.URL, "format": p.Format}}); err != nil {
			return Result{}, err
		}
	}
	ctx, cancel := context.WithTimeout(c.Ctx, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", p.URL, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	const maxSize = 5 * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return Result{}, err
	}
	if len(body) > maxSize {
		return Result{}, fmt.Errorf("Response too large (exceeds 5MB limit)")
	}
	ct := resp.Header.Get("Content-Type")
	base := ct
	if i := strings.Index(base, ";"); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	var out string
	switch {
	case strings.HasPrefix(base, "image/"):
		return Result{Title: fmt.Sprintf("%s (%s)", p.URL, base), Metadata: map[string]any{}, Output: "Image fetched successfully"}, nil
	case strings.Contains(base, "json") || strings.Contains(base, "text/plain") || strings.Contains(base, "xml"):
		out = string(body)
		if p.Format == "text" {
			out = strings.TrimSpace(out)
		}
	default:
		if p.Format == "html" {
			out = string(body)
		} else if p.Format == "text" {
			out = htmlToText(string(body))
		} else {
			out = htmlToMarkdown(string(body))
		}
	}
	return Result{Title: fmt.Sprintf("%s (%s)", p.URL, base), Metadata: map[string]any{}, Output: out}, nil
}
