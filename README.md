# Intellexor

Intellexor is an AI coding agent server written in Go. It pairs an HTTP API with a
built-in web UI and speaks the **Agent Client Protocol (ACP)**, so you can drive
it from the browser, from ACP-compatible desktop clients, or from plain HTTP.
One binary, no external dependencies: the agent loop, tools, storage and the
web UI are all compiled in.

Highlights:

- **Agent loop** with streaming, retries, cost/token accounting, session
  titles, subagents (`task`) and per-session diff tracking.
- **Built-in tools**: `bash`, `read`, `edit`, `write`, `apply_patch`, `grep`,
  `glob`, `task`, `todowrite`, `webfetch`, `question`, `board` (+ `invalid` for
  malformed calls) — with output truncation limits and argument validation.
- **Web UI**: single-page app (dark/light/system themes) with a session
  sidebar, streaming markdown timeline, collapsible tool rows with diff
  viewer, reasoning blocks, todo/permission/question docks, agent + model
  pickers, attachments, `!` shell mode, context-usage indicator, a
  client-side speech-to-text mic button and a kanban board tab — starting a
  card links it to a new session and the agent moves it to done (via the
  `board` tool) when the task is finished. The agent can also `watch` cards
  (it is messaged on behalf of the user when a watched card is marked done)
  and `start` cards itself, exactly like the board's Start button — the
  orchestrator automatically watches every ticket it starts and is pinged
  with a "Ticket X is done." message when its session finishes. The
  orchestrator chat beside the board is horizontally resizable (drag its
  right edge; double-click resets).
- **Cron jobs**: a dedicated tab where each job pairs a prompt with a
  schedule (cron expression, `@macros` or `@every <duration>`, server
  timezone); every run starts a fresh session prompted with the job's
  prompt. Jobs are managed by the backend (they survive browser refreshes
  and server restarts), can be titled/edited, paused (holding only the next
  run — a session already started keeps going) and resumed (re-armed without
  firing), deleted, and viewed (jumps to the latest active — or last
  finished — run's session; running jobs show an hourglass).
- **All permissions granted by default** — Intellexor never blocks on tool
  approval. `permission.*` config keys and the `/permission` endpoints exist
  for compatibility but have no effect.

## Build & run

```sh
go build -o bin/intellexor .
./bin/intellexor --dir /path/to/project --port 4096
# open http://127.0.0.1:4096
```

Flags: `--host` (default `127.0.0.1`), `--port` (default `4096`, falls back to
a random free port if busy), `--dir` (working directory, defaults to cwd),
`--quiet`.

## ACP mode

`Intellexor acp` exposes the agent over ACP — newline-delimited JSON-RPC 2.0.
With a port, **both protocols share the same socket**: browsers opening the
port get the web UI (HTTP), ACP clients get an ndjson JSON-RPC stream
(connections are classified by the first byte).

Methods: `initialize`, `session/new|load|list|prompt|cancel`,
`session/update` notifications, and `session/request_permission` callbacks.

```sh
./bin/intellexor acp                      # stdio (for ACP desktop clients)
./bin/intellexor acp --port 8080 --cwd /path/to/project   # web UI + ACP on :8080
```

Environment: `ACP_PORT` or `PORT` selects TCP mode, `ACP_HOST` overrides the
bind address (default `127.0.0.1`).

## Docker

```sh
docker build -t intellexor .
docker run -p 8080:8080 -v my-project:/workspace -v intellexor-data:/data intellexor
```

The container starts `Intellexor acp` on `0.0.0.0:$PORT` (default 8080) — the
browser UI and ACP clients share that one port. Data lives in `/data`, the
working project in `/workspace`. Provider config can be mounted at
`/data/.config/intellexor/intellexor.json` (or set `XDG_CONFIG_HOME`); `/app/intellexor.json`
is also read, so `-v "$PWD/intellexor.json:/app/intellexor.json"` just works.

The image is ubuntu-based and gives the agent a full toolchain: `bash`,
`curl`, `git`, `node`/`npm`, `go` (1.26.x at `/usr/local/go`), and
`playwright` with chromium (browsers in `/opt/ms-playwright`).

## Speech to text (mic button)

The composer has a microphone button: click, talk, click again — the whole
recording is transcribed once and appended to the input box (no real-time
partials). It runs **fully client-side**
(sherpa-onnx WASM in a Web Worker + NVIDIA Parakeet TDT 0.6B v3 int8,
multilingual); audio never leaves the browser. The ~640 MB model is
downloaded once with a visible progress bar and cached in the browser
(Cache Storage `intellexor-stt-v1`). Requires a secure context (`https://` or
`localhost`) for microphone access.

## Kanban board

The board is the first tab in the web UI: three columns (`open`,
`inprogress`, `done`) of cards stored per project in a single JSON file —
it survives restarts and is shared by every client connected to the same
project. Card IDs are plain numbers (`1`, `2`, …) so they are easy to
reference; cards can carry attachments (usually images) that are passed to
the session prompt when the card is started.

**Starting a card** (the Start button, or the agent's `board` tool with
action `start`) links the card to a fresh agent session, moves it to
`inprogress` and prompts the session with the card's title as its task.

**Agents update the cards they work on.** A session started from a card is
linked to it, so the agent can move *its own* card without passing an ID:
when the task is finished it calls the `board` tool with action `move`,
column `done` — the card is resolved automatically. Agents can also `add`
follow-up cards, `list` the board, and `watch` cards: a watcher session is
messaged on behalf of the user as soon as a watched card is marked done
(watching the card linked to your own session is rejected — you are
already working on it).

**Auto-triggering cards** — two ways a card starts without anyone clicking:

- **Links (chains):** `link` points a card at a *target* card; when the
  source card is moved to done, the target starts automatically in a fresh
  session (only cards still waiting in `open` can be targeted). This lets
  agents chain work: finish card 1 → card 2 starts by itself.
- **Schedules:** `schedule` sets a timed start on a card — pass `in`
  (`"30m"`, `"2h"`, `"1d"`) or `startAt` (unix milliseconds) and a
  background scheduler starts the card then, exactly like the Start
  button. Only open cards can be scheduled; `startAt 0` clears the
  schedule.

The whole board can be exported/imported as JSON (`GET /board/export`,
`POST /board/import?mode=replace|merge`).

## Cron jobs

A cron job pairs a prompt with a schedule; every run starts a **fresh
agent session** prompted with the job's prompt (each run has no memory of
previous runs, so prompts should be self-contained). Jobs live in a
dedicated tab and are managed by the backend — they survive browser
refreshes and server restarts.

Schedules are standard 5-field cron expressions (server timezone; `*`,
lists, ranges, steps and 3-letter month/day names are supported), the
macros `@hourly`/`@daily`/`@midnight`/`@weekly`/`@monthly`/`@yearly`, or
`@every <duration>` (e.g. `@every 30m`) firing relative to now.

Jobs can be titled/edited, paused (holding only the next run — a session
already started keeps going), resumed (re-armed without firing, even if
the schedule came due while paused), deleted, and viewed (jumps to the
latest run's session; running jobs show an hourglass). A background sweep
starts a session per due job and broadcasts `cron.updated`. The agent can
manage jobs itself via the `cron` tool (including `preview`, which
validates a spec and returns its next fire time).

## Alarms

The `schedule_wakeup` tool schedules a **one-shot wake-up for the agent's
own session**: at the given timestamp (unix milliseconds) the system
injects a user message (`alarm-triggered`, plus an optional note as
context) into the session, so the agent can continue itself. The typical
pattern for long-running background work: launch the job, schedule a
wake-up for when it should be done, end the turn — the wake-up brings the
agent back to check the result (instead of blocking on `sleep` or
polling). Wake-ups are persisted per project and survive restarts (a
wake-up that came due while the server was down fires right after
startup); `action: "list"` shows the session's scheduled wake-ups and
`action: "cancel"` disarms one. A background sweep checks for due
wake-ups every 5 seconds.

## Orchestrator agent

The `orchestrator` agent coordinates work instead of doing it: it manages
the kanban board, starts cards in fresh sessions, schedules cron jobs and
schedules wake-ups — its toolset is `board`, `webfetch`, `todowrite`,
`task`, `cron` and `schedule_wakeup` only (it cannot read or edit files).
It is the agent
behind the chat beside the board tab (horizontally resizable — drag its
right edge; double-click resets).

The orchestrator **automatically watches every card it starts**: when
that card's session marks it done, the orchestrator is pinged with a
short "Ticket X is done." message so it can coordinate follow-ups (start
the next linked card, report status, …) without being asked. When it
finishes a coordination task it reports the state of the board: what is
running, what is waiting, what is scheduled.

## Configuration

Config is JSON(C), read from (in order, merged):

- Global: `~/.config/intellexor/intellexor.json` (also `opencode.json` there, plus
  legacy `~/.config/opencode/opencode.json`, `~/opencode.json`, and
  `/app/intellexor.json` in the container)
- Project: `intellexor.json` / `opencode.json` (`.jsonc` supported) in the working
  directory, merged upward to the filesystem root

Providers use the **OpenAI-compatible** protocol (`/chat/completions` SSE) by
default — this covers vLLM, one-api, OpenRouter, Ollama, etc:

```json
{
  "model": "vllm/intellexie",
  "provider": {
    "vllm": {
      "name": "vLLM",
      "options": {
        "baseURL": "http://localhost:8080/v1",
        "apiKey": "secret"
      },
      "models": {
        "intellexie": {
          "name": "intellexie",
          "tool_call": true,
          "attachment": true,
          "options": { "thinking_token_budget": 4096 },
          "limit": { "context": 262144, "output": 65536 },
          "cost": { "input": 0, "output": 0 }
        }
      }
    }
  }
}
```

- Per-model `options` are merged verbatim into the request body (e.g.
  `thinking_token_budget`).
- Auth keys are resolved per provider: config > `auth.json` (managed via
  `PUT /auth/:providerID`) > environment variables.
- `permission` uses the rules format (`{"bash": {"git *": "allow", "*":
  "ask"}}`, last matching rule wins) but is currently inert — everything is
  allowed.
- `compaction` controls context compaction (opencode-style): `"auto": false`
  disables the automatic summarize-when-overflow pass, `"reserved"` overrides
  the token reserve kept free below the model's context window (default: the
  model's output limit, capped at 32000). `/compact` in the composer
  summarizes the session on demand either way.

## HTTP API

JSON shapes follow the v1 convention: errors as `{name, data}`, success
values as bare JSON. Highlights:

- `GET /event` — SSE stream of `{id, type, properties}` frames with
  `server.connected` first and 10s heartbeats; `GET /global/event` wraps
  frames as `{directory, payload}`
- Sessions: CRUD, children, todo, diff, messages, abort, fork, shell,
  command, share stub, message/part delete; `prompt_async` (204) and blocking
  `POST /session/:id/message` (returns the message with parts)
- Cron jobs: `GET /cron`, `POST /cron` `{title, prompt, spec}`,
  `PATCH /cron/:id` `{title?|prompt?|spec?|paused?}`, `DELETE /cron/:id`,
  and `GET /cron/preview?spec=` (validates an expression, returns its next
  fire time). A background sweep starts a fresh session per due job and
  broadcasts `cron.updated` on the hub.
- `POST /session/:id/revert` / `unrevert` — rewind the conversation to a
  message: later messages are hidden (purged on the next prompt) and file
  changes made by them are restored from per-message backups taken before
  each edit/write/apply_patch
- `GET /session/:id/export` — download the full session (info, every message
  with all parts/tool calls, todo and diff state) as a JSON file;
  `POST /session/import` — body is such a file, creates a brand-new session
  (new id, request's directory) with the complete history loaded; the web UI
  offers Import in the session header only while the session has no messages
- `GET /board/export` — download the whole board (every card with column,
  order, done-links, schedule and attachments) as a JSON file;
  `POST /board/import?mode=replace|merge` — body is such a file: replace
  makes the board exactly the imported one, merge appends its cards with
  fresh ids (done-links remapped); the board header offers both as a choice
- `POST /session/:id/summarize` — compact the context now (also the `/compact`
  command): the conversation is summarized into a `summary: true` assistant
  message and later requests replay history from it; runs automatically
  before a step when the context overflows the model window (see
  `compaction` config)
- Events: `session.{created,updated,deleted,status,idle,error,diff}`,
  `message.{updated,removed,part.updated,part.delta}`,
  `permission.{asked,replied}`, `question.{asked,replied,rejected}`,
  `todo.updated`, `command.executed`, `board.updated`, `cron.updated`
- `GET /provider`, `GET/PUT/DELETE /auth/:id`, `/config`,
  `/config/providers`, `/permission` + reply, `/question` + reply/reject,
  `/path`, `/vcs*`, `/agent`, `/command`, `/mcp`, `/file*`,
  `/global/health`, `/doc` (self-describing endpoint)
- Basic auth when `OPENCODE_SERVER_PASSWORD` is set (`?auth_token=` for SSE)
- `?directory=` / `x-opencode-directory` header for instance routing

## Agents

Five agents ship compiled in:

| Name      | Mode      | Purpose                                                        |
| --------- | --------- | -------------------------------------------------------------- |
| `build`   | primary   | Default agent, full tool access                                |
| `plan`    | primary   | Read-only planning; edit/write denied                          |
| `orchestrator` | primary | Coordinates work: kanban board, starting cards, cron jobs (board/webfetch/todowrite/task/cron only) |
| `general` | subagent  | Multi-step research and parallelizable work                    |
| `explore` | subagent  | Fast codebase search (grep/glob/read/bash/webfetch only)       |

## Layout

```
main.go                 entry point (serve + acp)
internal/acp/        ACP JSON-RPC transport, methods, event mapping
internal/app/        instance, agents, prompt loop, processor, history
internal/config/     global/project config loading and merging
internal/cron/       cron jobs: schedule parsing, per-project storage
internal/provider/   provider catalog, OpenAI-compatible streaming client
internal/tool/       built-in tools + truncation + validation
internal/session/    wire types (Session/Message/Part) + file storage
internal/permission/ internal/question/  blocking ask/reply services
internal/server/     HTTP routing, SSE, instance management, embedded UI
web/                 embedded SPA (assets/)
```
