# AGENTS.md

Intellexor: a single-binary AI coding agent server in Go (HTTP API + embedded web UI + ACP JSON-RPC). `README.md` documents the full HTTP API, config format, agents, and Docker setup — this file covers what the README doesn't make obvious.

## Commands

```sh
 go build -o bin/intellexor ./cmd/intellexor        # bin/ is gitignored
 go test ./...                                      # ~15s; only internal/app has tests
 go test ./internal/app -run TestAbortThenResend    # single test
 go vet ./... && gofmt -l .                         # the only checks — no CI, no linter config
```

Tests are fully local (httptest fake providers streaming SSE) — no network or services needed. The suite is slow because abort tests use real-time streaming delays, not mocks.

## Gotchas

- **The web UI is compiled into the binary.** `web/assets/*` is loaded via `go:embed` (web/embed.go); editing assets requires a rebuild. There is no dev server or hot reload.
- **Zero external Go dependencies** — `go.mod` has no requires, by design ("one binary, no external dependencies"). Use the stdlib; don't add third-party packages without strong justification.
- **Tests must isolate the data dir.** It is cached at package level, so new tests need `os.Setenv("INTELLEXOR_DATA", t.TempDir())` plus `store.ResetDataDirForTest()` — see the setup helpers in internal/app/abort_test.go.
- **Permissions are inert.** Everything is allowed by default; `permission.*` config and the `/permission` endpoints exist for opencode compatibility but have no effect. Don't write tests expecting them to block.
- **Port fallback:** when the default port 4096 is busy, the server silently binds a random free port (cmd/intellexor/main.go:100) — the printed URL is the source of truth.
- The version is a plain const (`session.Version` in internal/session/types.go), not injected via ldflags.
- `internal/engine/` is empty (vestigial).

## Architecture

- Entry: `cmd/intellexor/main.go`. Default mode serves HTTP; `intellexor acp` runs ACP over stdio, or TCP when `--port`/`ACP_PORT`/`PORT` is set — then **HTTP and ACP share one socket**, with connections classified by the first byte (internal/acp).
- `internal/server` creates an `app.Instance` per working directory and handles routing/SSE; `internal/acp` maps the same instance to JSON-RPC methods. Changes to agent behavior usually land in `internal/app` (prompt loop, processor) and surface through both.
- Config is JSONC, merged from global (`~/.config/intellexor/`) plus project files (`intellexor.json`/`opencode.json`) walked upward to the filesystem root (internal/config).
- Providers speak OpenAI-compatible SSE (`/chat/completions`); client, catalog, and fallbacks live in `internal/provider`.
- HTTP API follows opencode v1 shapes: errors as `{name, data}`, success as bare JSON.
- `internal/server/templates/*.txt` are the built-in command prompts (e.g. `/init`, `/review`).
