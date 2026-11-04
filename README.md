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

## Why Intellexor?

Most agent harnesses are Node.js servers: hundreds of MB of RSS before the
first prompt, a `node_modules` tree, and a runtime you have to keep alive.
Intellexor is a **single static Go binary** — the agent loop, tools, storage,
web UI and speech-to-text engine are all compiled in. It idles at a few tens
of MB of RAM and starts in milliseconds, so it is comfortable to leave
running permanently on a laptop, a VPS, a Raspberry Pi or in a tiny
container — which matters, because its best features (cron jobs, scheduled
cards, wake-up alarms) only pay off when the server is *always on*.

- **One binary, zero dependencies.** No Node, no Python, no database. Copy
  the binary to a machine and run it.
- **One port serves everything.** Web UI, HTTP API and ACP JSON-RPC are
  multiplexed on the same socket — no reverse proxy juggling.
- **Headless by argument, not by build.** The same binary opens a desktop
  window on your workstation or runs as a plain server with `--headless` —
  e.g. in Docker on a build box — and you drive it from any browser on the
  network. Your agent sessions live on the server, so you can close the
  laptop and check on them from your phone's browser later.
- **Always-on autonomy.** Cron jobs, card schedules and wake-up alarms are
  persisted server-side and survive restarts — the agent keeps working (or
  wakes up to work) whether or not a browser is connected.

## When would I use what?

The features below are independent but compose. Typical shapes:

- **"Run this 40-minute training job and tell me when it's done."**
  Ask the agent to launch the job in the background and schedule a wake-up
  (see [Alarms](#alarms)). It ends its turn immediately instead of burning
  tokens polling, and the alarm brings it back to check the result —
  parse the metrics, fix a failure, or report success.

- **"Wait for the CI pipeline, then merge."** Same pattern: the agent
  triggers the pipeline, schedules a wake-up for the expected duration,
  and when woken checks the status and proceeds (or reschedules if the
  pipeline is still running).

- **"Implement, then test, then write the docs."** Put three cards on the
  kanban board and **link** them in a chain (see [Kanban board](#kanban-board)).
  When the implementation card is marked done, the test card starts itself
  in a fresh session, then the docs card. You review the result of each
  stage while the chain keeps moving.

- **"Start the load test at 2 AM."** Create the card now, **schedule** it
  (`in: "1d"` or a `startAt` timestamp), and go home. The scheduler starts
  it exactly like the Start button — a fresh session with the card's task.

- **"Keep an eye on new issues and prepare fixes."** A **cron job** (see
  [Cron jobs](#cron-jobs)) runs a triage prompt every morning: it reads the
  issue tracker, writes one kanban card per actionable issue. The cards sit
  in `open` for a developer to review, tweak and start — the agent never
  ships anything on its own, it just keeps the backlog ready.

- **"Coordinate all of the above."** The **orchestrator** agent lives in the
  chat beside the board: it starts cards, chains them, schedules jobs and
  wake-ups, and is pinged when each ticket finishes so it can kick off the
  next one.

## Build & run

```sh
go build -o bin/intellexor .
./bin/intellexor --dir /path/to/project --port 4096
# open http://127.0.0.1:4096
```

Flags: `--host` (default `127.0.0.1`), `--port` (default `4096`, falls back to
a random free port if busy), `--dir` (working directory, defaults to cwd),
`--quiet`, and `--headless` / `-h`.

**One port serves everything**: the web UI, the HTTP API and ACP
(newline-delimited JSON-RPC 2.0) are multiplexed on the same socket —
connections are classified by their first byte, so browsers get the UI and
ACP clients get a JSON-RPC stream. ACP is always on; there is no separate
mode. Methods: `initialize`, `session/new|load|list|prompt|cancel`,
`session/update` notifications, and `session/request_permission` callbacks.

## Desktop app

By default Intellexor opens the web UI in a native desktop window (via
[Wails](https://wails.io)) while the server keeps running as usual — the
window talks to the same in-process handler, so nothing else changes. Pass
`--headless` (or `-h`) to skip the window and run as a plain server.

**There is exactly one app**: desktop vs headless is a runtime argument, not
a build variant. Release binaries are built with the desktop window included;
`--headless` turns any of them into a plain server. Building from source
needs the platform's GUI headers and the `desktop,production` build tags (on
Ubuntu ≥ 24.04 use WebKit 4.1 via the `webkit2_41` tag):

```sh
# Linux: sudo apt install build-essential pkg-config libgtk-3-dev libwebkit2gtk-4.1-dev
go build -tags "desktop,production,webkit2_41" -o bin/intellexor .
./bin/intellexor --dir /path/to/project
```

The released Linux binary dynamically links libgtk/libwebkit — present on any
desktop distro; headless server deployments belong in Docker (the image
builds its own CGO-free binary). On macOS, add
`CGO_LDFLAGS="-framework UniformTypeIdentifiers"` (Wails' darwin frontend
uses UTType but never links its framework).

Closing the window shuts the server down; if no display is available the
process logs the GUI error and keeps serving headless.

## Docker

```sh
docker build -t intellexor .
docker run -p 8080:8080 -v my-project:/workspace -v intellexor-data:/data intellexor
```

The container starts the server (headless) on `0.0.0.0:4096` — the browser
UI, HTTP API and ACP clients share that one port (override with
`docker run ... intellexor --port 8080`). Data lives in `/data`, the
working project in `/workspace`. Provider config can be mounted at
`/data/.config/intellexor/intellexor.json` (or set `XDG_CONFIG_HOME`); `/app/intellexor.json`
is also read, so `-v "$PWD/intellexor.json:/app/intellexor.json"` just works.

The image is ubuntu-based and gives the agent a full toolchain: `bash`,
`curl`, `git`, `node`/`npm`, `go` (1.26.x at `/usr/local/go`), and
`playwright` with chromium (browsers in `/opt/ms-playwright`).

## Speech to text (mic button)

The composer has a microphone button: click, talk, click again — the whole
recording is transcribed once and appended to the input box (no real-time
partials). It runs **fully client-side** (transformers.js / onnxruntime-web
in a module Worker + NVIDIA **Parakeet** `parakeet-ctc-0.6b`, a CTC model —
fp16 on GPU, q8 on CPU; the model choice is a hard requirement, do not swap
it for Whisper); audio never leaves the browser. Inference runs on
**WebGPU** when the browser exposes it, with an automatic fallback to CPU
wasm. The engine (~42 MB) ships embedded with the binary and is served from
`/stt/`; the ~1.2 GB model is downloaded once from the Hugging Face hub
with a visible progress bar and cached in the browser (Cache Storage
`transformers-cache`). Parakeet is English-only and needs no language
config. Requires a secure context (`https://` or `localhost`) for
microphone access.

## Kanban board

The board is where you *delegate* instead of chatting. Instead of one long
conversation, you break work into cards; each card that gets started runs in
its own fresh agent session, so tasks stay isolated, reviewable and
parallelizable. Use it when you have several independent tasks (or a
multi-stage pipeline) and want the agent to chew through them while you
review results as they land.

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
  session (only cards still waiting in `open` can be targeted). This is the
  multi-stage pattern: *implement → write tests → update docs*, or
  *migrate schema → backfill data → verify counts*. Each stage is its own
  session with a clean context, and you can inspect (or abort) any stage
  while the rest of the chain keeps moving. Cards waiting on a link show
  what unblocks them, so the board reads like a pipeline.
- **Schedules:** `schedule` sets a timed start on a card — pass `in`
  (`"30m"`, `"2h"`, `"1d"`) or `startAt` (unix milliseconds) and a
  background scheduler starts the card then, exactly like the Start
  button. Use it for off-hours work: queue the risky migration now,
  schedule it for 2 AM, review the diff in the morning. Only open cards
  can be scheduled; `startAt 0` clears the schedule.

The two compose: a scheduled card can itself be the head of a chain, so
"tonight at 2 AM: deploy, then run the smoke tests, then post the results"
is one scheduled card plus two links.

The whole board can be exported/imported as JSON (`GET /board/export`,
`POST /board/import?mode=replace|merge`).

## Cron jobs

Cron jobs turn the agent into a **scheduled worker**: a prompt that runs on
a schedule whether or not you remember to ask. Use them for recurring
chores that don't need a human in the loop to *start* — triaging new
issues, checking for failing CI runs or stale dependency alerts, sweeping
TODOs, generating a daily changelog draft. A common setup is a triage job
that inspects the issue tracker each morning and **creates kanban cards**
for what it finds: the cards land in `open` for a developer to review,
adjust and start — the agent prepares the backlog, the human decides what
ships.

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

Alarms solve the problem every agent harness has with **long-running work**:
the job takes 40 minutes, but blocking on `sleep` or polling in a loop
wastes tokens and holds the session hostage. Instead, the agent launches
the work in the background, schedules a wake-up for when it should be
finished, and ends its turn — the session goes idle (freeing you to use it
or close the browser), and at wake-up time the system injects a message
that brings the agent back to check the result: read the training metrics,
inspect the pipeline status, or reschedule if the job needs more time.

The `schedule_wakeup` tool schedules a **one-shot wake-up for the agent's
own session**: at the given timestamp (unix seconds, or a relative `in`
duration like `"5m"`) the system
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

In the web UI the ⏰ indicator on a session (header, tab, sidebar) is
clickable and opens a modal listing the session's wake-ups — pending
ones with a live "will trigger in dd hh:mm:ss" countdown, fired ones
with "triggered ... ago" — each with **Trigger now** and **Delete**
buttons (`GET /alarm/session/{sessionID}`,
`POST /alarm/{alarmID}/trigger`, `DELETE /alarm/{alarmID}`). Wake-ups
are also part of the session export: importing re-arms future ones
under the new session, while past-due untriggered ones are marked fired
so an import never replays stale wake-ups.

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
  disables the automatic summarize pass, `"reserved"` overrides the token
  reserve kept free below the model's context window (default: 15% of the
  window, at least 16000 for windows that can afford it). Auto compaction
  fires proactively — once the estimated context (the last response's
  measured prompt size plus the unmeasured growth since) reaches the window
  minus that reserve — so the summary request itself still fits.
  `/compact` in the composer summarizes the session on demand either way.
- `worktree` enables optional **git worktree isolation** for parallel work:
  `"enabled": true` runs every board card and `task` subagent in a fresh
  worktree on its own branch (`itx/…`) instead of the shared directory, so
  several sessions can edit the same repository at once. `"dir"` overrides
  the worktree location (default `.intellexor/worktrees` inside the repo,
  excluded via `.git/info/exclude` so project config and `AGENTS.md` resolve
  identically), `"base"` overrides the ref new worktrees branch from
  (default: current HEAD). Individual cards can opt in via their *Isolate in
  a git worktree* toggle (or the board tool's `add … isolate: true`) even
  when the default is off, and the `task` tool accepts
  `isolation: "worktree"` per call. The card shows the branch, offers a
  diff against the base (`GET /session/:id/worktree/diff`) and a **Merge**
  action (`POST /board/card/:id/merge`) that commits pending worktree
  changes, merges the branch into the repo and removes the worktree.
- `board` configures the kanban board's automation: `"max_running"` caps how
  many cards the dependency flow starts at once (0 = unlimited; manual
  starts are never capped), `"review_agent"` names the agent that reviews a
  card's changes when it moves to the review column (default the built-in
  `review` agent; empty disables the automatic run), and `"auto_review":
  false` turns the automatic review run off. Cards can pick their own agent
  and model, carry a description and acceptance criteria (both sent to the
  agent with the task), and declare `blockedBy` dependencies — a card only
  starts once all of its blockers are done. When a card's session goes idle
  it moves to review; approving moves it to done (merging an isolated
  worktree), rejecting sends it back to in-progress with the findings as
  the next prompt. Provider failures (429/5xx) are retried once, then the
  card is marked blocked with the error.

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
- `POST /board/card/:id/merge` — merge a worktree-isolated card's branch
  back into the repository and remove the worktree (the card's session
  returns to the main directory);
  `GET /session/:id/worktree/diff` — diff of an isolated session's worktree
  against its base ref
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
main.go                 entry point (server + desktop window)
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
