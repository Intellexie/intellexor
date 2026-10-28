# Intellexor API Documentation — UI Client Reference

**Audience:** an AI agent (or human) building a full UI client that talks to an Intellexor server. This document contains *everything* needed: the HTTP API, the SSE event stream, the ACP (Agent Client Protocol) JSON-RPC surface, all wire types, and the typical client flows.

**Source of truth:** the Go code in `internal/server`, `internal/acp`, `internal/app`, `internal/session`, `internal/board`, `internal/cron`, `internal/alarm`. This file must be updated whenever the API changes (see AGENTS.md).

---

## Table of contents

1. [Conventions](#1-conventions) — base URL, auth, directory routing, errors, CORS
2. [Wire types](#2-wire-types) — Session, Message, Part, Tokens, FileDiff, PermissionRequest
3. [HTTP API — Sessions](#3-http-api--sessions)
4. [HTTP API — Permissions & Questions](#4-http-api--permissions--questions)
5. [HTTP API — Kanban board](#5-http-api--kanban-board)
6. [HTTP API — Cron jobs](#6-http-api--cron-jobs)
7. [HTTP API — Alarms (wake-ups)](#7-http-api--alarms-wake-ups)
8. [HTTP API — Config, providers, auth](#8-http-api--config-providers-auth)
9. [HTTP API — Workspaces](#9-http-api--workspaces)
10. [HTTP API — Misc (path, vcs, files, agents, commands)](#10-http-api--misc)
11. [SSE event stream](#11-sse-event-stream) — full event catalog
12. [ACP protocol](#12-acp-protocol) — JSON-RPC 2.0 over the same port
13. [Typical client flows](#13-typical-client-flows)
14. [Endpoint quick reference](#14-endpoint-quick-reference)
15. [UI requirements & feature-parity spec](#15-ui-requirements--feature-parity-spec) — build a complete new UI from this

---

## 1. Conventions

### 1.1 Base URL & ports

- One binary serves **everything on one port** (default `4096`): the web UI, the HTTP API, and ACP. If 4096 is busy the server silently binds a random free port — **the URL the server prints at startup is the source of truth**.
- Default host `127.0.0.1`. Docker exposes `0.0.0.0:4096`.

### 1.2 Authentication

Auth is **disabled** unless the env var `OPENCODE_SERVER_PASSWORD` is set. When set, every request must present credentials one of two ways:

1. **HTTP Basic auth**: `Authorization: Basic base64(user:password)` — username defaults to `opencode` (override with `OPENCODE_SERVER_USERNAME`).
2. **Query token** (needed for `EventSource`, which cannot set headers): `?auth_token=<base64("user:password")>` — standard base64 of the `user:password` string.

Failure → `401` with header `www-authenticate: Basic realm="Secure Area"`, empty body.

### 1.3 Directory (instance) routing

The server keeps one `app.Instance` per working directory. Every instance-scoped route selects the directory via:

- `?directory=<abs path>` query param (wins), or
- `x-opencode-directory: <abs path>` header, or
- omitted → the server's default directory.

**Session-scoped routes** (`/session/{id}/...`) resolve the directory *from the session record itself*, so `?directory=` is effectively optional there. CORS allow-lists the headers `Content-Type, Authorization, x-opencode-directory, x-opencode-workspace`.

A non-empty `X-Timezone: <IANA name>` header on session routes is persisted on the session (used for cron/schedule display).

### 1.4 Response shapes

- **Success**: bare JSON (no envelope). Ack endpoints return the literal `true`, `[]`, `{}` or `null`.
- **Error**: `{ "name": "<string>", "data": { ... } }` with the HTTP status:

| Status | `name` | `data` |
|---|---|---|
| 400 | `BadRequest` | `{"message": "..."}` |
| 404 | `NotFoundError` | `{"message": "..."}` |
| 404 | `SessionNotFoundError` | `{"sessionID": "...", "message": "Session not found: <id>"}` |
| 500 | `NamedError.Unknown` | `{"message": "..."}` (e.g. share endpoints) |

- Empty/absent request bodies decode to the zero struct (no error). Malformed JSON → 400 `BadRequest`.
- `OPTIONS` on any route → `204`, empty. CORS: `Access-Control-Allow-Origin: *`.

### 1.5 Timestamp units — IMPORTANT

- Session/message/part/board/cron timestamps: **unix milliseconds**.
- Alarm `triggerAt`/`created`: **unix seconds** (legacy ms values are migrated on load).
- ACP `session/list` `updatedAt`: **unix seconds**.

---

## 2. Wire types

All types live in `internal/session/types.go` unless noted.

### 2.1 `Session`

```jsonc
{
  "id": "ses_...",              // string
  "slug": "abcd1234",           // 8-char string
  "projectID": "...",
  "directory": "/abs/path",
  "path": "...",                // optional
  "parentID": "ses_...",        // optional (subagent children)
  "title": "...",
  "version": "1.15.0",
  "agent": "build",             // optional
  "timezone": "Europe/Berlin",  // optional
  "model": {                    // optional ModelRef
    "id": "...", "providerID": "...", "variant": "..."
  },
  "cost": 0.0,
  "tokens": {                   // always present
    "input": 0.0, "output": 0.0, "reasoning": 0.0,
    "total": 0.0,                       // optional
    "cache": { "read": 0.0, "write": 0.0 }
  },
  "context": 0.0,               // optional — estimated context size
  "share": { "url": "..." },    // optional (unused in Intellexor)
  "metadata": {},               // optional map[string]any
  "time": {
    "created": 0, "updated": 0, // unix ms
    "compacting": 0,            // optional — set while compaction runs
    "archived": 0               // optional — archived timestamp
  },
  "permission": [               // optional rules (inert, opencode compat)
    { "permission": "...", "pattern": "...", "action": "..." }
  ],
  "summary": {                  // optional — last compaction summary
    "additions": 0, "deletions": 0, "files": 0,
    "diffs": [ { "file": "...", "patch": "...", "additions": 0, "deletions": 0, "status": "..." } ]
  },
  "revert": {                   // optional — active revert state
    "messageID": "...", "partID": "...", "snapshot": "...", "diff": "..."
  }
}
```

### 2.2 Messages

`UserMessage`:

```jsonc
{
  "id": "msg_...", "sessionID": "ses_...", "role": "user",
  "time": { "created": 0 },
  "agent": "...", "model": { "providerID": "...", "modelID": "...", "variant": "..." },
  "format": <raw>,                       // optional
  "summary": { "title": "...", "body": "...", "diffs": [FileDiff] },  // optional
  "system": "...",                       // optional
  "tools": { "name": true }              // optional per-session tool overrides
}
```

`AssistantMessage`:

```jsonc
{
  "id": "msg_...", "sessionID": "ses_...", "role": "assistant",
  "parentID": "msg_...",
  "time": { "created": 0, "completed": 0 },
  "error": { "name": "...", "data": {} },  // optional NamedError, e.g. MessageAbortedError
  "modelID": "...", "providerID": "...", "mode": "...", "agent": "...",
  "path": { "cwd": "...", "root": "..." },
  "summary": true,          // optional bool — true on compaction summary messages
  "cost": 0.0,
  "tokens": { /* same Tokens shape */ },
  "structured": <raw>,      // optional
  "variant": "...",         // optional
  "finish": "..."           // optional: "stop", "tool-calls", "error", ...
}
```

Messages are returned as `WithParts`:

```jsonc
{ "info": UserMessage | AssistantMessage, "parts": [Part] }   // discriminate by info.role
```

### 2.3 `Part` — one flat struct, fields per `type`

Common fields: `id`, `sessionID`, `messageID`, `type`.

| `type` | Extra fields |
|---|---|
| `text` | `text`, `synthetic?` (bool), `ignored?` (bool), `time {start, end?}`, `metadata?` |
| `reasoning` | same shape as `text` |
| `tool` | `callID`, `tool`, `state` (ToolState below) |
| `step-start` | `reason?` |
| `step-finish` | `reason?`, `snapshot?`, `cost?` (float), `tokens?` |
| `patch` | `hash`, `files: [string]` |
| `file` | `mime`, `filename`, `url` (data URI), `source?` (FileSource) |
| `agent` | `name` |
| `subtask` | `prompt`, `description`, `agent`, `model?` (ModelRef), `command?` |
| `retry` | `attempt` (int), `error` (NamedError) — emitted on provider retries, including app-level step retries after a mid-stream failure (connection reset/truncated SSE); the retried turn gets a fresh assistant message, the failed attempt stays in the transcript |
| `compaction` | `auto?` (bool), `overflow?` (bool), `tail_start_id?` |

`ToolState`:

```jsonc
{
  "status": "pending" | "running" | "completed" | "error",
  "input": {},          // optional map
  "raw": "...",         // optional
  "title": "...",       // optional
  "metadata": {},       // optional (e.g. {"output": "..."} for bash)
  "output": "...",      // optional (cleared when time.compacted is set)
  "error": "...",       // optional string, on status=error
  "time": { "start": 0, "end": 0, "compacted": 0 },
  "attachments": []     // optional
}
```

`FileSource`: `{ "type", "path"?, "text"?: {"value","start","end"}, "name"?, "kind"?, "range"?, "clientName"?, "uri"? }`.

### 2.4 Other shared types

- `FileDiff`: `{ "file": string, "patch": string, "additions": int, "deletions": int, "status"?: string }`
- `NamedError`: `{ "name": string, "data": map[string]any }` — e.g. `{"name":"APIError","data":{"message":"...","isRetryable":true,"statusCode":429}}`
- `PermissionRequest` (from `GET /permission`): `{ "id", "sessionID", "permission", "patterns": [string], "metadata": {}, "always": [string], "tool"?: {"messageID","callID"} }`

---

## 3. HTTP API — Sessions

### 3.1 `GET /session` — list sessions

Query: `?directory=`, `?scope=project` (list sessions across all projects), `?roots=true` (omit sessions with a `parentID`).
**200**: array of `Session`, sorted by `time.updated` descending.

### 3.2 `POST /session` — create session

Body (all optional):

```jsonc
{
  "parentID": "ses_...",       // creates a child session
  "title": "...",              // default: "New session - <iso>" or "Child session - <iso>"
  "agent": "build",
  "model": { "id": "...", "providerID": "...", "variant": "..." },
  "metadata": {}
}
```

**200**: the created `Session`. Events: `session.created` + `session.updated`.

### 3.3 `GET /session/{sessionID}` — get session
**200**: `Session`. **404**: `SessionNotFoundError`.

### 3.4 `DELETE /session/{sessionID}` — delete session
Aborts any running prompt first, removes storage. **200**: `true`. Event: `session.deleted`.

### 3.5 `PATCH /session/{sessionID}` — update session

Body (all optional):

```jsonc
{
  "title": "...",
  "metadata": {},
  "permission": [ { "permission": "...", "pattern": "...", "action": "..." } ],  // merged with existing
  "time": { "archived": 1700000000000 }   // archive (number); null is ignored
}
```

**200**: updated `Session`. Event: `session.updated`.

### 3.6 `GET /session/{sessionID}/children`
**200**: array of `Session` with `parentID == id` (`[]` if none).

### 3.7 `GET /session/{sessionID}/todo`
**200**: array of todo objects (raw maps, typically `{content, status, priority}`). `[]` if none.

### 3.8 `DELETE /session/{sessionID}/todo`
**200**: `[]`. Event: `todo.updated` with `todos: []`.

### 3.9 `GET /session/{sessionID}/diff`
**200**: array of `FileDiff` — cumulative file changes made by the session. `[]` if none.

### 3.10 `GET /session/{sessionID}/message` — list messages

Query: `?limit=N` (optional; counts display units — user prompt = 1, each assistant text/reasoning/tool part = 1; whole messages are never split so the result may exceed the limit). Invalid `limit` → 400.
**200**: array of `WithParts`. Reverted messages (from `revert.messageID` onward) are excluded.

### 3.11 `DELETE /session/{sessionID}/message/{messageID}`
**200**: `true`. Event: `message.removed`.

### 3.12 `DELETE /session/{sessionID}/message/{messageID}/part/{partID}`
**200**: `true`. Event: `message.part.removed`.

### 3.13 `POST /session/{sessionID}/message` — blocking prompt

Body (`PromptInput`):

```jsonc
{
  "messageID": "msg_...",            // optional client-chosen id
  "model": { "providerID": "...", "modelID": "..." },
  "agent": "build",
  "noReply": false,
  "tools": { "bash": true },         // per-prompt tool overrides
  "system": "...",
  "variant": "...",
  "parts": [ { "type": "text", "text": "..." } ]   // REQUIRED, non-empty
}
```

Part objects are `Part`-shaped JSON; typical: `{"type":"text","text":"..."}` or `{"type":"file","mime":"image/png","url":"data:image/png;base64,...","filename":"..."}`.
**200**: `WithParts` — the final assistant message (blocks until the run completes). **400** if `parts` empty or the prompt fails.

### 3.14 `POST /session/{sessionID}/prompt_async` — async prompt

Same body as 3.13. **204 No Content**, empty body. Progress arrives via SSE (`message.part.delta`, `message.part.updated`, `message.updated`, `session.status`, `session.idle`). **This is the intended interactive flow.**

### 3.15 `POST /session/{sessionID}/abort`
Cancels the run and rejects pending permissions with reason `"Aborted"`. **200**: `true`.

### 3.16 `GET /session/status`
Query: `?directory=`. **200**: `map[sessionID] → StatusInfo` — only *busy* sessions (`{}` when all idle). `StatusInfo` shapes: `{"type":"busy"}`, `{"type":"retry","attempt":1,"message":"...","next":<ms>}`, `{"type":"compact"}`.

### 3.17 `POST /session/{sessionID}/shell` — `!` shell mode

Body: `{ "command": "...", "agent": "..." }`. The command is prefixed with `!` and prompted.
**200**: last assistant `WithParts` (or `{"info":null,"parts":null}`).

### 3.18 `POST /session/{sessionID}/command` — slash commands

Body: `{ "command": "init", "arguments": "...", "agent": "...", "model": "..." }`.
Built-ins: `init` (guided AGENTS.md setup), `review` (review changes), `compact` (summarize session). Config-defined commands override built-ins. Template placeholders: `${path}`, `$ARGUMENTS`, `$1`..`$N`.
**200**: last assistant `WithParts`. Event: `command.executed` `{name, sessionID, arguments}` (after execution). **404** `Command not found: <name>`.

### 3.19 `POST /session/{sessionID}/summarize` — compact context now

Body: `{ "providerID": "...", "modelID": "..." }` (both optional). **200**: `WithParts` (the summary message). Also runs automatically when context overflows (see `compaction` config).

### 3.20 `POST /session/{sessionID}/init`
No-op stub. **200**: `true`.

### 3.21 `POST /session/{sessionID}/fork`

Body: `{ "messageID": "msg_..." }` (optional — forks whole visible history).
**200**: the new `Session` (new id/slug, title `"<title> (forked)"`, copies todo/diff/alarms). Event: `session.created`.

### 3.22 `GET /session/{sessionID}/export`

**200**: full export payload (also served as an attachment):

```jsonc
{
  "format": "intellexor.session", "version": 1, "exportedAt": 0,
  "session": Session,
  "messages": [ { "info": <message>, "parts": [Part] } ],
  "todos": <raw>, "diffs": <raw>,
  "alarms": [Alarm]
}
```

### 3.23 `POST /session/import`

Body: an export payload (3.22). `format` must be `""` or `"intellexor.session"`. Creates a brand-new session (new id, request's directory) with the full history.
**200**: the new `Session`. Events: `session.created`, `session.updated`.

### 3.24 `POST /session/{sessionID}/revert`

Body: `{ "messageID": "msg_..." }` (required), `partID` optional/unused.
Rewinds the conversation to that message: later messages are hidden (purged on next prompt) and their file changes are restored from per-message backups. **200**: updated `Session` (with `revert` set). Event: `session.updated`.

### 3.25 `POST /session/{sessionID}/unrevert`
Restores snapshot + hidden messages, clears `revert`. **200**: updated `Session`.

### 3.26 `POST|DELETE /session/{sessionID}/share`
Always **500** `{"name":"NamedError.Unknown","data":{"message":"sharing is not available in Intellexor"}}`.

### 3.27 `POST /session/{sessionID}/permissions/{permissionID}`

Body: `{ "response": "once" | "always" | "reject" }`. **200**: `true`. (Permissions are inert by default — see §4.)

---

## 4. HTTP API — Permissions & Questions

### 4.1 Permissions (⚠ inert)

`GET /permission` → array of `PermissionRequest`. `POST /permission/{requestID}/reply` body `{"response":"once"|"always"|"reject"}` → `true`.

**Intellexor grants everything by default** — `permission.asked`/`permission.replied` events are never emitted in the default build and these endpoints exist only for opencode compatibility. A UI should not rely on them (but implementing the reply flow is harmless).

### 4.2 Questions (live — the `question` tool)

When the agent calls the `question` tool, the UI must answer:

- `GET /question` → **200**: array of pending requests:
  ```jsonc
  {
    "id": "que_...", "sessionID": "ses_...",
    "questions": [
      { "question": "...", "header": "...",
        "options": [ { "label": "...", "description": "..." } ],
        "multiple": false, "custom": false }
    ],
    "tool": { "messageID": "...", "callID": "..." }   // or null
  }
  ```
- `POST /question/{requestID}/reply` — body: `{ "answers": [["option label", ...], ...] }` (one array per question). **200**: `true`. Event: `question.replied`.
- `POST /question/{requestID}/reject` — no body. **200**: `true`. Event: `question.rejected`. Rejecting also **aborts the session run** (like `POST /session/{id}/abort`): the agent is blocked on the answer and must not resume with the refusal as a tool result.

The asking tool call blocks until reply/reject. On reply the agent continues with the answers; on reject the run is stopped.

---

## 5. HTTP API — Kanban board

### 5.1 Data model — `Card`

```jsonc
{
  "id": "1",                    // plain number string; max+1, never repurposed
  "title": "...",               // also used as session title + prompt on start
  "column": "open" | "inprogress" | "done",
  "order": 0,                   // position within column, ascending, always dense 0..n-1
  "created": 0,                 // unix ms
  "sessionID": "ses_...",       // optional — session working on the card
  "watchers": ["ses_..."],      // optional — sessions pinged when card is done
  "link": "2",                  // optional — target card auto-starts when this one is done
  "startAt": 0,                 // optional — scheduled start, unix ms (open cards only)
  "attachments": [              // optional — passed to the session prompt on start
    { "type": "file", "mime": "image/png", "url": "data:...", "filename": "..." }
  ]
}
```

Semantics:

- Cards are created **only in `open`**. Columns: `open` → `inprogress` → `done`.
- **Starting a card** (Start button / agent `board` tool / done-link / scheduler) creates a session titled with the card title, moves the card to `inprogress`, links `sessionID`, and prompts the session with the title + attachments.
- Moving a card to **`done`** prompts all `watchers` (on behalf of the user) and auto-starts the `link` target if it is still `open` and unowned.
- `sessionID` can only be set while the card is `open` and unowned (`""` clears it). `startAt` can only be set while `open` and unowned (`0` clears). `link` targets must exist, not be self, and be `open`.
- Deleting a card clears links pointing at it.

### 5.2 Endpoints

| Route | Body / params | Response |
|---|---|---|
| `GET /board` | — | **200** array of `Card`, display-ordered (column rank, then `order`) |
| `POST /board/card` | `{ "title": "...", "column": "open", "attachments": [...] }` (column must be `""`/`"open"`) | **200** created `Card` |
| `PATCH /board/card/{cardID}` | see below | **200** updated `Card` |
| `DELETE /board/card/{cardID}` | — | **200** `true` |
| `GET /board/export` | — | **200** `{ "format": "intellexor.board", "version": 1, "exportedAt": ms, "cards": [Card] }` (attachment) |
| `POST /board/import?mode=replace\|merge` | body: export payload | **200** `{ "imported": <int> }` |

`PATCH /board/card/{cardID}` body — all fields optional pointers; only non-nil apply:

```jsonc
{
  "title": "...",        // rename
  "column": "done",      // move (triggers watchers/link when moving to done)
  "before": "3",         // insert before this card in the target column (end if empty)
  "sessionID": "ses_...",// link a session (open+unowned only; "" clears)
  "link": "2",           // set/clear done-link ("" clears)
  "startAt": 1700000000000  // schedule (unix ms; 0 clears; open+unowned only)
}
```

Errors: 404 `Card not found: <id>`; 400 with messages like `"Cards can only be added to the open column"`, `"unknown column"`, `"Only cards still waiting in the open column can be scheduled"`.

Import modes: `replace` (default) — board becomes exactly the import (session refs/watchers cleared, invalid columns coerced to `open`); `merge` — appended with fresh ids, links remapped.

All board mutations broadcast **`board.updated`** `{ "directory": "..." }` — clients re-fetch `GET /board`.

---

## 6. HTTP API — Cron jobs

### 6.1 Data model — `Job`

```jsonc
{
  "id": "cron_...",
  "title": "...",              // also the created session's title
  "prompt": "...",             // sent as the user message to each run's fresh session
  "spec": "@daily",            // see below
  "paused": false,
  "created": 0,                // unix ms
  "lastRun": 0,                // optional, unix ms
  "nextRun": 0,                // optional, unix ms — next fire time
  "sessions": ["ses_..."],     // optional — recent run sessions, newest last, cap 20
  "runs": 0
}
```

`spec` formats (server local timezone):

- 5-field cron: `minute hour dom month dow` — `*`, lists, ranges, steps, 3-letter month/day names, `7` = Sunday in dow.
- Macros: `@hourly` `@daily` `@midnight` `@weekly` `@monthly` `@yearly` `@annually`.
- `@every <duration>` — e.g. `@every 30m`, firing relative to now.

Every run starts a **fresh session** (no memory of previous runs — prompts must be self-contained). A background sweep (10 s) claims due jobs and broadcasts `cron.updated`.

### 6.2 Endpoints

| Route | Body / params | Response |
|---|---|---|
| `GET /cron` | — | **200** array of `Job` (oldest first) |
| `POST /cron` | `{ "title": "...", "prompt": "...", "spec": "..." }` — `prompt` + `spec` required | **200** created `Job` (nextRun armed) |
| `GET /cron/preview?spec=<expr>` | — | **200** `{ "next": <unix ms> }`; 400 on parse error |
| `PATCH /cron/{cronID}` | `{ "title"?, "prompt"?, "spec"?, "paused"? }` (pointers; all nil → 400 `"nothing to update"`) | **200** updated `Job` |
| `DELETE /cron/{cronID}` | — | **200** `true` |

Pausing holds only the next run (an already-started session keeps going); resuming re-arms `nextRun` from now without firing. All mutations broadcast `cron.updated` `{ "directory": "..." }`.

---

## 7. HTTP API — Alarms (wake-ups)

One-shot wake-ups for a session: at `triggerAt` a user message `alarm-triggered` (+ optional note) is injected into the session so the agent continues itself. A background sweep checks every 5 s; wake-ups due while the server was down fire right after startup.

### 7.1 Data model — `Alarm`

```jsonc
{
  "id": "alm_...",
  "sessionID": "ses_...",
  "triggerAt": 0,        // unix SECONDS, must be in the future on create
  "note": "...",         // optional context appended to the wake message
  "created": 0,          // unix SECONDS
  "fired": false
}
```

### 7.2 Endpoints

| Route | Body / params | Response |
|---|---|---|
| `GET /alarm` | — | **200** array of **pending** alarms, all sessions, newest first |
| `GET /alarm/session/{sessionID}` | — | **200** all alarms of that session (pending + fired) |
| `POST /alarm` | `{ "sessionID": "...", "triggerAt": <unix sec>, "note": "..." }` | **200** created `Alarm` (400 if sessionID missing/unknown, triggerAt ≤ 0 or in the past) |
| `POST /alarm/{alarmID}/trigger` | — | **200** the claimed `Alarm` (`fired: true`); injects the wake message now |
| `POST /alarm/session/{sessionID}/clear-finished` | — | **200** `{ "removed": <n> }` — deletes all already-fired alarms of the session; pending alarms are untouched |
| `DELETE /alarm/{alarmID}` | — | **200** literal `null` |

All alarm mutations broadcast **`alarm.updated`** `{ "directory": "..." }` on every hub of the project. The firing itself surfaces as a normal prompt (`message.updated` / `message.part.updated`), not a dedicated event.

---

## 8. HTTP API — Config, providers, auth

### 8.1 Config read/write

- `GET /config` (alias `GET /global/config`) → **200**: the merged config (`config.Info`):

```jsonc
{
  "$schema": "...", "shell": "...", "logLevel": "...",
  "model": "provider/model", "small_model": "provider/model",
  "username": "...", "autoshare": true, "autoupdate": true,
  "disabled_providers": [], "enabled_providers": [],
  "instructions": ["path/to/file"],
  "provider": { "<id>": { "name", "npm", "api", "env", "options": {"apiKey","baseURL","api","headers"}, "models": { "<mid>": {...} } } },
  "permission": {},                       // inert
  "compaction": { "auto": true, "reserved": 0.15 },
  "tool_output": { "max_lines": 0, "max_bytes": 0 },
  "disabled_tools": [], "commands": {}, "agent": {},
  "subagent_depth": 3, "mcp": {}, "share": "..."
}
```

- `PATCH /config` (alias `PATCH /global/config`) — body: any JSON object; top-level keys are shallow-merged into the **primary global config file** (`~/.config/intellexor/intellexor.json`). **200**: the body echoed back.

### 8.2 Config introspection & editing

- `GET /config/providers` → `{ "providers": [ { "id", "name", "env": [], "npm"?, "api"?, "models": {Model} } ], "default": { "<providerID>": "<modelID>" } }`
- `GET /config/files` → `{ "files": [ { "path", "scope": "global"|"project", "exists": true } ] }` — merge-precedence order; the last project entry is the create-if-missing target.
- `GET /config/entries` → `{ "providers": {id: {file, config}}, "agents": {id: {file, config}}, "builtin": [ {name, description, mode, color, hidden} ], "default_model": "..." }` — raw per-file entries.
- `PUT /config/provider/{providerID}` — body `{ "file": "<config path>", "entry": {...} }`. `entry.npm` must be empty (auto-set to `"@ai-sdk/openai-compatible"`) or exactly that. **200** `true`.
- `DELETE /config/provider/{providerID}?file=<path>` → `true`.
- `PUT|DELETE /config/provider/{providerID}/model/{modelID}` — same `{file, entry}` body / `?file=` query. **200** `true`.
- `PUT|DELETE /config/agent/{agentID}` — same shapes; writes the `agent` section. **200** `true`.

### 8.3 Providers & auth

- `GET /provider` → `{ "all": [ { "id", "name", "source": "env"|"config"|"custom"|"api"|"wellknown", "env": [], "key"?, "options": {"apiKey":"","baseURL":"..."}, "models": {Model} } ], "default": {pid: mid}, "connected": [pid] }` — the real API key is never returned.
- `GET /provider/auth` → `{ "<providerID>": [ { "type": "api", "label": "api key" } ] }` (label `"api key (env)"` for env-sourced providers).
- `PUT /auth/{providerID}` — body is one `auth.Info`: `{ "type": "api", "key": "sk-..." }` or oauth shape `{ "type": "oauth", "access", "refresh", "expires", "accountId" }`. Written to `<DataDir>/auth.json`. **200** `true`.
- `DELETE /auth/{providerID}` → `true`.
- `POST /provider/{id}/oauth/authorize` → `null`; `POST /provider/{id}/oauth/callback` → `true` (stubs).

`provider.Model` shape:

```jsonc
{
  "id": "...", "name": "...", "release_date"?: "...",
  "tool_call": true, "attachment": true, "reasoning": true, "temperature": true,
  "cost": { "input": 0, "output": 0, "cache_read"?: 0, "cache_write"?: 0 },
  "limit": { "context": 262144, "output": 65536 },
  "options"?: {}, "headers"?: {}, "interleaved"?: true
}
```

---

## 9. HTTP API — Workspaces

Additional working directories with their own sessions/config.

- `GET /workspace` → **200** array of:
  ```jsonc
  { "directory": "/abs", "name": "...", "sessions": 0, "busy": false, "hasIdle": true, "lastActivity": 0 }
  ```
  (server default dir listed first even if unregistered; `name` = user name or `basename(directory)`.)
- `POST /workspace` — body `{ "path": "/abs" }` → **200** the `workspaceInfo`. Seeds `<dir>/intellexor.json` stub if no config exists. 404 if the directory doesn't exist.
- `POST /workspace/rename` — body `{ "directory": "/abs", "name": "..." }` → **200** `true` (empty name resets to basename).

---

## 10. HTTP API — Misc

- `GET /global/health` → `{ "healthy": true, "version": "1.15.0" }`
- `GET /path` → `{ "home", "state", "config", "worktree", "directory" }`
- `POST /instance/dispose` → `true` (no-op)
- `GET /vcs` → `{ "branch": "...", "default_branch"?: "..." }` or `{}`
- `GET /vcs/status` → `[ { "file": "/abs/path", "additions": 0, "deletions": 0, "status": "modified"|"added"|"deleted" } ]` (additions/deletions always 0)
- `GET /vcs/diff?context=3&mode=branch` → `[ { "file", "patch", "additions", "deletions" } ]` (`mode=branch` diffs against `origin/HEAD`)
- `GET /vcs/diff/raw` → raw `text/x-diff` body
- `GET /agent` → `[ { "name", "description"?, "mode": "primary"|"subagent", "native": true, "hidden": false, "color"?, "permission": [] } ]`
  Built-ins: `build` (default, `#034cff`), `plan` (read-only, `#a753ae`), `orchestrator` (board/cron coordination, `#0d9f6f`), `general` (subagent, `#2090f5`), `explore` (subagent, `#fcb239`).
- `GET /command` → `[ { "name", "description", "source": "command", "template", "hints": [] } ]` — built-ins `init`, `review`, `compact` plus config-defined commands.
- `GET /mcp` → `{}`; `GET /skill`, `GET /lsp`, `GET /formatter` → `[]` (stubs)
- `GET /file?path=<dir>&dirs=1` → `[ { "name", "path", "absolute", "type": "file"|"directory", "ignored": false } ]` (`.git`/`node_modules` marked `ignored: true` but still listed)
- `GET /file/content?path=<file>` → `{ "type": "text", "content": "..." }` (relative paths resolve against the instance directory)
- `GET /doc` → OpenAPI stub
- `POST /log` → `true` (no-op)
- TUI stubs (`POST /tui/*`, `GET /tui/control/next`) → `true` (accepted, no-op)

---

## 11. SSE event stream

### 11.1 Endpoints

- `GET /event?directory=` — one instance's hub. Requires auth when a password is set (use `?auth_token=` for `EventSource`).
- `GET /global/event` — **all** directories; each frame wrapped as `{ "directory": "...", "payload": {id, type, properties} }`.

### 11.2 Frame format

Standard SSE: `event: message` (always the literal `message`) + `data: <json>`. The JSON payload is:

```jsonc
{ "id": "evt_...", "type": "<event type>", "properties": { ... } }   // properties never null
```

First frame on connect: `server.connected` `{}`. Heartbeat every 10 s: `server.heartbeat` `{}`. There is **no replay / Last-Event-ID** — on reconnect, re-fetch state via the REST API. Events are dropped (never queued) if a subscriber's buffer fills.

### 11.3 Event catalog

| Event type | `properties` | Notes |
|---|---|---|
| `server.connected` | `{}` | first frame |
| `server.heartbeat` | `{}` | every 10 s |
| `session.created` | `{ "sessionID", "info": Session }` | also fired for subagents, board cards, cron runs, forks, imports |
| `session.updated` | `{ "sessionID", "info": Session }` | any session mutation: rename, cost/tokens after each step, title generation, compaction start/end, revert |
| `session.deleted` | `{ "sessionID", "info": Session }` | |
| `session.status` | `{ "sessionID", "status": {"type":"busy"} \| {"type":"retry","attempt","message","next"} \| {"type":"compact"} \| {"type":"idle"} }` | |
| `session.idle` | `{ "sessionID" }` | a run finished |
| `session.error` | `{ "sessionID", "error": NamedError }` | step failed (aborts do NOT emit this) |
| `session.diff` | `{ "sessionID", "diff": [FileDiff] }` | cumulative per-session file changes |
| `session.compacted` | `{ "sessionID" }` | compaction succeeded |
| `message.updated` | `{ "sessionID", "info": UserMessage \| AssistantMessage }` | upsert by `info.id`; fires for user message saved, assistant created/finished/failed/aborted, compaction messages |
| `message.removed` | `{ "sessionID", "messageID" }` | |
| `message.part.updated` | `{ "sessionID", "part": Part, "time": ms }` | **the upsert channel for every part** — apply by `(messageID, part.id)` |
| `message.part.delta` | `{ "sessionID", "messageID", "partID", "field": "text", "delta": "..." }` | streaming token chunks; append-only, never persisted — reconcile with the next `message.part.updated` for that `partID` |
| `message.part.removed` | `{ "sessionID", "messageID", "partID" }` | |
| `permission.asked` / `permission.replied` | see §4.1 | ⚠ never emitted in the default build (permissions are inert) |
| `question.asked` | `{ "id", "sessionID", "questions": [...], "tool" }` | UI must reply via `POST /question/{id}/reply` |
| `question.replied` | `{ "sessionID", "requestID", "answers": [[string]] }` | |
| `question.rejected` | `{ "sessionID", "requestID" }` | |
| `todo.updated` | `{ "sessionID", "todos": [...] }` | |
| `command.executed` | `{ "name", "sessionID", "arguments" }` | after a slash command finishes |
| `board.updated` | `{ "directory" }` | no card payload — re-fetch `GET /board` |
| `cron.updated` | `{ "directory" }` | re-fetch `GET /cron` |
| `alarm.updated` | `{ "directory" }` | re-fetch `GET /alarm` |

**Client rendering model:** maintain state from REST, then apply events: `message.part.delta` appends to the streaming text buffer; `message.part.updated` upserts the authoritative part; `message.updated` upserts message metadata (cost/tokens/finish/error); `session.updated` refreshes the session record (context usage, title, status).

---

## 12. ACP protocol

ACP is **always on**, multiplexed with HTTP on the same port. It is **not WebSocket**: it is newline-delimited JSON-RPC 2.0 over a raw TCP socket (or stdio in embedded mode).

### 12.1 Connecting

1. Open a plain TCP socket to the server port.
2. The server peeks the **first byte** to classify the connection: `{` (or whitespace) → ACP; anything else (e.g. `G` of `GET`) → HTTP. **You have 30 seconds to send the first byte.**
3. Frames: one JSON object per line, `\n`-terminated. Max frame size **16 MiB**. `id` is a JSON **number**.

### 12.2 Client → agent requests

| Method | Params | Result |
|---|---|---|
| `initialize` | ignored | `{ "protocolVersion": 1, "agentCapabilities": { "loadSession": true, "promptCapabilities": { "embeddedContext": true, "image": true } }, "authMethods": [ { "id": "intellexor-auth", "name": "Intellexor", "description": "..." } ], "agentInfo": { "name": "Intellexor", "version": "..." } }` |
| `authenticate` | ignored | `null` (always succeeds) |
| `session/new` | `{ "cwd": "..." }` (optional) | `{ "sessionId": "ses_...", "modes": { "current": "build", "available": [ {"id":"build","name":"Build"}, {"id":"plan","name":"Plan"}, {"id":"orchestrator","name":"Orchestrator"} ] } }` |
| `session/load` / `session/resume` | `{ "sessionId": "...", "cwd": "..." }` | same shape as `session/new`; **replays history first** as `session/update` notifications (`user_message_chunk` / `agent_message_chunk` / `agent_thought_chunk`) |
| `session/list` | — | `{ "sessions": [ { "sessionId", "cwd", "updatedAt" } ] }` (`updatedAt` = unix **seconds**) |
| `session/set_model` / `session/set_mode` | ignored | `null` (no-ops) |
| `session/prompt` | `{ "sessionId": "...", "prompt": [contentBlock], "messageId"?: "..." }` | `{ "stopReason": "end_turn"|"cancelled"|"max_tokens"|"refusal", "usage": { "cost", "tokens": { "input", "output", "reasoning", "cacheRead", "cacheWrite" } } }` |

`contentBlock`: `{ "type": "text", "text": "..." }` | `{ "type": "image", "data": "<raw base64>", "mimeType": "image/png" }` | `{ "type": "resource", "text"? | "uri"? }` | `{ "type": "resource_link", "uri": "..." }`. Unknown types are silently dropped.

**Streaming:** `session/update` notifications arrive on the same socket **while `session/prompt` is still pending** — read concurrently; match responses by numeric `id`. Requests are handled in per-request goroutines (responses may be out of order).

### 12.3 Agent → client notifications

Envelope: `{"jsonrpc":"2.0","method":"session/update","params":{ "sessionId": "...", "update": {...} }}`

| `update.sessionUpdate` | `update` fields |
|---|---|
| `agent_message_chunk` | `{ "messageId", "content": { "type": "text", "text": <delta> } }` |
| `agent_thought_chunk` | same shape (reasoning deltas) |
| `user_message_chunk` | same shape (history replay of user text) |
| `tool_call` | `{ "toolCallId", "title", "kind", "status": "pending", "rawInput" }` |
| `tool_call_update` | `{ "toolCallId", "title"?, "kind", "status": "in_progress"|"completed"|"failed", "content"?: [ { "type": "content", "content": { "type": "text", "text": ... } } ], "rawInput"?, "rawOutput"? }` |

`kind` mapping: bash/shell→`execute`, webfetch→`fetch`, edit/apply_patch/patch/write→`edit`, grep/glob/context/invalid→`search`, read→`read`, task→`think`, else `other`.

### 12.4 Agent → client requests (the client must answer)

Only one: **`session/request_permission`** (only for sessions created/loaded on that connection; inert in default builds since permissions are auto-granted, but implement it anyway):

```jsonc
// request
{ "jsonrpc":"2.0", "id": 7, "method": "session/request_permission",
  "params": { "sessionId": "...", "toolCall": { "toolCallId": "...", "kind": "execute", "rawInput": {...} },
              "options": [ {"optionId":"once","kind":"allow_once","name":"Allow once"},
                           {"optionId":"always","kind":"allow_always","name":"Always allow"},
                           {"optionId":"reject","kind":"reject_once","name":"Reject"} ] } }
// response
{ "jsonrpc":"2.0", "id": 7, "result": { "optionId": "once" } }
```

Anything but `once`/`always` (or `allow_once`/`allow_always`) is treated as `reject`; an unanswered/failed call auto-rejects.

The server **never** sends `fs/*` or `terminal/*` requests.

### 12.5 Client → agent notifications (no `id`, no response)

- `session/cancel` with `{ "sessionId": "..." }` → aborts that session (the pending `session/prompt` returns `stopReason: "cancelled"`).
- `cancel` (no params) → aborts all sessions of this connection.

### 12.6 Errors

`{ "code": int, "message": string, "data"?: any }`:

| Code | Meaning |
|---|---|
| `-32700` | parse error (invalid JSON line; response has `"id": null`) |
| `-32601` | `method not found: <method>` |
| `-32602` | invalid params |
| `-32000` | server error: session not found (`session %q not found`), prompt failure, or agent error `"<ErrorName>: <message>"` |

---

## 13. Typical client flows

### 13.1 Interactive chat (HTTP + SSE)

1. `GET /global/health` → verify server + version.
2. `GET /event` (or `/global/event`) — open the SSE stream **first**.
3. `POST /session` → create a session (or `GET /session` to list).
4. `POST /session/{id}/prompt_async` with `parts: [{"type":"text","text":"..."}]` → 204.
5. Render from events: `message.updated` (new assistant message), `message.part.delta` (stream text into the UI), `message.part.updated` (authoritative parts, tool rows with status/input/output), `session.status` (busy/retry/idle indicator), `session.updated` (cost/tokens/context).
6. `session.idle` (or `session.status` type `idle`) → turn finished.
7. `POST /session/{id}/abort` to stop.
8. If `question.asked` arrives → show the dock → `POST /question/{id}/reply`.

### 13.2 Board tab

1. `GET /board` → render three columns sorted by `order`.
2. Subscribe to `board.updated` → re-fetch `GET /board` (payload has no cards).
3. Create: `POST /board/card`. Move/reorder/link/schedule: `PATCH /board/card/{id}`. Delete: `DELETE /board/card/{id}`.
4. Start button → `PATCH /board/card/{id}` `{"column":"inprogress"}` — the server creates + prompts the linked session automatically (watch `session.created`).
5. Export/import via `GET /board/export` / `POST /board/import?mode=`.

### 13.3 Cron tab

1. `GET /cron` → list; `cron.updated` → re-fetch.
2. `POST /cron` to create; `GET /cron/preview?spec=` to validate + show next fire time while editing.
3. `PATCH /cron/{id}` for edit/pause/resume; `DELETE /cron/{id}`.
4. "View" jumps to `job.sessions[last]`.

### 13.4 ACP client

1. TCP connect → first byte `{` → `initialize` → `session/new`.
2. `session/prompt`; read `session/update` notifications concurrently; answer `session/request_permission` if asked.
3. `session/cancel` notification to abort; `session/load` + history replay to resume.

### 13.5 Config / settings UI

1. `GET /config` + `GET /config/files` + `GET /config/entries` → render editors.
2. `PUT /config/provider/{id}` / `.../model/{id}` / `PUT /config/agent/{id}` with `{file, entry}` to write; `DELETE ...?file=` to remove.
3. `GET /provider` + `GET /provider/auth` → provider/model pickers; `PUT /auth/{providerID}` `{"type":"api","key":"..."}` to store keys.
4. `GET /agent` → agent picker; `GET /command` → slash-command menu.

---

## 14. Endpoint quick reference

| Method & path | Purpose |
|---|---|
| `GET /global/health` | health + version |
| `GET /event`, `GET /global/event` | SSE streams |
| `GET/POST /session` | list / create |
| `GET/PATCH/DELETE /session/{id}` | read / update / delete |
| `GET /session/status` | busy sessions map |
| `GET /session/{id}/children` | subagent children |
| `GET/DELETE /session/{id}/todo` | todo list |
| `GET /session/{id}/diff` | cumulative file diffs |
| `GET /session/{id}/message` | messages + parts (`?limit=`) |
| `POST /session/{id}/message` | blocking prompt |
| `POST /session/{id}/prompt_async` | async prompt (204) |
| `DELETE /session/{id}/message/{mid}[/part/{pid}]` | delete message / part |
| `POST /session/{id}/abort` | stop the run |
| `POST /session/{id}/shell` | `!` command |
| `POST /session/{id}/command` | slash command |
| `POST /session/{id}/summarize` | compact now |
| `POST /session/{id}/fork` | fork at message |
| `GET /session/{id}/export` / `POST /session/import` | export / import |
| `POST /session/{id}/revert` / `unrevert` | rewind / restore |
| `POST /session/{id}/permissions/{pid}` | reply to permission |
| `GET /permission` + `POST /permission/{id}/reply` | permissions (inert) |
| `GET /question` + `POST /question/{id}/reply|reject` | question dock |
| `GET /board` | cards |
| `POST /board/card`, `PATCH/DELETE /board/card/{id}` | mutate |
| `GET /board/export`, `POST /board/import?mode=` | backup / restore |
| `GET/POST /cron`, `GET /cron/preview?spec=`, `PATCH/DELETE /cron/{id}` | cron jobs |
| `GET /alarm`, `POST /alarm`, `GET /alarm/session/{sid}`, `POST /alarm/session/{sid}/clear-finished`, `POST /alarm/{id}/trigger`, `DELETE /alarm/{id}` | wake-ups |
| `GET/PATCH /config`, `GET /global/config`, `PATCH /global/config` | config |
| `GET /config/providers`, `/config/files`, `/config/entries` | introspection |
| `PUT/DELETE /config/provider/{pid}[/model/{mid}]`, `PUT/DELETE /config/agent/{aid}` | config editing (`?file=` on DELETE) |
| `GET /provider`, `GET /provider/auth`, `PUT/DELETE /auth/{pid}` | providers & keys |
| `GET/POST /workspace`, `POST /workspace/rename` | workspaces |
| `GET /path`, `GET /vcs`, `/vcs/status`, `/vcs/diff`, `/vcs/diff/raw` | paths & git |
| `GET /agent`, `GET /command`, `GET /mcp` | agents, commands, mcp |
| `GET /file`, `GET /file/content` | file browser |
| `GET /doc` | OpenAPI stub |

---

## 15. UI requirements & feature-parity spec

This section specifies **what a complete Intellexor UI must do**. Sections 1–14 define the protocol; this section defines the product. It is derived from the current reference implementation (`web/assets/` — a ~6,600-line vanilla-JS single-page app) and is written so an agent can build a **completely new UI** (any framework) against a **running Intellexor instance** without reading the reference code. Where a requirement maps to an endpoint or event, it is named explicitly.

### 15.1 Deployment & packaging constraints

- **The UI is compiled into the binary.** Assets live in `web/assets/` and are loaded via `go:embed` (`web/embed.go`). There is **no dev server and no hot reload** — any asset change requires `go build`. A replacement UI must either replace the files in `web/assets/` (keeping `embed.go`) or be served by the same mechanism.
- **One port serves everything.** The UI, the HTTP API, and ACP share a single socket (default 4096; the URL the server prints is the source of truth). The UI must use **relative URLs** — it may be served from any host/port.
- **Directory routing is a query param, not a path prefix.** All API calls append `?directory=<abs path>` (§1.3); the UI never relies on URL-path-based instance routing.
- **Desktop mode:** when built with the `desktop` tags, the same UI runs inside a Wails webview. The reference UI detects this and swaps `fetch`/`EventSource` for the Wails bridge (`window.go.main.Api.Call`, `window.runtime.EventsOn("server-event", …)`). A new UI may ignore this and use plain HTTP/SSE — the server side is identical — but must not break when `window.go` is absent.
- **No build toolchain is required** (the reference UI is framework-free with two vendored libs: `marked` for markdown and `DOMPurify` for sanitization). A new UI may use a bundler, but its output must land in `web/assets/` and be committed.
- **Static assets that must exist** (served from the embedded FS): `/logo.png` (favicon + branding), `/notification.mp3` (completion sound), and — only if speech-to-text is kept — `/stt-worker.js` plus the `/stt/` ONNX bundle. Everything else is optional.

### 15.2 Information architecture & layout

Required top-level structure (names are free; behavior is not):

1. **Icon rail** (far left, ~64px): brand/logo, sidebar toggle, board-panel toggle, current workspace avatar, other open workspaces, new-workspace button, settings button.
2. **Board panel** (optional left panel, ~300px): compact list of `open` + `inprogress` kanban cards; click opens the board.
3. **Session sidebar** (~280px): workspace title, "New session" button, session list grouped by day (Today / Yesterday / Older), sorted by `time.updated` descending, with an "Archived (N)" collapsible section.
4. **Main area**: everything else — workspace tab bar, session tab bar, and the active view.
5. Overlays: modal root and a toast stack (bottom-right).

**Views** (hash routing is the reference approach; any router is fine):

| View | Trigger | Content |
|---|---|---|
| Landing / new session | `#/` | Brand mark, hint text, composer. Sending creates the session lazily. |
| Chat | `#/{sessionID}` | Header + timeline + composer. |
| Subagent (read-only) | `#/{childSessionID}` | Same timeline, no composer; "← Back to parent" + "subagent · read-only" badge. |
| Kanban board | `#/board` | Fixed first tab in the session tab bar. |
| Cron jobs | `#/cron` | Fixed last tab. |

**Tab bars:**

- **Workspace tabs** — one per open working directory; persisted client-side; switching workspaces must **not** reload the page (tear down directory-scoped state, refetch boot data, reconnect SSE with the new `?directory=`). Show an "unseen activity" dot per workspace (compare last-visited timestamp against `GET /workspace`'s `busy`/`lastActivity`).
- **Session tabs** — Board (fixed first), one tab per open session (persisted per workspace), "+" (new session), Cron (fixed last). Tab indicators: spinner (busy), yellow dot (pending permission), red dot (error), unseen dot, ⏰ (pending alarm). Middle-click closes; right-click renames. A "pending" tab may be created client-side before the session exists; the backing session is created via `POST /session` immediately or lazily on first send, then the placeholder id is swapped.

**Responsive:** below 768px the sidebar and board panel become off-canvas drawers with a backdrop; the rail shrinks; the timeline/composer tighten. The app must be usable at phone widths.

### 15.3 Design system

The reference implementation defines a token-based theme; a new UI should keep the *mechanism* (semantic tokens re-declared per theme on `<html data-color-scheme="…">`) even if it changes the values.

- **Theme switching:** three-state (system / light / dark), persisted in `localStorage`. `system` follows `matchMedia("(prefers-color-scheme: dark)")` with a live change listener. The active theme is applied by setting `data-color-scheme` on `<html>` — **not** via `@media` queries — so JS can resolve "system" itself.
- **Required semantic tokens** (per theme): surfaces (`bg-base`, `bg-deep`, `layer-1..4`), text (`text-base`, `text-muted`, `text-faint`, `text-inverse`), borders (`border-weak/base/strong`), accent + accent-fg, hover/active surfaces, status pairs (ok/warn/err as bg+fg), diff tints (add/del), code background, shadow, and a glass token (`rgba` surface + `backdrop-filter: blur`) for floating panels.
- **Reference palette:** grey ramp `#080808 → #ffffff` (13 steps); accent `#3b5cf6` (light) / `#a2bcff` (dark); diff add `#12c905` / del `#fc533a`; fonts Inter (UI) + JetBrains Mono (mono); radii 4–12px; base font 14px/150%.
- **Motion:** subtle — hover transitions ~.15s, panel collapse .18s, modal/toast entrances .15–.2s, streaming "shimmer" and bouncing-dots indicators. **All animation must be disabled under `prefers-reduced-motion: reduce`.**
- **Scrollbars:** custom, thin (8px), theme-aware.

### 15.4 Chat view — composer

The composer is the heart of the app. Requirements:

- **Input:** auto-growing textarea (max ~240px), Enter sends, Shift+Enter newlines, IME-composition-safe (don't send while composing).
- **Per-session drafts:** text and attachments are kept per session (and for the landing view) and survive tab switches and re-renders; caret/focus must be preserved across re-renders.
- **Attachments:** "+" button **and paste** (images/files) → chips with remove buttons; sent as `{type:"file", mime, url (data URL), filename}` parts (§3.14).
- **Three send modes** (§3.14, §3.17, §3.18):
  - text starting with `!` → `POST /session/{id}/shell` (visual "shell mode" styling while the draft starts with `!`);
  - text starting with `/` matching a command from `GET /command` → `POST /session/{id}/command`;
  - otherwise → `POST /session/{id}/prompt_async` with `{agent, model?, parts}`.
- **Slash-command menu:** typing `/` opens a filterable popover of `GET /command` results plus client-side UI commands (`/undo`, `/redo`, `/new`, `/models`); keyboard navigation (↑/↓/Enter/Tab/Escape); commands whose template contains `$ARGUMENTS`/`$1` keep the `/name ` prefix so arguments can be typed.
- **Agent selector:** from `GET /agent` (build/plan/orchestrator/general/explore), persisted per workspace.
- **Model selector:** searchable popover over `GET /provider` (grouped by provider, sorted by `release_date` desc), persisted per workspace.
- **Send/stop toggle:** while the session is busy the send button becomes **Stop** → `POST /session/{id}/abort`; **Escape also aborts**. After aborting, if the session is still busy ~1.5s later, toast "Abort requested — waiting for the running tool to exit…".
- **Docks above the composer** (priority order: permissions > questions > todos; plus the mic dock when recording) — see §15.9.
- **Hint row:** current git branch from `GET /vcs` (or "No Git").

### 15.5 Chat view — timeline

- **Load:** `GET /session/{id}/message?limit=100` — only the last ~100 *display units* are kept (a user prompt, response text block, thinking block, or tool call each count as one; synthetic/ignored parts are free). SSE events for messages older than the loaded window must be **ignored** to prevent out-of-order appends. Re-fetches must not clobber locally buffered streaming text (longer-text-wins).
- **Render from events** (§11.3): `message.updated` (new message), `message.part.delta` (stream text — throttle re-renders to ~120ms), `message.part.updated` (authoritative parts; patch in place when possible), `message.removed`, `session.status`, `session.idle`, `session.error`.
- **User messages:** plain-text bubble (escaped, *not* markdown), attachment chips (image thumbnails), hover-revealed meta (agent · model, time), **Copy** button, and **Revert** button → `POST /session/{id}/revert {messageID}` (puts the message text back into the composer and reloads). When `sess.revert` is set, show a **revert banner** with Restore → `POST /session/{id}/unrevert`.
- **Assistant messages:** markdown rendering (GFM, `breaks: true`) **sanitized with DOMPurify**; fenced code blocks get a copy button; links open in new tabs (`rel="noopener"`). A **Fork** button → `POST /session/{id}/fork {messageID}` → navigate to the cloned session. On error, an error card. Completed messages show a meta line (agent · model · duration).
- **Part types to render** (§2.3):
  - `text` — markdown bubble; skip `synthetic`/`ignored`/empty parts.
  - `reasoning` — collapsible "🧠 Thinking [N chars]" row, collapsed by default; open/closed state must survive re-renders; live char count while streaming.
  - `tool` — see below.
  - `file` — attachment chip row.
  - `retry` — "retrying — attempt N" line.
  - `compaction` — "Session compacted" divider; a message with `info.summary` renders the streaming summary.
- **Tool call rows:** header (icon + friendly name + sub-label + animated ellipsis while running + chevron); `+N −M` changes badge from `state.metadata.filediff`; collapsible body **lazily filled on first open**; user's open/closed choice survives re-renders; errored tools (`state.status === "error"`) default open with an error card. Per-tool minimum:
  - `bash` → "Shell", command as sub-label, `<pre>` body with `$ command` + `metadata.output`.
  - `read` → "Read", file basename, output body.
  - `edit`/`write`/`apply_patch` → **diff body** (render `metadata.diff` unified-diff with add/del/hunk styling), **default open**.
  - `task` → subagent row; if `state.metadata.sessionId` exists the row links to the child's read-only session view (spinner while the child is busy).
  - `todowrite` → **not rendered in the timeline** (todos appear in the dock).
  - `grep`/`glob`/`webfetch`/`question`/`invalid` → friendly labels; unknown tools → capitalized name + output body.
- **Thinking indicator:** while status is `busy`/`retry`/`compact`, show a shimmer row ("Thinking" + animated dots + right-aligned `agent · model` label); `retry` shows "retrying in Ns — attempt #N" from `status.next`/`status.attempt`; `compact` shows "Compacting".
- **Autoscroll:** stick to bottom when within ~60px of it; floating "jump to latest" button when scrolled up >200px.
- **No message editing/deletion UI is required** (the endpoints exist, §3.11–3.12, but the reference UI only exposes revert).

### 15.6 Session management

- Create (`POST /session`), rename (`PATCH /session/{id} {title}` — via context menu or double-click on the header title), archive (`PATCH {time:{archived: <ms>}}`; restore with `0`), delete (`DELETE /session/{id}` with confirm).
- Sidebar rows show status indicators: spinner (busy/retry/compact), yellow dot (pending permission), red dot (errored), unseen dot (new user message while not viewing), ⏰ (pending alarm).
- **Context/cost display:** header SVG ring showing `sess.context` (fallback: last assistant message tokens) vs the model's `limit.context`; click opens a popover with Total Tokens, Context, Context Limit, Usage %, Input/Output/Reasoning/Cache Read/Cache Write, and Total Cost (`sess.cost`). Abbreviate numbers (k/M).
- **Export:** `GET /session/{id}/export` → download `intellexor-session-{slug}.json`. **Import:** only while the session is empty → file picker → `POST /session/import` → navigate to the new session.
- **Subagents:** `GET /session/{id}/children`; child sessions are viewable read-only.
- Last-open chat per workspace is restored on boot.

### 15.7 Kanban board

- Three columns `open` / `inprogress` / `done` (§5), cards sorted by `order`; per-column collapse persisted (collapsed columns become vertical buttons in a rail, still droppable). Reload on `board.updated`.
- **Create:** header modal **and** an inline "Add card…" textarea pinned to the bottom of the Open column — both support attachments/paste; `POST /board/card {title, column:"open", attachments}`. Drafts survive re-renders. Cards can only be created in Open. Enter creates; Ctrl/Shift+Enter newline.
- **Edit/delete:** Open cards only — edit modal or double-click title (contenteditable) → `PATCH /board/card/{id}`; delete with confirm.
- **Move:** HTML5 drag & drop with insert-before computed from pointer position; optimistic local reorder, then `PATCH {column, before}`. Moving out of In Progress asks "Stop the agent?".
- **Start:** Open cards get **Start** → `POST /session` → `PATCH {column:"inprogress", sessionID}` → `POST /session/{sid}/prompt_async` with the card title + attachments → open a tab without switching. Cards with `sessionID` get **View**; in-progress cards show a spinner while their session is busy.
- **Schedule:** Open cards get **Schedule** → datetime picker + presets (1h/2h/8h/24h) → `PATCH {startAt}` (unix ms; 0 removes). Show a 🕐 badge with humanized time; the server starts the card.
- **Linking (follow-ups):** each card has a draggable link pill; dropping it on an Open card sets `PATCH {link: dst}` — when the source is done, the target auto-starts. Render animated dashed SVG bezier curves between linked cards (border-to-border, highlighted on hover); redraw on resize/scroll. Click the pill to unlink (`link: ""`).
- **Watchers:** show 👁 + count (`c.watchers`).
- **Export/import:** `GET /board/export`; import via file picker with an explicit **Merge** or **Replace** choice → `POST /board/import?mode=merge|replace`.
- **Orchestrator chat column:** a full chat panel pinned to agent `orchestrator`, living in a real session (id persisted per project). Reuses the timeline renderers, has its own composer/streaming/abort, is collapsible to a thin vertical strip and horizontally resizable (~230–720px). "+" offers chat history (previous orchestrator sessions) or New chat.

### 15.8 Cron & alarms

- **Cron tab** (§6): rows with a running indicator (⏳ while any run session is busy), title, "paused" badge, spec, "next {rel}", "last {rel}", "N runs", prompt preview. Actions: Pause/Play (`PATCH {paused}` — pause holds only the next run), View (newest busy run session, else last), Edit, Delete (confirm). Create/edit modal: title, prompt, schedule field accepting cron expressions / `@macros` / `@every <duration>`, presets, and **live validation** via debounced `GET /cron/preview?spec=…` showing the next fire time or an error. Reload on `cron.updated`.
- **Alarms** (§7): `GET /alarm` (map sessionID → earliest `triggerAt`, **unix seconds**) drives ⏰ indicators in header/tabs/sidebar. ⏰ opens a modal listing the session's alarms (`GET /alarm/session/{sid}`) with live countdowns, **Trigger now** (`POST /alarm/{id}/trigger`), and Delete. "Add scheduled message" → note + datetime picker + presets (5/15/30 min, 1h, Tomorrow 9:00) → `POST /alarm {sessionID, triggerAt (unix s), note}` — the session receives an `alarm-triggered` user message at that time.

### 15.9 Docks: permissions, questions, todos

All docks render **above the composer**, only for the current session or its task children.

- **Permission dock** (⚠ inert in the default build — implement anyway, it's harmless): "Permission required" + human description + pattern chips; buttons **Deny / Allow always / Allow once** → `POST /permission/{id}/reply {reply: "reject"|"always"|"once"}`. Events: `permission.asked` / `permission.replied`.
- **Question dock** (live — the `question` tool, §4.2): a multi-question wizard — "N of M" progress, question text, option buttons (single- or multi-select per `multiple`), free-text input for custom answers, Back/Next/Submit, and **Dismiss** (`POST /question/{id}/reject`). Wizard state (answers/step/draft) must survive composer re-renders. Submit → `POST /question/{id}/reply {answers}`. Events: `question.asked` / `question.replied` / `question.rejected`.
- **Todos dock:** "X of N todos completed" checklist with ✓/current styling; clear button (`DELETE /session/{id}/todo`) when idle. Event: `todo.updated`; initial load `GET /session/{id}/todo`.

### 15.10 Settings / config UI

Two-pane modal (side nav: **General / Providers / Agents**; Models reached per-provider):

- **General:** color scheme segmented control (system/light/dark); animated-background on/off; server version from `GET /global/health`.
- **Providers:** list from `GET /config/entries` (name, model count, type badge, file path). Add/Edit form: ID, type (`@ai-sdk/openai-compatible`), display name, endpoint URL (`options.baseURL`), API key (`options.apiKey`), target config file (`GET /config/files`; default to the closest existing project file). Save: `PUT /config/provider/{id} {file, entry}` (whole-entry replace; preserve unknown fields). Remove: `DELETE /config/provider/{id}?file=…` with confirm.
- **Models:** per-provider list with capability badges (tools/reasoning/attachments/temperature) and limits. Add/Edit: model ID, name, capability checkboxes, context/output limits → `PUT /config/provider/{id}/model/{mid} {file, entry}`; `DELETE …?file=…`.
- **Agents:** built-in agents (read-only, from `GET /agent`'s `data.builtin`) plus configured agents. Add/Edit: name, description, model (`provider/model`), mode (subagent/primary/all), system prompt, tool-group checkboxes, config file → `PUT /config/agent/{name} {file, entry}`; `DELETE /config/agent/{id}?file=…`.
- After any config write, re-fetch `GET /provider` (and auto-pick a default model if none is selected).

### 15.11 Extras

- **Speech-to-text (optional but expected):** mic button in the composer; click-to-record, click-again-to-stop; transcription appended to the draft. The reference implementation uses transformers.js (ONNX, NVIDIA Parakeet) in a module worker served from `/stt/`; long recordings are decoded in silence-aligned ~25 s segments (the worker splits at the quietest point near each boundary, so no words are clipped) with `decode-progress` messages driving the dock. Capture goes through an AudioWorklet whose (silent) output is routed through a zero-gain node into the destination — engines only pull nodes reachable from the destination, and a zero-output capture node is never processed on some of them. Dock states: downloading (progress + cancel), recording (timer), decoding (segment progress). Requires a secure context. A new UI may omit STT or swap the engine, but the mic dock states are the UX baseline.
- **Notifications:** completion sound (`/notification.mp3`) played when a top-level session goes idle after being busy — **unlocked by the first user gesture** (browser autoplay policy) and suppressed for ~10s after a user-initiated abort. No desktop notifications: instead a **tab progress indicator** (animated favicon + spinner in `document.title` while the current session runs; amber dot when other sessions work; red dot on error).
- **Animated background (optional):** canvas aurora + dot grid + vignette behind the app; full strength on the landing view, dimmed elsewhere; pausable (`visibilitychange`), static under `prefers-reduced-motion`, theme-aware.
- **Keyboard:** Enter/Shift+Enter (send/newline), Escape (abort / close menus/modals), command-menu arrows, middle-click tab close. No global hotkey layer required.
- **Toasts:** bottom-right stack for errors and confirmations (e.g. abort requests).

### 15.12 State, reconnection & performance requirements

- **Boot sequence:** health check (`GET /global/health`, with a "Cannot reach Intellexor server" fallback page) → `GET /path` → `GET /vcs` → `GET /session` (filter out subagents) → restore tabs & last session → `GET /alarm` → `GET /session/status` → `GET /permission` → `GET /question` → `GET /provider` → `GET /command` → connect SSE → route.
- **SSE:** one directory-scoped stream (`GET /event?directory=…`) plus a global stream (`GET /global/event`) for cross-workspace activity dots. `EventSource` auto-reconnects; on `server.connected` the client must refetch `GET /session/status` and re-render everything so indicators recover after downtime.
- **State model:** a single in-memory store keyed by session id (`{order, info, parts}` per session) is sufficient; rendering may be full-rebuild per view, **but** streaming text should be patched in place (throttled ~120ms) and re-renders must preserve composer focus/caret, open menus, contenteditable edits, and unsaved drafts.
- **Markdown cache:** memoize rendered markdown (the reference uses an LRU of ~400 entries) — long sessions re-render often.
- **Timezone:** send an `X-Timezone` header with API calls (the server uses it for scheduling display).

### 15.13 Feature-parity checklist

A new UI is feature-complete when all of the following are true:

- [ ] Landing view with lazy session creation; chat view; read-only subagent view
- [ ] Workspace tabs (switch without reload, unseen dots) and session tabs (all five indicators, pending-tab promotion, middle-click close, right-click rename)
- [ ] Composer: drafts per session, attachments + paste, `!` shell mode, `/` command menu with args, agent & model pickers, send/stop toggle, Escape-to-abort
- [ ] Timeline: streaming deltas (throttled), markdown + sanitized code blocks with copy, reasoning rows, per-tool rows with lazy bodies and diffs, thinking/retry/compaction indicators, autoscroll + jump-to-latest, 100-unit window with out-of-order protection
- [ ] Message actions: copy, revert (+ banner/restore), fork
- [ ] Session: create/rename/archive/delete, day-grouped sidebar with status dots, context/cost ring + popover, export/import
- [ ] Docks: permission (Deny/Allow once/Allow always), question wizard with dismiss, todos with clear
- [ ] Board: 3 columns, drag & drop with insert-before, add-card inline + modal with attachments, edit/delete, Start/Schedule/View, link pills + SVG link lines, watchers, collapse, export/import (merge/replace), orchestrator chat column
- [ ] Cron tab: rows with running indicator, pause/play, live spec validation via preview endpoint, create/edit/delete
- [ ] Alarms: ⏰ indicators, alarm list modal with countdowns, trigger-now, scheduled-message modal
- [ ] Settings: theme + background toggles, provider/model/agent CRUD against the config endpoints
- [ ] SSE reconnection recovery on `server.connected`; global stream for workspace dots
- [ ] Sound notification with gesture unlock + abort suppression; tab progress indicator
- [ ] Responsive down to phone widths; `prefers-reduced-motion` honored
- [ ] Works from any host/port with `?directory=` routing and relative URLs; compiles into the binary via `web/embed.go`
