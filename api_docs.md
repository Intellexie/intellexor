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
| `GET /alarm`, `POST /alarm`, `GET /alarm/session/{sid}`, `POST /alarm/{id}/trigger`, `DELETE /alarm/{id}` | wake-ups |
| `GET/PATCH /config`, `GET /global/config`, `PATCH /global/config` | config |
| `GET /config/providers`, `/config/files`, `/config/entries` | introspection |
| `PUT/DELETE /config/provider/{pid}[/model/{mid}]`, `PUT/DELETE /config/agent/{aid}` | config editing (`?file=` on DELETE) |
| `GET /provider`, `GET /provider/auth`, `PUT/DELETE /auth/{pid}` | providers & keys |
| `GET/POST /workspace`, `POST /workspace/rename` | workspaces |
| `GET /path`, `GET /vcs`, `/vcs/status`, `/vcs/diff`, `/vcs/diff/raw` | paths & git |
| `GET /agent`, `GET /command`, `GET /mcp` | agents, commands, mcp |
| `GET /file`, `GET /file/content` | file browser |
| `GET /doc` | OpenAPI stub |
