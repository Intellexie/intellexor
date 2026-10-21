//go:build desktop

// Desktop mode: the multiplexed server (web UI/API + ACP on one socket)
// keeps running exactly as before — the HTTP listener still serves external
// clients (browsers, ACP, tools) — and the web UI is additionally shown in
// a native webview window via Wails.
//
// The webview cannot rely on HTTP through the scheme handler, so it gets
// two in-process channels to the SAME handler:
//   - API calls: the bound Api struct (window.go.main.Api.Call) routes
//     requests straight through srv.Handler() with no network hop.
//   - events: srv.OnEvent forwards the event bus to the webview via
//     runtime.EventsEmit ("server-event"), replacing the SSE endpoints.
//
// Build (requires the platform's GUI dev headers, e.g. libgtk-3-dev and
// libwebkit2gtk-4.1-dev on Linux):
//
//	go build -tags "desktop,production,webkit2_41" -o intellexor .
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/Intellexie/intellexor/internal/server"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// ApiResponse mirrors an HTTP reply: status code plus raw body, parsed by
// the frontend's api() wrapper exactly like a fetch response.
type ApiResponse struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// Api is bound into the webview as window.go.main.Api.
type Api struct{ srv *server.Server }

// Call routes one API request to the server handler in-process. path
// includes the query string (e.g. "/session?directory=/tmp"); body is the
// raw JSON request body ("" for none); timezone feeds the X-Timezone
// header, like the web UI's fetch wrapper does.
func (a *Api) Call(method, path, body, timezone string) (*ApiResponse, error) {
	if method == "" {
		method = http.MethodGet
	}
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://intellexor.wails"+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if timezone != "" {
		req.Header.Set("X-Timezone", timezone)
	}
	w := httptest.NewRecorder()
	a.srv.Handler().ServeHTTP(w, req)
	return &ApiResponse{Status: w.Code, Body: w.Body.String()}, nil
}

func runDesktop(env desktopEnv) {
	// Serve web UI/API + ACP in the background for external clients; the
	// window itself reaches the same handler in-process (Api above).
	go serveForever(env.srv, env.wd, env.ln)

	// The bound bridge: the webview's API layer (window.go.main.Api).
	api := &Api{srv: env.srv}

	// Assets is nil, so every request the webview makes over the scheme is
	// forwarded to the server handler: static UI and runtime injection
	// alike. API calls and events bypass it entirely (Api + EventsEmit).
	err := wails.Run(&options.App{
		Title:     "Intellexor",
		Width:     1280,
		Height:    800,
		MinWidth:  900,
		MinHeight: 600,
		AssetServer: &assetserver.Options{
			Handler: env.srv.Handler(),
		},
		Bind: []interface{}{api},
		OnStartup: func(ctx context.Context) {
			// Push the event stream into the webview: SSE through the
			// scheme handler is not viable, so events travel over the
			// Wails events runtime instead.
			env.srv.OnEvent(func(directory string, ev any) {
				wailsruntime.EventsEmit(ctx, "server-event", map[string]any{
					"directory": directory,
					"payload":   ev,
				})
			})
		},
		BackgroundColour: options.NewRGB(0x08, 0x08, 0x08),
	})
	if err != nil {
		// GUI could not start (no display, stripped build): keep the
		// server running headless-style instead of dying.
		fmt.Fprintln(os.Stderr, "desktop error:", err, "— continuing headless")
		select {} // block forever; the goroutine above keeps serving
	}

	// Window closed: shut the server down and exit.
	env.ln.Close()
	os.Exit(0)
}
