# AGENTS.md

Intellexor: a single-binary AI coding agent server in Go (HTTP API + embedded web UI + ACP JSON-RPC). `README.md` documents the full HTTP API, config format, agents, and Docker setup — this file covers what the README doesn't make obvious.

## Commands

```sh
 go build -o bin/intellexor .                        # bin/ is gitignored
 go test ./...                                      # ~15s; only internal/app has tests
 go test ./internal/app -run TestAbortThenResend    # single test
 go vet ./... && gofmt -l .                         # the only checks — no CI, no linter config
```

Tests are fully local (httptest fake providers streaming SSE) — no network or services needed. The suite is slow because abort tests use real-time streaming delays, not mocks.

## Gotchas

- **The web UI is compiled into the binary.** `web/assets/*` is loaded via `go:embed` (web/embed.go); editing assets requires a rebuild. There is no dev server or hot reload.
- **Desktop mode is behind build tags.** The default `go build` produces a headless server (desktop code compiled out; running it without `--headless` just prints a note and serves). The desktop app (Wails webview + HTTP + ACP listeners) needs `go build -tags "desktop,production,webkit2_41"` plus the platform GUI headers (Linux: libgtk-3-dev, libwebkit2gtk-4.1-dev — noble ships no 4.0) — see `desktop.go` / `desktop_disabled.go`. `--headless`/`-h` forces plain-server behavior in any build. Wails (github.com/wailsapp/wails/v2) is the only external dependency; don't add more without strong justification.
- **THERE IS EXACTLY ONE APP.** Desktop vs headless is a *runtime argument* (`--headless`), never two build variants, two binaries, or two release artifact sets. Do not split the release pipeline into "server" and "desktop" jobs/flavors — one binary per platform, built with `-tags "desktop,production"`, serves both. (This was tried once and rightly shot down.) The only exception is the Docker image, which builds its own CGO-free headless binary because scratch containers cannot carry GTK/WebKit — that's a deployment form factor, not a second app. Note the released linux binary dynamically links libgtk/libwebkit (fine on desktop distros; headless server users belong in Docker). Darwin builds need `CGO_LDFLAGS="-framework UniformTypeIdentifiers"` — Wails' darwin frontend uses UTType but never links its framework.
- **Tests must isolate the data dir.** It is cached at package level, so new tests need `os.Setenv("INTELLEXOR_DATA", t.TempDir())` plus `store.ResetDataDirForTest()` — see the setup helpers in internal/app/abort_test.go.
- **Permissions are inert.** Everything is allowed by default; `permission.*` config and the `/permission` endpoints exist for opencode compatibility but have no effect. Don't write tests expecting them to block.
- **Port fallback:** when the default port 4096 is busy, the server silently binds a random free port (main.go:100) — the printed URL is the source of truth.
- **The version is a package-level var** (`session.Version` in internal/session/types.go), not a const: the release workflow (`.github/workflows/release.yml`) injects the git tag via `-ldflags "-X github.com/Intellexie/intellexor/internal/session.Version=<tag>"`. Unset, it falls back to the hardcoded default.
- `internal/engine/` is empty (vestigial).

## Architecture

- Entry: `main.go` (module root — installable via `go install github.com/Intellexie/intellexor@...`). There is no `acp` subcommand: **ACP is always on**, multiplexed with HTTP on the same socket (connections classified by the first byte) via `acp.Multiplex` (internal/acp). `--headless`/`-h` skips only the desktop window.
- `internal/server` creates an `app.Instance` per working directory and handles routing/SSE; `internal/acp` maps the same instance to JSON-RPC methods. Changes to agent behavior usually land in `internal/app` (prompt loop, processor) and surface through both.
- Config is JSONC, merged from global (`~/.config/intellexor/`) plus project files (`intellexor.json`/`opencode.json`) walked upward to the filesystem root (internal/config).
- Providers speak OpenAI-compatible SSE (`/chat/completions`); client, catalog, and fallbacks live in `internal/provider`.
- HTTP API follows opencode v1 shapes: errors as `{name, data}`, success as bare JSON.
- `internal/server/templates/*.txt` are the built-in command prompts (e.g. `/init`, `/review`).

## API documentation (`api_docs.md`)

`api_docs.md` (repo root) is the **complete client-facing API reference** — HTTP endpoints, SSE event catalog, ACP JSON-RPC surface, and all wire types — written for agents building UI clients against this server.

**MAINTAIN IT: every time the API changes, update `api_docs.md` in the same change.** Concretely, whenever a commit touches any of the following, check the doc for drift and fix it:

- `internal/server/` (routes_*.go, mux.go, server.go, workspace.go) — new/changed/removed endpoints, request/response shapes, error names
- `internal/session/types.go` — wire types (Session, Message, Part, ToolState, …)
- `internal/acp/` — JSON-RPC methods, `session/update` variants, error codes
- `internal/board/`, `internal/cron/`, `internal/alarm/` — data models and semantics
- `internal/bus/` + publish call sites in `internal/app/` — event types and their `properties` payloads
- `internal/config/`, `internal/auth/`, `internal/provider/` — config/auth/provider shapes

When adding an endpoint or event, add it to the relevant section **and** the §14 quick-reference table (events also go in the §11.3 catalog). Keep the timestamp-unit warnings (ms vs seconds) accurate. If unsure whether something changed the public surface, diff the route registrations in `internal/server/mux.go` and the `Hub.Publish` call sites against the doc.

You are runniong as the process "intellexor", if you kill it you will kill the session completely.
When the user asks you to "deploy it", commit eveything, increment the tag, point the "latest" tag to the latest commit and push.
Github PAT is available in GITHUB_PAT env-var. 
