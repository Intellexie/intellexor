package util

import (
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// MimeOfFile returns the mime type of the file at path: the extension map
// first, content sniffing of the first 512 bytes as a fallback.
func MimeOfFile(path string) string {
	if t := mime.TypeByExtension(filepath.Ext(path)); t != "" {
		return t
	}
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := f.Read(head)
	return http.DetectContentType(head[:n])
}

var textLikeMimes = map[string]bool{
	"application/json":         true,
	"application/xml":          true,
	"application/javascript":   true,
	"application/x-javascript": true,
	"application/yaml":         true,
	"application/x-yaml":       true,
	"application/toml":         true,
	"application/x-sh":         true,
	"application/sql":          true,
}

// AttachmentKind classifies a mime type for preview purposes: "image"
// (inline <img> preview), "text" (fetched and shown as color-coded code) or
// "download" (no preview). text/html is deliberately a download — serving
// it inline from the app origin would be an XSS vector.
func AttachmentKind(mimeType string) string {
	m := strings.ToLower(strings.TrimSpace(mimeType))
	if i := strings.Index(m, ";"); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	switch {
	case strings.HasPrefix(m, "image/"):
		return "image"
	case m == "text/html" || m == "application/xhtml+xml":
		return "download"
	case strings.HasPrefix(m, "text/"):
		return "text"
	case strings.HasSuffix(m, "+json"), strings.HasSuffix(m, "+xml"):
		return "text"
	case textLikeMimes[m]:
		return "text"
	default:
		return "download"
	}
}
