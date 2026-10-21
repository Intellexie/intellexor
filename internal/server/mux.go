package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Intellexie/intellexor/internal/util"
	"github.com/Intellexie/intellexor/web"
)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	handle := func(pattern string, fn func(w http.ResponseWriter, r *http.Request) *apiError) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			s.cors(w, r)
			if r.Method == "OPTIONS" {
				w.WriteHeader(204)
				return
			}
			if !s.authOK(r) {
				w.Header().Set("www-authenticate", `Basic realm="Secure Area"`)
				w.WriteHeader(401)
				return
			}
			if err := fn(w, r); err != nil {
				writeErr(w, err)
			}
		})
	}

	raw := func(pattern string, fn func(w http.ResponseWriter, r *http.Request) *apiError) {
		handle(pattern, fn)
	}
	_ = raw

	// global
	mux.HandleFunc("GET /global/health", func(w http.ResponseWriter, r *http.Request) {
		s.cors(w, r)
		if !s.authOK(r) {
			w.WriteHeader(401)
			return
		}
		writeJSON(w, 200, map[string]any{"healthy": true, "version": s.Version})
	})
	handle("GET /global/config", s.getConfig)
	handle("PATCH /global/config", s.patchConfig)
	handle("POST /global/dispose", s.okTrue)

	// events
	mux.HandleFunc("GET /event", s.handleEvents)
	mux.HandleFunc("GET /global/event", s.handleGlobalEvents)

	// sessions
	handle("GET /session", s.listSessions)
	handle("GET /session/status", s.sessionStatus)
	handle("POST /session", s.createSession)
	handle("GET /session/{sessionID}", s.getSession)
	handle("DELETE /session/{sessionID}", s.deleteSession)
	handle("PATCH /session/{sessionID}", s.patchSession)
	handle("GET /session/{sessionID}/children", s.sessionChildren)
	handle("GET /session/{sessionID}/todo", s.sessionTodo)
	handle("DELETE /session/{sessionID}/todo", s.sessionTodoDelete)
	handle("GET /session/{sessionID}/diff", s.sessionDiff)
	handle("GET /session/{sessionID}/message", s.sessionMessages)
	handle("DELETE /session/{sessionID}/message/{messageID}", s.deleteSessionMessage)
	handle("DELETE /session/{sessionID}/message/{messageID}/part/{partID}", s.deleteSessionPart)
	handle("POST /session/{sessionID}/message", s.sessionMessage)
	handle("POST /session/{sessionID}/prompt_async", s.sessionPromptAsync)
	handle("POST /session/{sessionID}/abort", s.abortSession)
	handle("POST /session/{sessionID}/shell", s.sessionShell)
	handle("POST /session/{sessionID}/command", s.sessionCommand)
	handle("POST /session/{sessionID}/summarize", s.sessionSummarize)
	handle("POST /session/{sessionID}/init", s.sessionInit)
	handle("POST /session/{sessionID}/fork", s.sessionFork)
	handle("GET /session/{sessionID}/export", s.exportSession)
	handle("POST /session/import", s.importSession)
	handle("POST /session/{sessionID}/revert", s.revertSession)
	handle("POST /session/{sessionID}/unrevert", s.unrevertSession)
	handle("POST /session/{sessionID}/share", s.shareSession)
	handle("DELETE /session/{sessionID}/share", s.shareSession)
	handle("POST /session/{sessionID}/permissions/{permissionID}", s.replySessionPermission)

	// permission / question
	handle("GET /permission", s.listPermission)
	handle("POST /permission/{requestID}/reply", s.replyPermission)
	handle("GET /question", s.listQuestions)
	handle("POST /question/{requestID}/reply", s.replyQuestion)
	handle("POST /question/{requestID}/reject", s.rejectQuestion)

	// kanban board (first tab in the web UI)
	handle("GET /board", s.getBoard)
	handle("POST /board/card", s.createBoardCard)
	handle("PATCH /board/card/{cardID}", s.patchBoardCard)
	handle("DELETE /board/card/{cardID}", s.deleteBoardCard)
	handle("GET /board/export", s.exportBoard)
	handle("POST /board/import", s.importBoard)

	// cron jobs (scheduled prompts that start fresh sessions)
	handle("GET /cron", s.listCron)
	handle("POST /cron", s.createCron)
	handle("GET /cron/preview", s.previewCron)
	handle("PATCH /cron/{cronID}", s.patchCron)
	handle("DELETE /cron/{cronID}", s.deleteCron)

	// alarms (one-shot session wake-ups set via the schedule_wakeup tool)
	handle("GET /alarm", s.listAlarms)
	handle("POST /alarm", s.createAlarm)
	handle("GET /alarm/session/{sessionID}", s.sessionAlarms)
	handle("POST /alarm/{alarmID}/trigger", s.triggerAlarm)
	handle("DELETE /alarm/{alarmID}", s.deleteAlarm)

	// provider / auth / config
	handle("GET /provider", func(w http.ResponseWriter, r *http.Request) *apiError { providersHandler(s)(w, r); return nil })
	handle("GET /provider/auth", func(w http.ResponseWriter, r *http.Request) *apiError { providerAuthHandler(s)(w, r); return nil })
	handle("PUT /auth/{providerID}", s.putAuth)
	handle("DELETE /auth/{providerID}", s.deleteAuth)
	handle("GET /config", s.getConfig)
	handle("PATCH /config", s.patchConfig)
	handle("GET /config/providers", s.getConfigProviders)
	handle("GET /config/files", s.configFiles)
	handle("GET /config/entries", s.configEntries)
	handle("PUT /config/provider/{providerID}", s.putConfigProvider)
	handle("DELETE /config/provider/{providerID}", s.deleteConfigProvider)
	handle("PUT /config/provider/{providerID}/model/{modelID}", s.putConfigProviderModel)
	handle("DELETE /config/provider/{providerID}/model/{modelID}", s.deleteConfigProviderModel)
	handle("PUT /config/agent/{agentID}", s.putConfigAgent)
	handle("DELETE /config/agent/{agentID}", s.deleteConfigAgent)
	handle("POST /provider/{providerID}/oauth/authorize", func(w http.ResponseWriter, r *http.Request) *apiError {
		writeJSON(w, 200, nil)
		return nil
	})
	handle("POST /provider/{providerID}/oauth/callback", s.okTrue)

	// workspaces (additional working directories with their own sessions/config)
	handle("GET /workspace", s.listWorkspaces)
	handle("POST /workspace", s.createWorkspace)
	handle("POST /workspace/rename", s.renameWorkspace)

	// misc
	handle("GET /path", func(w http.ResponseWriter, r *http.Request) *apiError { pathHandler(s)(w, r); return nil })
	handle("POST /instance/dispose", s.disposeInstance)
	handle("GET /vcs", s.vcsHandler)
	handle("GET /vcs/status", s.vcsStatus)
	handle("GET /vcs/diff", s.vcsDiff)
	mux.HandleFunc("GET /vcs/diff/raw", func(w http.ResponseWriter, r *http.Request) {
		if !s.authOK(r) {
			w.WriteHeader(401)
			return
		}
		s.vcsDiffRaw(w, r)
	})
	handle("GET /agent", s.listAgents)
	handle("GET /command", s.listCommands)
	handle("GET /mcp", s.listMCP)
	handle("GET /skill", func(w http.ResponseWriter, r *http.Request) *apiError {
		writeJSON(w, 200, []any{})
		return nil
	})
	handle("GET /lsp", func(w http.ResponseWriter, r *http.Request) *apiError {
		writeJSON(w, 200, []any{})
		return nil
	})
	handle("GET /formatter", func(w http.ResponseWriter, r *http.Request) *apiError {
		writeJSON(w, 200, []any{})
		return nil
	})
	handle("GET /file", s.fileHandler)
	handle("GET /file/content", s.fileContent)
	handle("GET /file/status", func(w http.ResponseWriter, r *http.Request) *apiError {
		writeJSON(w, 200, []any{})
		return nil
	})
	handle("POST /log", s.okTrue)
	handle("GET /doc", func(w http.ResponseWriter, r *http.Request) *apiError {
		writeJSON(w, 200, map[string]any{
			"openapi": "3.1.0",
			"info":    map[string]any{"title": "opencode", "version": "1.0.0"},
			"paths":   map[string]any{},
		})
		return nil
	})

	// tui stubs (accepted, no-op)
	for _, p := range []string{
		"POST /tui/append-prompt", "POST /tui/submit-prompt", "POST /tui/clear-prompt",
		"POST /tui/open-help", "POST /tui/open-sessions", "POST /tui/open-themes",
		"POST /tui/open-models", "POST /tui/execute-command", "POST /tui/show-toast",
		"POST /tui/publish", "POST /tui/select-session", "POST /tui/control/response",
	} {
		handle(p, s.okTrue)
	}
	handle("GET /tui/control/next", s.okTrue)

	// web UI fallback (registered last; ServeMux prefers exact patterns)
	mux.HandleFunc("/", s.serveWeb)
	return mux
}

func (s *Server) serveWeb(w http.ResponseWriter, r *http.Request) {
	s.cors(w, r)
	if r.Method != "GET" && r.Method != "HEAD" {
		http.NotFound(w, r)
		return
	}
	p := r.URL.Path
	if p == "/" || filepath.Ext(p) == "" {
		p = "/index.html"
	}
	f, err := web.FS().Open(strings.TrimPrefix(p, "/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		if p == "/" {
			http.ServeFileFS(w, r, web.FS(), "index.html")
			return
		}
		http.NotFound(w, r)
		return
	}
	switch filepath.Ext(p) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".js", ".mjs":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".wasm":
		w.Header().Set("Content-Type", "application/wasm")
		// embed.FS carries no modtime, so ServeContent sends no validators and
		// browsers would heuristically cache assets across binary rebuilds.
		// Force revalidation; the strong ETag keeps unchanged wasm at a 304.
		w.Header().Set("ETag", `"`+wasmETag()+`"`)
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, p, st.ModTime(), f.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	}))
}

var (
	wasmTagOnce sync.Once
	wasmTag     string
)

func wasmETag() string {
	wasmTagOnce.Do(func() {
		h := sha256.New()
		// Hash every vendored wasm (the onnxruntime binaries under stt/);
		// large but done once per process.
		for _, name := range []string{
			"stt/ort-wasm-simd-threaded.asyncify.wasm",
			"stt/ort-wasm-simd-threaded.wasm",
		} {
			f, err := web.FS().Open(name)
			if err != nil {
				wasmTag = "none"
				return
			}
			if _, err := io.Copy(h, f); err != nil {
				f.Close()
				wasmTag = "none"
				return
			}
			f.Close()
		}
		wasmTag = hex.EncodeToString(h.Sum(nil))
	})
	return wasmTag
}

func (s *Server) cors(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-opencode-directory, x-opencode-workspace")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.Header().Set("Vary", "Origin")
}

// ---- SSE ----

func sseWrite(w http.ResponseWriter, flusher http.Flusher, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: message\n")
	fmt.Fprintf(w, "data: %s\n\n", b)
	flusher.Flush()
}

func (s *Server) sse(w http.ResponseWriter, r *http.Request, directory string) {
	in := s.Instance(directory)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	ch := in.Hub.Subscribe()
	defer in.Hub.Unsubscribe(ch)

	sseWrite(w, flusher, map[string]any{"id": util.ID("evt"), "type": "server.connected", "properties": map[string]any{}})
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			sseWrite(w, flusher, map[string]any{"id": util.ID("evt"), "type": "server.heartbeat", "properties": map[string]any{}})
		case ev := <-ch:
			sseWrite(w, flusher, ev)
		}
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	s.cors(w, r)
	if !s.authOK(r) {
		w.Header().Set("www-authenticate", `Basic realm="Secure Area"`)
		w.WriteHeader(401)
		return
	}
	s.sse(w, r, directoryOf(r))
}

func (s *Server) handleGlobalEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := s.subscribeGlobal()
	defer s.unsubscribeGlobal(ch)

	sseWrite(w, flusher, map[string]any{"directory": s.DefaultDir, "payload": map[string]any{"id": util.ID("evt"), "type": "server.connected", "properties": map[string]any{}}})
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			sseWrite(w, flusher, map[string]any{"directory": s.DefaultDir, "payload": map[string]any{"id": util.ID("evt"), "type": "server.heartbeat", "properties": map[string]any{}}})
		case ge := <-ch:
			sseWrite(w, flusher, ge)
		}
	}
}

func (s *Server) InstanceDirs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for d := range s.instances {
		out = append(out, d)
	}
	return out
}
