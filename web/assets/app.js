"use strict";

/* ============================== api ============================== */

// The current workspace (working directory). Mutable: switching workspaces
// re-points it in-place (see switchWorkspace) — no page reload. Everything
// DIR-scoped (api()'s directory param, wsGet/wsSet keys, SSE URL) reads it
// at call time, so it follows automatically.
let DIR = localStorage.getItem("intellexor.dir") || "";

// Per-workspace localStorage: tabs, model/agent picks etc. are scoped to the
// current workspace so switching workspaces swaps the whole UI state with it.
// The first read of a key migrates the legacy (global) value, so existing
// users keep their tabs and preferences.
function wsKey(key) { return "intellexor." + key + "." + (DIR || "default"); }
function wsGet(key) {
  const k = wsKey(key);
  let v = localStorage.getItem(k);
  if (v === null) {
    v = localStorage.getItem("intellexor." + key);
    if (v !== null) { try { localStorage.setItem(k, v); } catch {} }
  }
  return v;
}
function wsSet(key, val) { try { localStorage.setItem(wsKey(key), val); } catch {} }

// Desktop mode (Wails webview): HTTP through the webview scheme handler is
// not viable, so API calls go through the bound Go bridge
// (window.go.main.Api — the same http.Handler the HTTP server uses,
// in-process) and events arrive over the Wails events runtime (see
// initDesktopEvents). Web mode keeps fetch + EventSource; the HTTP server
// itself is unchanged and still serves external clients.
const DESKTOP = !!(window.go && window.go.main && window.go.main.Api);

async function api(method, path, body) {
  if (DESKTOP) {
    let tz = "";
    try { tz = Intl.DateTimeFormat().resolvedOptions().timeZone; } catch {}
    let p = path;
    if (DIR && !/[?&]directory=/.test(path)) {
      p += (path.includes("?") ? "&" : "?") + "directory=" + encodeURIComponent(DIR);
    }
    const r = await window.go.main.Api.Call(method, p, body === undefined ? "" : JSON.stringify(body), tz);
    if (r.status === 204) return null;
    const text = r.body || "";
    if (!text) {
      if (r.status >= 400) throw new Error("HTTP " + r.status);
      return null;
    }
    let v;
    try { v = JSON.parse(text); } catch { v = text; }
    if (r.status >= 400) {
      const e = new Error((v && v.data && v.data.message) || "error");
      e.payload = v;
      throw e;
    }
    return v;
  }
  const url = new URL(path, location.origin);
  if (DIR) url.searchParams.set("directory", DIR);
  const opts = { method, headers: {} };
  // the server records this on the session so tools can report times in the
  // user's device timezone instead of the server's
  try { opts.headers["X-Timezone"] = Intl.DateTimeFormat().resolvedOptions().timeZone; } catch {}
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(url, opts);
  if (res.status === 204) return null;
  const text = await res.text();
  if (!text) {
    if (!res.ok) throw new Error(res.statusText);
    return null;
  }
  const v = JSON.parse(text);
  if (!res.ok) {
    const e = new Error((v && v.data && v.data.message) || "error");
    e.payload = v;
    throw e;
  }
  return v;
}

const get = (p) => api("GET", p);
const post = (p, b) => api("POST", p, b === undefined ? {} : b);
const patch = (p, b) => api("PATCH", p, b);
const del = (p) => api("DELETE", p);

/* ============================== state ============================== */

const S = {
  dir: "",
  version: "", // server version from /global/health
  vcs: {},
  sessions: [],
  statuses: {},
  permissions: [],
  questions: [],
  providers: { all: [], default: {}, connected: [] },
  current: null, // session id
  view: "sessions", // "sessions" | "board" (the kanban first tab)
  msgs: new Map(), // sid -> {order:[ids], info:{id->info}, parts:{id->part}}
  todos: {},
  children: {},
  busy: new Set(),
  model: JSON.parse(wsGet("model") || "null"), // {providerID, modelID}
  agent: wsGet("agent") || "build",
  scheme: localStorage.getItem("intellexor.scheme") || "system",
  bg: localStorage.getItem("intellexor.bg") !== "0", // animated background on/off (global pref)
  unseen: new Set(),
  errored: new Set(),
  scrollStick: true,
  streaming: new Map(), // partID -> messageID (parts with live text deltas)
  toolOpen: new Map(), // tool partID -> user's explicit open/closed choice (survives timeline re-renders)
  reasoningOpen: new Map(), // reasoning partID -> user's explicit open/closed choice (survives timeline re-renders)
  tabs: [], // open session tabs, in order (persisted)
  alarms: {}, // sessionID -> earliest pending wake-up triggerAt (unix seconds) — drives the ⏰ indicator and its hover title
  sidebarOpen: true, // resolved from localStorage/viewport by initSidebar()
  boardPanelOpen: false, // cards side panel (rail toggle), resolved by initBoardPanel()
  commands: null, // cached GET /command result (slash-command menu)
  subagents: new Map(), // sessionID -> info for subagent sessions (fetched on demand, not in the sidebar)
  es: null,
};

function applyScheme() {
  let s = S.scheme;
  if (s === "system") s = matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  document.documentElement.dataset.colorScheme = s;
}

// Applies the animated-background preference: hides the whole #bg layer
// (canvas, grid, vignette) via body.bg-off and stops/starts the canvas
// painter. bg.js also reads the same localStorage key on load so the
// background never flashes on before app.js boots.
function applyBackground() {
  document.body.classList.toggle("bg-off", !S.bg);
  if (window.IntellexorBG) IntellexorBG.setActive(S.bg);
}

/* ============================== utils ============================== */

const $ = (sel, root) => (root || document).querySelector(sel);
const el = (tag, cls, html) => {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (html !== undefined) e.innerHTML = html;
  return e;
};
const esc = (s) =>
  String(s == null ? "" : s)
    .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
const fmtTime = (ms) =>
  new Date(ms).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });

function groupDay(ms) {
  const d = new Date(); d.setHours(0, 0, 0, 0);
  const today = d.getTime();
  if (ms >= today) return "Today";
  if (ms >= today - 86400000) return "Yesterday";
  return "Older";
}

function toast(msg, isErr) {
  const t = el("div", "toast" + (isErr ? " err" : ""));
  t.textContent = msg;
  $("#toast-root").appendChild(t);
  setTimeout(() => t.remove(), isErr ? 6000 : 3000);
}

/* ============================== notification sound ============================== */

// Played when the agent finishes working (see the session.idle handler).
// Browsers block audible playback until the user has interacted with the page,
// so the first pointer/key gesture "unlocks" the element with a muted
// play/pause; plays that are still blocked resolve as silent no-ops.
const Notify = (() => {
  const audio = new Audio("/notification.mp3");
  audio.preload = "auto";
  let unlocked = false;
  const unlock = () => {
    if (unlocked) return;
    unlocked = true;
    audio.muted = true;
    audio.play().then(() => {
      audio.pause();
      audio.currentTime = 0;
      audio.muted = false;
    }).catch(() => { audio.muted = false; });
  };
  addEventListener("pointerdown", unlock, { once: true });
  addEventListener("keydown", unlock, { once: true });
  return {
    play() {
      try {
        audio.currentTime = 0;
        const p = audio.play();
        if (p && p.catch) p.catch(() => {});
      } catch {}
    },
  };
})();

// Timestamp until which the completion chime is muted — set when the user
// aborts a run themselves (they are already looking at the UI).
let soundSuppressedUntil = 0;

// Sessions whose idle session.status arrived while they were still marked
// busy. The server publishes the idle status before session.idle, and the
// status handler deletes the busy entry — so session.idle must recover the
// "was actually working" bit from here to decide whether to chime.
const idleAfterBusy = new Set();

const svg = (d, size = 14) =>
  `<svg width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">${d}</svg>`;
const ICONS = {
  edit: svg('<path d="M12 20h9"/><path d="M16.5 3.5a2.121 2.121 0 0 1 3 3L7 19l-4 1 1-4L16.5 3.5z"/>'),
  archive: svg('<rect x="2" y="4" width="20" height="5" rx="1"/><path d="M4 9v9a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9"/><path d="M10 13h4"/>'),
  chev: svg('<path d="m9 18 6-6-6-6"/>', 12),
  plus: svg('<path d="M12 5v14M5 12h14"/>'),
  up: svg('<path d="M12 19V5M5 12l7-7 7 7"/>', 16),
  down: svg('<path d="M12 5v14M19 12l-7 7-7-7"/>', 14),
  stop: svg('<rect x="6" y="6" width="12" height="12" rx="2"/>', 14),
  warn: svg('<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z"/><path d="M12 9v4M12 17h.01"/>'),
  spinner: '<svg class="spin" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3"><path d="M21 12a9 9 0 1 1-6.219-8.56"/></svg>',
  file: svg('<path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z"/><path d="M14 2v5h5"/>', 12),
  folder: svg('<path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/>', 14),
  console: svg('<path d="m4 17 6-6-6-6"/><path d="M12 19h8"/>', 12),
  branch: svg('<line x1="6" y1="3" x2="6" y2="15"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><path d="M18 9a9 9 0 0 1-9 9"/>', 12),
  copy: svg('<rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/>', 12),
  play: svg('<polygon points="6 3 20 12 6 21 6 3"/>', 12),
  eye: svg('<path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/>', 12),
  revert: svg('<path d="M9 14 4 9l5-5"/><path d="M4 9h10.5a5.5 5.5 0 0 1 5.5 5.5v0a5.5 5.5 0 0 1-5.5 5.5H11"/>', 12),
  fork: svg('<circle cx="12" cy="18" r="3"/><circle cx="6" cy="6" r="3"/><circle cx="18" cy="6" r="3"/><path d="M18 9v2a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2V9"/><path d="M12 12v3"/>', 12),
  restore: svg('<path d="M3 3v5h5"/><path d="M3.05 13A9 9 0 1 0 6 5.3L3 8"/><path d="M12 7v5l4 2"/>', 14),
  close: svg('<path d="M18 6 6 18M6 6l12 12"/>', 12),
  mic: svg('<path d="M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z"/><path d="M19 10v2a7 7 0 0 1-14 0v-2"/><line x1="12" y1="19" x2="12" y2="22"/><line x1="8" y1="22" x2="16" y2="22"/>'),
  board: svg('<rect width="18" height="18" x="3" y="3" rx="2"/><path d="M8 7v7"/><path d="M12 7v4"/><path d="M16 7v9"/>', 12),
  target: svg('<circle cx="12" cy="12" r="10"/><circle cx="12" cy="12" r="6"/><circle cx="12" cy="12" r="2"/>', 12),
  clock: svg('<circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/>', 12),
  pause: svg('<rect x="6" y="4" width="4" height="16" rx="1"/><rect x="14" y="4" width="4" height="16" rx="1"/>', 12),
  trash: svg('<polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/><line x1="10" y1="11" x2="10" y2="17"/><line x1="14" y1="11" x2="14" y2="17"/>', 12),
};

/* ============================== tab progress ============================== */

// Progress indicator for the browser tab: an animated ring around the favicon
// plus a braille spinner in the title while the current session is running,
// a dot while other sessions are busy, and a red dot on error.
const TabProgress = (() => {
  const link = document.querySelector('link[rel="icon"]');
  const BASE = "/logo.png";
  const img = new Image();
  img.src = BASE;
  const SPIN = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];
  const ACCENT = "#5f8bff", WARN = "#fcb239", ERR = "#ff5f56";
  let timer = 0, frame = 0, mode = "idle", baseTitle = "Intellexor";

  function paint(draw) {
    if (!link) return;
    const c = document.createElement("canvas");
    c.width = c.height = 64;
    draw(c.getContext("2d"));
    link.href = c.toDataURL("image/png");
  }

  function logo(ctx, size, alpha) {
    ctx.globalAlpha = alpha;
    if (img.complete && img.naturalWidth) {
      ctx.drawImage(img, (64 - size) / 2, (64 - size) / 2, size, size);
    }
    ctx.globalAlpha = 1;
  }

  function ring(t) {
    paint((ctx) => {
      logo(ctx, 38, 0.35);
      const rot = t * 2 * Math.PI;
      const sweep = Math.PI * (0.75 + 0.45 * Math.sin(t * 2 * Math.PI));
      ctx.lineWidth = 5;
      ctx.lineCap = "round";
      ctx.strokeStyle = ACCENT;
      ctx.beginPath();
      ctx.arc(32, 32, 28, rot, rot + sweep);
      ctx.stroke();
    });
  }

  function dot(color) {
    paint((ctx) => {
      logo(ctx, 64, 1);
      ctx.fillStyle = color;
      ctx.strokeStyle = "rgba(0,0,0,0.4)";
      ctx.lineWidth = 2;
      ctx.beginPath();
      ctx.arc(50, 50, 11, 0, 2 * Math.PI);
      ctx.fill();
      ctx.stroke();
    });
  }

  function render() {
    if (mode === "busy") {
      ring(frame / 20);
      document.title = SPIN[frame % SPIN.length] + " " + baseTitle;
    } else {
      if (mode === "working") dot(WARN);
      else if (mode === "error") dot(ERR);
      else if (link) link.href = BASE;
      document.title = baseTitle;
    }
  }

  return {
    set(m) {
      if (m === mode || !link) return;
      mode = m;
      if (timer) { clearInterval(timer); timer = 0; }
      if (mode === "busy") timer = setInterval(() => { frame++; render(); }, 100);
      else frame = 0;
      render();
    },
    setTitle(t) { baseTitle = t; if (!timer) render(); },
  };
})();

function updateTab() {
  const busy = (id) => {
    const st = S.statuses[id];
    return !!(st && st.type !== "idle");
  };
  let mode = "idle";
  if (S.current && busy(S.current)) mode = "busy";
  else if (Object.keys(S.statuses).some(busy)) mode = "working";
  else if (S.current && S.errored.has(S.current)) mode = "error";
  TabProgress.set(mode);
}

/* ============================== markdown ============================== */

// Full markdown via the vendored marked parser (web/assets/marked.min.js),
// sanitized with DOMPurify (web/assets/purify.min.js) — model output is
// untrusted, so nothing reaches innerHTML without passing the allowlist.
const md = new marked.Marked({
  gfm: true, // tables, strikethrough, task lists, autolinks
  breaks: true, // single newlines become <br> (chat-style soft wrap)
  renderer: {
    code({ text, lang }) {
      return `<pre><button class="copy-code" type="button">copy</button><code data-lang="${esc(lang || "")}">${esc(text)}</code></pre>`;
    },
    codespan({ text }) {
      return `<code class="inline">${esc(text)}</code>`;
    },
    link({ href, title, tokens }) {
      const t = title ? ` title="${esc(title)}"` : "";
      return `<a href="${esc(href)}"${t} target="_blank" rel="noopener">${this.parser.parseInline(tokens)}</a>`;
    },
  },
});

// Rendered markdown is memoized by source: full timeline rebuilds re-render
// every part, but during a run only one or two parts actually change — the
// rest become cache hits instead of marked.parse + DOMPurify round-trips.
const mdCache = new Map();
const MD_CACHE_MAX = 400;

function renderMarkdown(src) {
  if (!src) return "";
  if (!window.marked || !window.DOMPurify) return esc(src); // assets missing — plain text
  let html = mdCache.get(src);
  if (html !== undefined) {
    // refresh recency so the LRU keeps the parts actually on screen
    mdCache.delete(src);
    mdCache.set(src, html);
    return html;
  }
  html = DOMPurify.sanitize(md.parse(src), { ADD_ATTR: ["target"] });
  if (mdCache.size >= MD_CACHE_MAX) mdCache.delete(mdCache.keys().next().value);
  mdCache.set(src, html);
  return html;
}

// Copy buttons are injected by the code renderer; DOMPurify strips inline
// onclick handlers, so copy clicks are handled by delegation instead.
document.addEventListener("click", (e) => {
  const btn = e.target.closest(".copy-code");
  if (!btn) return;
  const code = btn.parentElement && btn.parentElement.querySelector("code");
  if (!code) return;
  navigator.clipboard.writeText(code.textContent);
  btn.textContent = "copied";
  setTimeout(() => { btn.textContent = "copy"; }, 1200);
});

function renderDiff(patch) {
  const box = el("div", "diff");
  for (const line of (patch || "").split("\n")) {
    if (/^@@/.test(line)) box.appendChild(Object.assign(el("div", "hunk"), { textContent: line }));
    else if (/^\+/.test(line) && !/^\+\+\+/.test(line)) box.appendChild(Object.assign(el("div", "l add"), { textContent: line }));
    else if (/^-/.test(line) && !/^---/.test(line)) box.appendChild(Object.assign(el("div", "l del"), { textContent: line }));
    else if (line !== "" || true) box.appendChild(Object.assign(el("div", "l"), { textContent: line }));
  }
  return box;
}

/* ============================== event source ============================== */

function connectSSE() {
  if (DESKTOP) {
    // events come over the Wails bridge (initDesktopEvents); fire the same
    // initial event the SSE endpoint sends so statuses refresh on boot
    handleEvent({ type: "server.connected", properties: {} });
    return;
  }
  if (S.es) S.es.close();
  const q = DIR ? "?directory=" + encodeURIComponent(DIR) : "";
  const es = new EventSource("/event" + q);
  S.es = es;
  es.onmessage = (e) => {
    let ev;
    try { ev = JSON.parse(e.data); } catch { return; }
    handleEvent(ev);
  };
  es.onerror = () => { /* auto-reconnect */ };
}

function handleEvent(ev) {
  const p = ev.properties || {};
  switch (ev.type) {
    case "session.created":
    case "session.updated": {
      const info = p.info;
      if (!info) return;
      const idx = S.sessions.findIndex((s) => s.id === info.id);
      const wasReverted = idx >= 0 && !!S.sessions[idx].revert;
      const wasCompacting = idx >= 0 && !!(S.sessions[idx].time && S.sessions[idx].time.compacting);
      const prev = idx >= 0 ? S.sessions[idx] : null;
      if (idx >= 0) S.sessions[idx] = info; else S.sessions.unshift(info);
      sortSessions();
      // the server publishes session.updated once per LLM step (token counts
      // move) — only rebuild the sidebar when a field it displays changed
      // (title, sort position, archived state) or the session is new
      const sidebarSig = (s) => [s.title, s.time && s.time.updated, s.time && s.time.archived].join("|");
      if (!prev || sidebarSig(prev) !== sidebarSig(info)) renderSidebar();
      if (info.id === S.current) {
        updateHeaderSession(info);
        // the compacting flag drives the timeline's "Compacting" indicator
        if (wasCompacting !== !!(info.time && info.time.compacting)) {
          updateThinkingIndicator();
          updateComposerChrome();
        }
        if (wasReverted !== !!info.revert) {
          // Reverting puts the reverted message back in the chat box; read it
          // from the local store before the reload hides it.
          if (info.revert && info.revert.messageID) {
            const text = userMessageText(msgStore(info.id), info.revert.messageID);
            if (text) setComposerText(text);
          }
          // a revert (button or /undo //redo) is an explicit user action —
          // always land on the newest visible message
          S.scrollStick = true;
          loadMessages(info.id).then(() => renderTimeline()).catch(() => {});
        }
      }
      break;
    }
    case "session.deleted":
      S.tabs = S.tabs.filter((id) => id !== p.sessionID);
      saveTabs();
      S.sessions = S.sessions.filter((s) => s.id !== p.sessionID);
      S.msgs.delete(p.sessionID);
      delete S.statuses[p.sessionID];
      renderSidebar();
      updateTab();
      if (p.sessionID === S.current) navigate("");
      // the board chat's session was archived — fall back to a fresh one
      if (p.sessionID === boardChatSessionId()) {
        setBoardChatSessionId("");
        renderBoardChat();
      }
      break;
    case "server.connected":
      // (re)connect: session statuses may have changed while the stream was
      // down — refresh them so progress indicators (sidebar, tabs, and the
      // spinner on still-running subagent rows) reflect reality
      get("/session/status").then((st) => {
        S.statuses = st || {};
        renderSidebar();
        renderComposer();
        renderTimeline();
        renderBoard();
        renderBoardChat(); // the orchestrator chat's busy state may have changed
        updateTab();
      }).catch(() => {});
      break;
    case "session.status": {
      if (p.status && p.status.type === "idle") {
        // record whether the session was really working before the entry is
        // wiped — session.idle needs it to decide on the completion chime
        // (a compaction status is not a run: ending one stays silent)
        const prev = S.statuses[p.sessionID];
        if (prev && prev.type !== "idle" && prev.type !== "compact") idleAfterBusy.add(p.sessionID);
        delete S.statuses[p.sessionID];
      }
      else {
        S.statuses[p.sessionID] = p.status;
        S.errored.delete(p.sessionID); // fresh activity clears a stale error flag
      }
      updateSidebarRow(p.sessionID);
      updateComposerChrome();
      // the timeline itself only changes for the current session (thinking
      // indicator) or a task child (row spinner) — both patched in place
      if (p.sessionID === S.current) {
        updateThinkingIndicator();
      } else if (taskChildOfCurrent(p.sessionID)) {
        updateTaskRows($("#timeline-inner"), S.current, p.sessionID);
      }
      updateBoardSpinners(); // in-progress cards show a spinner while their session is busy
      updateBoardPanelSpinners();
      if (p.sessionID === boardChatSessionId()) {
        updateBoardChatThinking(); // thinking indicator + stop button
        updateBoardChatChrome();
      } else if (taskChildOf(boardChatSessionId(), p.sessionID)) {
        updateTaskRows($("#board-chat-timeline-inner"), boardChatSessionId(), p.sessionID);
      }
      updateCronSpinners(); // running crons show the hourglass
      updateTab();
      break;
    }
    case "session.idle": {
      // the idle session.status always precedes this event and has already
      // cleared S.statuses — the recorded bit is the only reliable signal
      // (the sessionBusy fallback covers an idle event without a status)
      const wasBusy = idleAfterBusy.delete(p.sessionID) || sessionBusy(p.sessionID);
      delete S.statuses[p.sessionID];
      S.streaming.clear();
      deltaPaintedLen.clear();
      deltaPaintedLenBoard.clear();
      updateSidebarRow(p.sessionID);
      updateComposerChrome();
      if (p.sessionID === S.current) {
        // one full rebuild per run end is fine: it moves the agent · model
        // label into the completed message's last tool row and stops the
        // live dots — things part-level patches don't cover
        renderTimeline();
      } else if (taskChildOfCurrent(p.sessionID)) {
        updateTaskRows($("#timeline-inner"), S.current, p.sessionID);
      }
      updateBoardSpinners(); // clear the spinner on the card this session was working on
      updateBoardPanelSpinners();
      if (p.sessionID === boardChatSessionId()) {
        renderBoardChat(); // clear the thinking indicator + settle the last row
        updateBoardChatChrome();
      } else if (taskChildOf(boardChatSessionId(), p.sessionID)) {
        updateTaskRows($("#board-chat-timeline-inner"), boardChatSessionId(), p.sessionID);
      }
      updateCronSpinners(); // clear the hourglass on the finished run
      updateTab();
      // the agent finished working — chime, but only for real completions of
      // top-level sessions (subagents going idle just hand control back to
      // their parent, and a user-initiated abort needs no fanfare)
      const finished = S.sessions.find((s) => s.id === p.sessionID);
      if (wasBusy && finished && !finished.parentID && Date.now() >= soundSuppressedUntil) {
        Notify.play();
      }
      break;
    }
    case "session.error": {
      if (p.sessionID) S.errored.add(p.sessionID);
      updateSidebarRow(p.sessionID); // error dot on the row
      updateBoardSpinners();
      updateBoardPanelSpinners();
      updateTab();
      break;
    }
    case "message.updated": {
      const store = msgStore(p.sessionID);
      const info = p.info;
      // updates to messages older than the loaded window are ignored —
      // pushing them would append out of order at the end of the timeline
      if (store.info[info.id] === undefined && store.order.length && info.id < store.order[0]) {
        break;
      }
      if (store.info[info.id] === undefined) store.order.push(info.id);
      store.info[info.id] = Object.assign(store.info[info.id] || {}, info);
      trimWindow(store);
      // the header's Import link is only offered while the session is empty —
      // the first message must retire it without waiting for session.updated
      if (p.sessionID === S.current && store.order.length === 1) renderHeader();
      if (info.role === "assistant" && info.error && p.sessionID === S.current) {
        renderTimeline();
      }
      if (info.role === "assistant" && info.error && p.sessionID === boardChatSessionId()) {
        renderBoardChat();
      }
      if (info.role === "user" && p.sessionID !== S.current) {
        S.unseen.add(p.sessionID);
        updateSidebarRow(p.sessionID); // unseen dot
      }
      break;
    }
    case "message.part.updated": {
      const store = msgStore(p.sessionID);
      const part = p.part;
      // parts for messages older than the loaded window are not displayed
      if (!store.info[part.messageID] && store.order.length && part.messageID < store.order[0]) {
        break;
      }
      if (!store.parts[part.messageID]) store.parts[part.messageID] = {};
      store.parts[part.messageID][part.id] = part;
      // text/reasoning part finalized — stop delta-streaming into it
      if ((part.type === "text" || part.type === "reasoning") && part.time && part.time.end) {
        S.streaming.delete(part.id);
        deltaPaintedLen.delete(part.id);
        deltaPaintedLenBoard.delete(part.id);
      }
      trimWindow(store); // a new part can push the window over the cap
      if (p.sessionID === S.current) {
        if (!patchPartIn($("#timeline-inner"), part, store.info[part.messageID])) {
          // brand-new part: append to the tail message's block when possible
          // instead of rebuilding the whole timeline
          if (!appendPartIn($("#timeline-inner"), part, store)) {
            renderTimeline();
          } else {
            // the part may have retired the Thinking row (tail block now
            // has visible content)
            updateThinkingIndicator();
            const tl = $("#timeline");
            if (tl && S.scrollStick) tl.scrollTop = tl.scrollHeight;
          }
        } else {
          const tl = $("#timeline");
          if (tl && S.scrollStick) tl.scrollTop = tl.scrollHeight;
        }
      }
      if (p.sessionID === boardChatSessionId()) {
        if (!patchPartIn($("#board-chat-timeline-inner"), part, store.info[part.messageID])) {
          if (!appendPartIn($("#board-chat-timeline-inner"), part, store)) {
            renderBoardChat();
          } else {
            updateBoardChatThinking();
            const tl = $("#board-chat-timeline");
            if (tl && boardChatNearBottom(tl)) tl.scrollTop = tl.scrollHeight;
          }
        } else {
          const tl = $("#board-chat-timeline");
          if (tl && boardChatNearBottom(tl)) tl.scrollTop = tl.scrollHeight;
        }
      }
      break;
    }
    case "message.part.delta": {
      const store = msgStore(p.sessionID);
      if (!store.parts[p.messageID]) store.parts[p.messageID] = {};
      let part = store.parts[p.messageID][p.partID];
      if (!part) {
        // delta for a message older than the loaded window — not displayed
        if (store.order.length && p.messageID < store.order[0]) break;
        // delta arrived before the part.updated event — create a placeholder
        part = {
          id: p.partID,
          messageID: p.messageID,
          sessionID: p.sessionID,
          type: p.field === "reasoning" ? "reasoning" : "text",
          text: "",
        };
        store.parts[p.messageID][p.partID] = part;
        if (!store.info[p.messageID]) {
          store.info[p.messageID] = { id: p.messageID, sessionID: p.sessionID, role: "assistant", time: {} };
        }
        if (!store.order.includes(p.messageID)) store.order.push(p.messageID);
        trimWindow(store);
      }
      if (p.field === "text" || p.field === "reasoning") {
        part.text = (part.text || "") + (p.delta || "");
        if (p.sessionID === S.current || p.sessionID === boardChatSessionId()) {
          S.streaming.set(p.partID, p.messageID);
          scheduleDeltaRender();
        }
      }
      break;
    }
    case "message.removed": {
      const store = msgStore(p.sessionID);
      store.order = store.order.filter((id) => id !== p.messageID);
      delete store.info[p.messageID];
      delete store.parts[p.messageID];
      if (p.sessionID === S.current) {
        renderTimeline();
        // an emptied session offers Import again
        if (!store.order.length) renderHeader();
      }
      if (p.sessionID === boardChatSessionId()) renderBoardChat();
      break;
    }
    case "permission.asked":
      S.permissions = S.permissions.filter((r) => r.id !== p.id);
      S.permissions.push(p);
      updateSidebarRow(p.sessionID); // permission dot
      if (isDockSession(p.sessionID)) renderComposerRegion();
      break;
    case "permission.replied":
      S.permissions = S.permissions.filter((r) => r.id !== p.requestID);
      updateSidebarRow(p.sessionID);
      if (isDockSession(p.sessionID)) renderComposerRegion();
      break;
    case "question.asked":
      S.questions.push(p);
      if (isDockSession(p.sessionID)) renderComposerRegion();
      break;
    case "question.replied":
    case "question.rejected":
      S.questions = S.questions.filter((q) => q.id !== p.requestID);
      if (isDockSession(p.sessionID)) renderComposerRegion();
      break;
    case "todo.updated":
      S.todos[p.sessionID] = p.todos;
      if (isDockSession(p.sessionID)) renderComposerRegion();
      break;
    case "board.updated":
      // another client (or tab) changed the board — always reload so card
      // starts are detected (and their tabs opened) even when the board is
      // not the active view; re-render only when it is visible
      loadBoard().then(() => { if (S.view === "board") renderBoard(); });
      break;
    case "cron.updated":
      // a cron job changed (created/edited/paused/deleted, or the backend
      // sweep started a run) — reload and re-render when the tab is visible
      loadCron().then(() => { if (S.view === "cron") renderCronList(); });
      break;
    case "alarm.updated":
      // a wake-up was set, cancelled or fired — refresh the ⏰ indicators
      // in the header, tabs and sidebar
      loadAlarms().catch(() => {});
      break;
    case "session.diff":
      break;
    default:
      break;
  }
}

function isDockSession(sessionID) {
  return sessionID === S.current || (S.current && S.children[S.current] && S.children[S.current].includes(sessionID));
}

// A session is "ongoing" while it has a non-idle status (busy/retry). Statuses
// are deleted on idle, so a missing entry means idle/never-ran.
function sessionBusy(sessionID) {
  const st = S.statuses[sessionID];
  return !!(st && st.type !== "idle");
}

// True when sessionID is a subagent referenced by a task tool part in
// session `sid`'s timeline — its row shows the open link and, while the
// subagent is still working, a progress indicator.
function taskChildOf(sessionID, childID) {
  if (!sessionID || !childID || sessionID === childID) return false;
  const store = S.msgs.get(sessionID);
  if (!store) return false;
  for (const mid of store.order) {
    const parts = store.parts[mid];
    if (!parts) continue;
    for (const pid of Object.keys(parts)) {
      const part = parts[pid];
      if (part && part.type === "tool" && part.tool === "task" &&
          part.state && part.state.metadata && part.state.metadata.sessionId === childID) return true;
    }
  }
  return false;
}

function taskChildOfCurrent(sessionID) {
  return taskChildOf(S.current, sessionID);
}

// Re-render the task rows in a timeline that reference subagent session
// `childSid` — their spinner follows the child's busy state. A targeted swap
// via patchPartIn (its outerHTML guard makes it a no-op when nothing changed)
// instead of the full timeline rebuild this used to trigger.
function updateTaskRows(container, sid, childSid) {
  if (!container || !sid || !childSid) return;
  const store = S.msgs.get(sid);
  if (!store) return;
  for (const mid of store.order) {
    const info = store.info[mid];
    if (!info) continue;
    const parts = store.parts[mid] || {};
    for (const pid of Object.keys(parts)) {
      const p = parts[pid];
      if (!p || p.type !== "tool" || p.tool !== "task") continue;
      const st = toolState(p);
      if (!(st.metadata && st.metadata.sessionId === childSid)) continue;
      patchPartIn(container, p, info);
    }
  }
}

function msgStore(sid) {
  if (!S.msgs.has(sid)) {
    S.msgs.set(sid, { order: [], info: {}, parts: {} });
  }
  return S.msgs.get(sid);
}

// Deltas arrive faster than anyone can read; re-parsing the full part text
// per frame is wasted work (and for long messages, megabytes of markdown per
// second). Throttle to a fixed cadence instead of every animation frame.
const DELTA_RENDER_MS = 120;
let deltaTimer = 0;
let deltaLast = 0;

// Text length each streaming part had at its last paint, per timeline. A
// tick only re-renders parts that grew since — without this, every tick
// re-parsed and re-set innerHTML on every entry in S.streaming even when
// only one of them was actually receiving deltas (the others were already
// painted and waiting for their next delta or finalization).
const deltaPaintedLen = new Map();     // main timeline: partID -> painted length
const deltaPaintedLenBoard = new Map(); // board chat timeline

function scheduleDeltaRender() {
  if (deltaTimer) return;
  deltaTimer = setTimeout(() => {
    deltaTimer = 0;
    deltaLast = Date.now();
    renderStreamingText();
    renderBoardChatStreaming();
  }, Math.max(0, deltaLast + DELTA_RENDER_MS - Date.now()));
}

// Re-render a single part's node in place instead of rebuilding the whole
// timeline (message.part.updated fires for every tool state change during a
// run). Returns false when the node is missing or the part type has no
// patchable node — callers then fall back to a full render (cheap now that
// renderMarkdown is memoized). Text, reasoning and tool nodes carry
// data-part; anything else takes the fallback.
function patchPartIn(container, part, info) {
  if (!container || !info) return false;
  const node = container.querySelector(`[data-part="${cssId(part.id)}"]`);
  if (!node) return false;
  if (part.type === "text") {
    if (part.synthetic || part.ignored || !(part.text || "").trim()) return false;
    const fresh = renderPart(part, info);
    if (!fresh) return false;
    node.replaceWith(fresh);
    return true;
  }
  if (part.type === "reasoning") {
    // data-part lives on the inner .assistant-text; swap the whole .reasoning
    // wrapper so the header count, dots and collapse state stay consistent
    const outer = node.closest(".reasoning");
    if (!outer) return false;
    const fresh = renderPart(part, info);
    if (!fresh || !fresh.classList.contains("reasoning")) return false;
    outer.replaceWith(fresh);
    return true;
  }
  if (part.type === "tool") {
    // data-part lives on the .tool wrapper; swap the whole row so running
    // state, streamed output and the expand choice (S.toolOpen) stay
    // consistent — without this, every tool state change forced a full
    // timeline rebuild that reset the scroll position mid-read.
    // Completed messages are excluded: their meta line is embedded in the
    // last tool row by renderAssistantMessage, which renderPart doesn't do.
    if (info.time && info.time.completed) return false;
    const fresh = renderPart(part, info);
    if (!fresh || !fresh.classList.contains("tool")) return false;
    // running tools fire part.updated repeatedly with state that doesn't
    // change the rendering (status pings, metadata touches) — skip the swap
    // when the fresh row is identical, so selection inside an open body
    // isn't needlessly destroyed
    if (fresh.outerHTML === node.outerHTML) return true;
    node.replaceWith(fresh);
    return true;
  }
  return false;
}

// Append a brand-new part to the tail message's block instead of rebuilding
// the whole timeline. Only safe when the part belongs to the last message in
// the store, that message is an assistant message, and its block is already
// in the DOM. Returns false when the caller must fall back to a full
// renderTimeline(); returns true (with nothing drawn) for parts that render
// nothing (todowrite, empty text) — they are stored and need no node.
function appendPartIn(container, part, store) {
  if (!container || !store) return false;
  const lastMid = store.order[store.order.length - 1];
  if (!lastMid || part.messageID !== lastMid) return false;
  const info = store.info[lastMid];
  if (!info || info.role !== "assistant") return false;
  const block = container.querySelector(`[data-msg="${cssId(lastMid)}"]`);
  if (!block) return false;
  const fresh = renderPart(part, info);
  if (!fresh) return true;
  block.appendChild(fresh);
  return true;
}

// Update only the parts with live deltas; falls back to a full timeline
// render when a streaming part has no DOM node yet (bubble not created).
function renderStreamingText() {
  const inner = $("#timeline-inner");
  if (!inner || !S.current) return;
  const store = msgStore(S.current);
  let needFull = false;
  for (const [pid, mid] of S.streaming) {
    // parts of other timelines (e.g. the board chat's session) stream too —
    // they are not this store's business and must not trigger a rebuild here
    if (!store.info[mid]) continue;
    const part = store.parts[mid] && store.parts[mid][pid];
    if (!part) { needFull = true; continue; }
    // nothing grew since the last paint — the node already shows this text
    if (deltaPaintedLen.get(pid) === (part.text || "").length) continue;
    const node = inner.querySelector(`[data-part="${cssId(pid)}"]`);
    if (!node) {
      // first delta for a part whose bubble doesn't exist yet — append it to
      // the tail message's block instead of rebuilding the whole timeline
      if (appendPartIn(inner, part, store)) {
        deltaPaintedLen.set(pid, (part.text || "").length);
        updateThinkingIndicator(); // the new content may retire the Thinking row
        continue;
      }
      needFull = true;
      continue;
    }
    node.innerHTML = renderMarkdown(part.text || "");
    deltaPaintedLen.set(pid, (part.text || "").length);
    // collapsed reasoning header: keep the character count in sync with the stream
    if (part.type === "reasoning") {
      const c = inner.querySelector(`[data-rcount="${cssId(pid)}"]`);
      if (c) {
        c.textContent = `[${fmtNum((part.text || "").length)}]`;
        c.title = `${(part.text || "").length} characters of reasoning`;
      }
    }
  }
  if (needFull) {
    renderTimeline();
    // the rebuild painted every streaming part at its current length
    for (const [pid, mid] of S.streaming) {
      const part = store.parts[mid] && store.parts[mid][pid];
      if (part) deltaPaintedLen.set(pid, (part.text || "").length);
    }
    return;
  }
  const tl = $("#timeline");
  if (tl && S.scrollStick) tl.scrollTop = tl.scrollHeight;
}

function cssId(id) {
  return (window.CSS && CSS.escape) ? CSS.escape(id) : String(id).replace(/[^a-zA-Z0-9_-]/g, "\\$&");
}

/* ============================== rendering ============================== */

function sortSessions() {
  S.sessions.sort((a, b) => b.time.updated - a.time.updated);
}

/* ---------- session tabs ---------- */

// Tabs are stored per workspace (see wsGet/wsSet) so each workspace restores
// its own open sessions.

// A pending tab is a client-side placeholder for a session that does not
// exist on the server yet: "+" (and the new-session buttons) open one titled
// "New session", and the session is only created when the first message is
// sent (see doSend). Each pending tab keeps its own composer draft, so
// several unsent messages can live side by side.
const PENDING_RE = /^pending:(\d+)$/;

function isPendingTab(id) {
  return typeof id === "string" && PENDING_RE.test(id);
}

function nextPendingId() {
  let max = 0;
  for (const id of S.tabs) {
    const m = PENDING_RE.exec(String(id));
    if (m) max = Math.max(max, +m[1]);
  }
  return "pending:" + (max + 1);
}

function loadTabs() {
  try { S.tabs = JSON.parse(wsGet("tabs") || "[]"); } catch { S.tabs = []; }
  if (!Array.isArray(S.tabs)) S.tabs = [];
  // drop tabs whose sessions no longer exist (deleted/archived) or are
  // subagents; pending (not-yet-created) tabs always survive
  S.tabs = S.tabs.filter((id) => isPendingTab(id) || S.sessions.some((s) => s.id === id && !s.parentID));
}

function saveTabs() {
  wsSet("tabs", JSON.stringify(S.tabs));
}

function openTab(sid) {
  if (isPendingTab(sid)) {
    if (!S.tabs.includes(sid)) {
      S.tabs.push(sid);
      saveTabs();
    }
    return;
  }
  const sess = S.sessions.find((s) => s.id === sid);
  if (!sess || sess.parentID) return;
  if (!S.tabs.includes(sid)) {
    S.tabs.push(sid);
    saveTabs();
  }
}

// "+" and the new-session buttons: open a fresh pending tab (the landing
// view stays reachable only via the empty hash, e.g. after closing tabs).
// The backing session is created on the server right away — tabs and the
// sidebar workspace read from the same session store, so the new session
// shows up in the workspace immediately instead of only after the first
// message is sent.
function newSessionTab() {
  const id = nextPendingId();
  S.tabs.push(id);
  saveTabs();
  navigate("/" + id);
  createPendingSession(id);
}

// Session creation for a pending tab, kept per pending id: if the user
// manages to send before the create resolves, doSend awaits the same
// promise instead of minting a second session for the tab.
const pendingCreates = new Map(); // pendingId -> Promise<session|undefined>

// Make a freshly created session visible in the workspace immediately —
// the SSE session.created event covers other clients, this covers our own.
function trackSession(sess) {
  if (!sess || !sess.id) return sess;
  if (!S.sessions.some((s) => s.id === sess.id)) {
    S.sessions.unshift(sess);
    sortSessions();
  }
  return sess;
}

function createPendingSession(id) {
  const job = (async () => {
    const body = { agent: S.agent, title: "New session" };
    if (S.model) body.model = { id: S.model.modelID, providerID: S.model.providerID };
    const sess = trackSession(await post("/session", body));
    // the tab may have been closed while the create was in flight — the
    // session still exists and stays visible in the workspace
    if (S.tabs.includes(id)) {
      promotePendingTab(id, sess.id);
      if (location.hash === "#/" + id) navigate("/" + sess.id);
    }
    renderSidebar();
    return sess;
  })();
  pendingCreates.set(id, job);
  job.catch((e) => {
    // keep the pending tab: the session is then created on first send
    // (see doSend), so the workspace and the tabs stay in sync either way
    toast(e.message || String(e), true);
  }).finally(() => {
    if (pendingCreates.get(id) === job) pendingCreates.delete(id);
  });
  return job;
}

// Resolve the session a send/command should go to. A pending tab becomes a
// real session here: an in-flight tab creation is reused when possible,
// otherwise the session is created now. The tab is promoted in place
// (position kept) and drafts move across with it.
async function ensureSessionId() {
  const pendingId = S.current;
  if (pendingId && !isPendingTab(pendingId)) return pendingId; // already real
  if (pendingId) {
    const inflight = pendingCreates.get(pendingId);
    const ready = inflight ? await inflight.catch(() => null) : null;
    if (ready && ready.id) {
      promotePendingTab(pendingId, ready.id);
      if (S.current === pendingId) navigate("/" + ready.id);
      return ready.id;
    }
  }
  const body = { agent: S.agent };
  if (S.model) body.model = { id: S.model.modelID, providerID: S.model.providerID };
  const sess = trackSession(await post("/session", body));
  promotePendingTab(pendingId, sess.id);
  if (S.current === pendingId) navigate("/" + sess.id);
  return sess.id;
}

// Swap a pending tab for the real session id once the server created it.
// Composer drafts (and attachments) typed under the pending key move across,
// so nothing typed while the create was in flight is lost.
function promotePendingTab(oldId, newId) {
  const idx = S.tabs.indexOf(oldId);
  if (idx >= 0) S.tabs[idx] = newId;
  else if (!S.tabs.includes(newId)) S.tabs.push(newId);
  saveTabs();
  if (composerDrafts.has(oldId)) {
    composerDrafts.set(newId, composerDrafts.get(oldId));
    composerDrafts.delete(oldId);
  }
  if (composerFiles.has(oldId)) {
    composerFiles.set(newId, composerFiles.get(oldId));
    composerFiles.delete(oldId);
  }
}

function closeTab(sid) {
  const idx = S.tabs.indexOf(sid);
  if (idx < 0) return;
  S.tabs.splice(idx, 1);
  saveTabs();
  if (S.current === sid) {
    const next = S.tabs[Math.min(idx, S.tabs.length - 1)];
    navigate(next ? "/" + next : "");
  }
  renderTabs();
}

function buildTabBar() {
  const bar = el("div", "tab-bar");
  bar.id = "tab-bar";
  return bar;
}

/* ---------- workspace tabs (the layer above the session tabs) ---------- */

// Which workspace tabs are open, in order. Persisted per browser; the
// current workspace is always in the list (closing the last tab is a no-op,
// like a browser never closing its final tab). Switching workspaces reloads
// the page (see switchWorkspace), so the list is the only state to carry.
function wsOpenList() {
  try {
    const v = JSON.parse(localStorage.getItem("intellexor.wsTabs") || "null");
    if (Array.isArray(v)) {
      const open = v.filter((d) => d && typeof d === "string");
      if (!open.includes(currentDir())) open.unshift(currentDir());
      return open;
    }
  } catch {}
  return [currentDir()];
}

function wsSaveOpen(open) {
  try { localStorage.setItem("intellexor.wsTabs", JSON.stringify(open)); } catch {}
}

// Display name for a workspace: the user-assigned name (server-persisted)
// when set, else the directory base name.
function workspaceDisplay(dir) {
  const ws = S.workspaces.find((w) => w.directory === dir);
  if (ws && ws.name) return ws.name;
  return dir.split("/").filter(Boolean).pop() || dir || "Workspace";
}

function buildWorkspaceBar() {
  const bar = el("div", "ws-tab-bar");
  bar.id = "ws-tab-bar";
  return bar;
}

function renderWorkspaceTabs() {
  const bar = $("#ws-tab-bar");
  if (!bar) return;
  bar.innerHTML = "";
  const open = wsOpenList();
  for (const dir of open) {
    const tab = el("div", "tab ws-tab" + (dir === currentDir() ? " active" : ""));
    tab.appendChild(el("span", "tab-icon", ICONS.folder));
    tab.appendChild(el("span", "tab-title", esc(workspaceDisplay(dir))));
    const ws = S.workspaces.find((w) => w.directory === dir);
    if (ws && workspaceUnseen(ws)) {
      const dot = el("span", "dot dot-unseen");
      dot.title = "Finished session since you were last here";
      tab.appendChild(dot);
    }
    const close = el("button", "tab-close", ICONS.close);
    close.title = "Close workspace tab";
    close.onclick = (e) => { e.stopPropagation(); closeWorkspaceTab(dir); };
    tab.appendChild(close);
    tab.title = workspaceDisplay(dir) + " (" + dir + ") — click to switch, right-click to rename or close";
    tab.onclick = () => switchWorkspace(dir);
    tab.oncontextmenu = (e) => {
      e.preventDefault();
      openContextMenu(e.clientX, e.clientY, [
        { label: "Rename", icon: ICONS.edit, action: () => openWorkspaceRenamePopover(e.clientX, e.clientY, dir) },
        { label: "Close", icon: ICONS.close, action: () => closeWorkspaceTab(dir) },
      ]);
    };
    bar.appendChild(tab);
  }
  const plus = el("button", "tab-new", ICONS.plus);
  plus.appendChild(el("span", "tab-new-label", "workspace"));
  plus.title = "Open workspace";
  plus.onclick = () => openWorkspacePicker();
  bar.appendChild(plus);
}

function closeWorkspaceTab(dir) {
  const open = wsOpenList();
  if (open.length <= 1) return; // never close the last tab
  const idx = open.indexOf(dir);
  if (idx >= 0) open.splice(idx, 1);
  wsSaveOpen(open);
  if (dir === currentDir()) {
    const next = open[Math.min(idx, open.length - 1)];
    if (next) switchWorkspace(next);
    return; // switchWorkspace reloads the page
  }
  renderWorkspaceTabs();
}

// Rename a workspace (same interaction as renaming a session tab: a small
// input popover at the cursor). The name is persisted server-side and shown
// everywhere the workspace appears.
function openWorkspaceRenamePopover(x, y, dir) {
  closePopovers();
  const pop = el("div", "popover rename-pop");
  const input = document.createElement("input");
  input.type = "text";
  input.value = workspaceDisplay(dir);
  input.placeholder = "Workspace name";
  input.maxLength = 200;
  pop.appendChild(input);
  document.body.appendChild(pop);
  const pw = pop.offsetWidth, ph = pop.offsetHeight;
  pop.style.left = Math.max(8, Math.min(window.innerWidth - pw - 8, x)) + "px";
  pop.style.top = Math.max(8, Math.min(window.innerHeight - ph - 8, y)) + "px";
  popoverEl = pop;
  let done = false;
  const finish = (save) => {
    if (done) return;
    done = true;
    closePopovers();
    if (!save) return;
    const v = input.value.trim();
    if (!v || v === workspaceDisplay(dir)) return;
    post("/workspace/rename", { directory: dir, name: v })
      .then(() => loadWorkspaces())
      .catch((err) => toast("Rename failed: " + (err && err.message ? err.message : err), true));
  };
  input.onkeydown = (e) => {
    if (e.key === "Enter") finish(true);
    else if (e.key === "Escape") finish(false);
  };
  input.onblur = () => finish(true);
  input.focus();
  input.select();
}

function renderTabs() {
  const bar = $("#tab-bar");
  if (!bar) return;
  bar.innerHTML = "";
  // the board is a fixed first tab, always present
  const boardTab = el("div", "tab" + (S.view === "board" ? " active" : ""));
  boardTab.title = "Kanban board";
  boardTab.appendChild(el("span", "tab-icon", ICONS.board));
  boardTab.appendChild(el("span", "tab-title", "Board"));
  boardTab.onclick = () => navigate("/board");
  bar.appendChild(boardTab);
  for (const sid of S.tabs) {
    const s = S.sessions.find((x) => x.id === sid);
    if (!s && !isPendingTab(sid)) continue;
    const pending = !s;
    const tab = el("div", "tab" + (sid === S.current ? " active" : ""));
    const title = pending ? "New session" : (s.title || "New session");
    tab.title = pending
      ? "New session — click to switch, middle-click to close"
      : title + " — click to switch, right-click to rename, middle-click to close";
    if (!pending) {
      const st = S.statuses[sid];
      if (st && st.type !== "idle") {
        tab.appendChild(el("span", "spinner", ICONS.spinner));
      } else if (S.permissions.some((p) => p.sessionID === sid)) {
        tab.appendChild(el("span", "dot dot-permission"));
      } else if (S.errored.has(sid)) {
        tab.appendChild(el("span", "dot dot-error"));
      } else if (S.unseen.has(sid)) {
        tab.appendChild(el("span", "dot dot-unseen"));
      }
    }
    if (!pending && S.alarms[sid]) {
      const clock = el("span", "alarm-indicator", "⏰");
      clock.title = alarmTitle(sid);
      clock.onclick = (e) => { e.stopPropagation(); openAlarmsModal(sid); };
      tab.appendChild(clock);
    }
    tab.appendChild(el("span", "tab-title", esc(title)));
    const close = el("button", "tab-close", ICONS.close);
    close.title = "Close tab";
    close.onclick = (e) => {
      e.stopPropagation();
      closeTab(sid);
    };
    tab.appendChild(close);
    tab.onclick = () => navigate("/" + sid);
    tab.onauxclick = (e) => { if (e.button === 1) closeTab(sid); };
    tab.onmousedown = (e) => { if (e.button === 1) e.preventDefault(); };
    if (!pending) tab.oncontextmenu = (e) => {
      e.preventDefault();
      openContextMenu(e.clientX, e.clientY, [
        { label: "Rename", icon: ICONS.edit, action: () => openRenamePopover(e.clientX, e.clientY, s) },
      ]);
    };
    bar.appendChild(tab);
  }
  const plus = el("button", "tab-new", ICONS.plus);
  plus.appendChild(el("span", "tab-new-label", "session"));
  plus.title = "New session";
  plus.onclick = () => newSessionTab();
  bar.appendChild(plus);
  // cron jobs: fixed tab, pushed to the right edge of the bar (scheduled
  // prompts that start fresh sessions)
  const cronTab = el("div", "tab tab-right" + (S.view === "cron" ? " active" : ""));
  cronTab.title = "Cron jobs";
  cronTab.appendChild(el("span", "tab-icon", ICONS.clock));
  cronTab.appendChild(el("span", "tab-title", "Cron jobs"));
  cronTab.onclick = () => navigate("/cron");
  bar.appendChild(cronTab);
  renderWorkspaceTabs();
}

/* ---------- workspaces ---------- */

// A workspace is a working directory with its own sessions, config and UI
// state. The server keeps one app.Instance per directory; the current one is
// DIR, fixed for the page's lifetime — switching workspaces reloads the app
// (see switchWorkspace) so every DIR-scoped piece of state follows along.
S.workspaces = [];

// Last time (unix ms) each workspace was open in this browser. Drives the
// finished-session dot: a workspace gets the dot when something happened in
// it since the user last had it open and nothing is running there anymore.
const wsVisited = (() => {
  try { return JSON.parse(localStorage.getItem("intellexor.wsVisited") || "{}"); }
  catch { return {}; }
})();

function markWorkspaceVisited(dir) {
  if (!dir) return;
  wsVisited[dir] = Date.now();
  try { localStorage.setItem("intellexor.wsVisited", JSON.stringify(wsVisited)); } catch {}
}

function currentDir() { return S.dir || DIR; }

function workspaceUnseen(ws) {
  return ws.directory !== currentDir() && !ws.busy && ws.lastActivity > (wsVisited[ws.directory] || 0);
}

// Extra workspace icons under the current-workspace badge in the rail. The
// current workspace is #rail-project itself, so it is skipped here.
function renderRailWorkspaces() {
  const box = $("#rail-workspaces");
  if (!box) return;
  box.innerHTML = "";
  for (const ws of S.workspaces) {
    if (ws.directory === currentDir()) continue;
    const b = el("button", "rail-project ws-btn");
    const name = (ws.name || ws.directory).split("/").filter(Boolean).pop() || "?";
    b.title = ws.directory + (workspaceUnseen(ws) ? " — finished session" : "");
    b.appendChild(el("span", "rail-avatar", name[0].toUpperCase()));
    if (workspaceUnseen(ws)) b.appendChild(el("span", "ws-dot"));
    b.onclick = () => switchWorkspace(ws.directory);
    b.oncontextmenu = (e) => {
      e.preventDefault();
      openContextMenu(e.clientX, e.clientY, [
        { label: "Rename", icon: ICONS.edit, action: () => openWorkspaceRenamePopover(e.clientX, e.clientY, ws.directory) },
      ]);
    };
    box.appendChild(b);
  }
}

async function loadWorkspaces() {
  try { S.workspaces = await get("/workspace") || []; } catch { return; }
  renderRailWorkspaces();
  renderWorkspaceTabs();
  refreshWorkspaceChrome();
}

// Keep the workspace chrome (sidebar title, rail badge, tab progress) in
// sync with a user-assigned workspace name — it loads async after boot.
function refreshWorkspaceChrome() {
  const name = workspaceDisplay(currentDir());
  const st = $("#sidebar-title");
  if (st) st.textContent = name;
  const rp = $("#rail-project");
  if (rp) rp.title = name;
  const av = $("#rail-avatar");
  if (av) av.textContent = name[0].toUpperCase();
  TabProgress.setTitle("Intellexor — " + name);
}

let wsRefreshTimer = null;
function scheduleWorkspaceRefresh() {
  if (wsRefreshTimer) return;
  wsRefreshTimer = setTimeout(() => { wsRefreshTimer = null; loadWorkspaces(); }, 500);
}

// Desktop only: one Wails events channel ("server-event", emitted by the
// Go bridge for every instance event) feeds both the session event handler
// and the cross-workspace refresh — the web mode uses the /event and
// /global/event SSE endpoints for the same.
function initDesktopEvents() {
  if (!DESKTOP || !window.runtime || !window.runtime.EventsOn) return;
  window.runtime.EventsOn("server-event", (m) => {
    if (!m || typeof m !== "object") return;
    const t = m.payload && m.payload.type;
    if (m.directory && m.directory !== currentDir() &&
        (t === "session.status" || t === "session.idle" || t === "session.created" || t === "session.deleted")) {
      scheduleWorkspaceRefresh();
    }
    // currentDir() is empty until /global/path loads — accept everything
    // during boot, then filter exactly (matches the /event SSE scope)
    if (!m.directory || !currentDir() || m.directory === currentDir()) handleEvent(m.payload);
  });
}

// Switch workspaces in place — no page reload, so it feels like a tab
// switch: re-point DIR, wipe every DIR-scoped piece of state, re-fetch the
// workspace's data (the boot sequence subset) and re-route. The open-tabs
// list and localStorage carry across; the hash is dropped since session ids
// don't transfer between workspaces.
async function switchWorkspace(dir) {
  if (!dir || dir === currentDir()) return;
  markWorkspaceVisited(currentDir());
  rememberLastSession(S.current); // record under the OLD workspace's scope
  try { localStorage.setItem("intellexor.dir", dir); } catch {}
  const open = wsOpenList();
  if (!open.includes(dir)) { open.push(dir); wsSaveOpen(open); }
  DIR = dir;

  // wipe state that belongs to the old workspace
  S.dir = "";
  S.sessions = [];
  S.msgs = new Map();
  S.statuses = {};
  S.alarms = {};
  S.errored = new Set();
  S.unseen = new Set();
  S.permissions = [];
  S.questions = [];
  S.subagents = new Map();
  S.streaming.clear();
  S.current = null;
  S.view = "sessions";
  // model/agent picks are persisted per workspace — re-read from the new
  // scope (refreshProviders fills the model if this workspace has none)
  S.model = JSON.parse(wsGet("model") || "null");
  S.agent = wsGet("agent") || "build";
  boardCards = [];
  cronJobs = [];
  if (S.es) { S.es.close(); S.es = null; } // web-mode SSE is per-directory
  loadTabs(); // the new workspace's own tab list

  // drop the old workspace's hash (session ids don't transfer) without
  // triggering a route; the search part is untouched
  history.replaceState(null, "", location.pathname + location.search);

  // re-fetch the new workspace's data (boot-sequence subset)
  const pathInfo = await get("/path").catch(() => ({}));
  S.dir = pathInfo.directory || "";
  try { S.vcs = await get("/vcs"); } catch {}
  try {
    S.sessions = (await get("/session") || []).filter((s) => !s.parentID);
    sortSessions();
  } catch (e) {
    toast(e.message, true);
  }
  loadTabs(); // filter again now that sessions exist
  loadAlarms().catch(() => {});
  try { S.statuses = await get("/session/status") || {}; } catch {}
  try { S.permissions = await get("/permission") || []; } catch {}
  try { S.questions = await get("/question") || []; } catch {}
  await refreshProviders();
  loadCommands();
  loadCron().catch(() => {});
  if (S.boardPanelOpen) loadBoard().catch(() => {});

  // restore this workspace's last open chat (no hashchange event — route()
  // below picks it up; openTab inside route re-opens its tab)
  const restore = restorableSession();
  if (restore) {
    history.replaceState(null, "", location.pathname + location.search + "#/" + restore.id);
  }

  // chrome + indicators follow the new workspace
  refreshWorkspaceChrome();
  markWorkspaceVisited(S.dir || DIR);
  loadWorkspaces(); // refresh unseen dots (the old ws was just visited)
  renderWorkspaceTabs();
  connectSSE(); // reconnect the per-directory stream (web mode)

  await route(); // renders sidebar + main from the fresh state
}

// Aggregated event stream across all workspaces: keeps the finished-session
// dots on the workspace icons live while the user is in another workspace.
function connectGlobalSSE() {
  if (DESKTOP) return; // the bridge's single "server-event" channel covers it
  const es = new EventSource("/global/event");
  es.onmessage = (e) => {
    let m;
    try { m = JSON.parse(e.data); } catch { return; }
    const t = m.payload && m.payload.type;
    if (m.directory && m.directory !== currentDir() &&
        (t === "session.status" || t === "session.idle" || t === "session.created" || t === "session.deleted")) {
      scheduleWorkspaceRefresh();
    }
  };
}

/* ---------- workspace picker ---------- */

// The rail "+" opens a directory browser (directories only); picking one
// registers it as a new workspace — own sessions, own config file with the
// usual global/parent fallbacks — and switches to it.
function openWorkspacePicker() {
  openModal("New workspace", (modal, ov) => {
    const bar = el("div", "dir-bar");
    const pathInput = el("input");
    pathInput.type = "text";
    pathInput.spellcheck = false;
    pathInput.placeholder = "/absolute/path/to/workspace";
    bar.appendChild(pathInput);
    const goBtn = el("button", "c-btn", "Go");
    goBtn.onclick = () => load(pathInput.value.trim());
    bar.appendChild(goBtn);
    modal.appendChild(bar);

    const list = el("div", "dir-list");
    modal.appendChild(list);
    const status = el("div", "dir-status");
    modal.appendChild(status);

    const actions = el("div", "m-actions");
    const cancel = el("button", "c-btn", "Cancel");
    cancel.onclick = () => ov.remove();
    const create = el("button", "btn primary", "Create workspace");
    create.onclick = async () => {
      create.disabled = true;
      try {
        const ws = await post("/workspace", { path: pathInput.value.trim() });
        ov.remove();
        switchWorkspace(ws.directory);
      } catch (e) {
        toast(e.message || String(e), true);
        create.disabled = false;
      }
    };
    actions.appendChild(cancel);
    actions.appendChild(create);
    modal.appendChild(actions);

    async function load(path) {
      if (!path) return;
      list.innerHTML = "";
      status.textContent = "Loading…";
      try {
        const ents = await api("GET", "/file?dirs=1&path=" + encodeURIComponent(path));
        pathInput.value = path;
        status.textContent = "";
        const parent = path.replace(/\/+$/, "").replace(/\/[^/]*$/, "") || "/";
        const up = el("div", "dir-row dir-up", "…");
        up.title = "Parent directory";
        up.onclick = () => load(parent);
        list.appendChild(up);
        const dirs = (ents || [])
          .filter((d) => d.type === "directory")
          .sort((a, b) => a.name.localeCompare(b.name));
        if (!dirs.length) list.appendChild(el("div", "dir-empty", "No subdirectories"));
        for (const d of dirs) {
          const row = el("div", "dir-row");
          row.appendChild(el("span", "dir-icon", ICONS.folder));
          row.appendChild(el("span", "dir-name", esc(d.name)));
          row.onclick = () => load(d.absolute || d.path);
          list.appendChild(row);
        }
      } catch (e) {
        status.textContent = e.message || String(e);
      }
    }
    pathInput.onkeydown = (e) => { if (e.key === "Enter") load(pathInput.value.trim()); };
    get("/path").then((p) => load(p.home || "/")).catch(() => load("/"));
  });
}

/* ---------- sidebar collapse ---------- */

function isNarrow() { return matchMedia("(max-width: 767px)").matches; }

// Reflect S.sidebarOpen onto the DOM: on desktop the sidebar collapses inline,
// on small screens it slides in as an overlay drawer with a backdrop.
function applySidebar() {
  const app = $("#app");
  if (!app) return;
  app.classList.toggle("sidebar-collapsed", !S.sidebarOpen);
  app.classList.toggle("sidebar-open", S.sidebarOpen);
  let bd = $("#sidebar-backdrop");
  if (isNarrow() && S.sidebarOpen) {
    if (!bd) {
      bd = el("div", "sidebar-backdrop");
      bd.id = "sidebar-backdrop";
      bd.onclick = () => setSidebar(false);
      document.body.appendChild(bd);
    }
  } else if (bd) {
    bd.remove();
  }
}

// `transient` changes the sidebar without touching the persisted preference —
// used when the board panel borrows the sidebar's slot (the user's choice is
// restored when the panel closes). An explicit (non-transient) change cancels
// that pending restore — and mirrors the swap in the other direction: an
// explicit open borrows the board panel's slot (collapsing it until the
// sidebar closes again), an explicit close hands the slot back.
function setSidebar(open, transient = false) {
  S.sidebarOpen = open;
  if (!transient) {
    boardPanelHoldsSidebar = false;
    try { localStorage.setItem("intellexor.sidebar", open ? "1" : "0"); } catch {}
    if (open && S.boardPanelOpen) {
      sidebarHoldsBoardPanel = true;
      setBoardPanel(false, true);
    } else if (!open && sidebarHoldsBoardPanel) {
      sidebarHoldsBoardPanel = false;
      setBoardPanel(true, true);
    }
  }
  applySidebar();
}

function initSidebar() {
  let saved = null;
  try { saved = localStorage.getItem("intellexor.sidebar"); } catch {}
  // default: open on desktop, closed (drawer) on small screens
  S.sidebarOpen = saved != null ? saved === "1" : !isNarrow();
  applySidebar();
}

/* ---------- board side panel (rail toggle) ---------- */

// A compact left-hand panel listing the In Progress and Open cards, one line
// each; the full card text shows in a tooltip on hover. Mirrors the sidebar
// collapse pattern: state + apply + localStorage persistence, inline collapse
// on desktop and an overlay drawer (from the left) on small screens. It sits
// between the rail and the sidebar, so while it is open the sidebar is
// collapsed — transiently, the user's sidebar preference comes back on close.

// In Progress first — active work is what you glance at — then Open. Done is
// intentionally absent: the panel is a working queue, not an archive.
const BOARD_PANEL_COLUMNS = [
  { id: "inprogress", label: "In Progress" },
  { id: "open", label: "Open" },
];

// True while the board panel is holding the slot the sidebar was occupying:
// the sidebar is collapsed for that time and restored when the panel closes.
let boardPanelHoldsSidebar = false;

// The mirror image: true while the sidebar is holding the slot the board panel
// was occupying — the panel is collapsed for that time and restored when the
// sidebar closes. At most one of the two holds is ever active: each panel
// opening collapses the other, so they never stand side by side.
let sidebarHoldsBoardPanel = false;

function applyBoardPanel() {
  const app = $("#app");
  if (!app) return;
  app.classList.toggle("board-panel-collapsed", !S.boardPanelOpen);
  app.classList.toggle("board-panel-open", S.boardPanelOpen);
  let bd = $("#board-panel-backdrop");
  if (isNarrow() && S.boardPanelOpen) {
    if (!bd) {
      bd = el("div", "sidebar-backdrop");
      bd.id = "board-panel-backdrop";
      bd.onclick = () => setBoardPanel(false);
      document.body.appendChild(bd);
    }
  } else if (bd) {
    bd.remove();
  }
  if (!S.boardPanelOpen) hideBoardPanelTip();
}

function setBoardPanel(open, transient = false) {
  S.boardPanelOpen = open;
  if (!transient) {
    // an explicit panel change cancels a pending restore, mirroring setSidebar
    sidebarHoldsBoardPanel = false;
    try { localStorage.setItem("intellexor.boardPanel", open ? "1" : "0"); } catch {}
  }
  // The panel takes over the sidebar's slot on the left: collapse the sidebar
  // for as long as the panel is open, and hand the slot back on close. Both
  // directions are transient so the user's persisted sidebar preference is
  // never overwritten by the swap; an explicit sidebar toggle while the panel
  // is open cancels the restore (see setSidebar).
  if (open && S.sidebarOpen) {
    boardPanelHoldsSidebar = true;
    setSidebar(false, true);
  } else if (!open && boardPanelHoldsSidebar) {
    boardPanelHoldsSidebar = false;
    setSidebar(true, true);
  }
  applyBoardPanel();
  if (open) loadBoard(); // fresh data on every open; loadBoard re-renders the panel
}

function initBoardPanel() {
  let saved = null;
  try { saved = localStorage.getItem("intellexor.boardPanel"); } catch {}
  S.boardPanelOpen = saved === "1"; // default closed — an optional panel
  // Runs after initSidebar, so the resolved sidebar state is known: if the
  // panel boots open, it takes the slot and the sidebar waits.
  if (S.boardPanelOpen && S.sidebarOpen) {
    boardPanelHoldsSidebar = true;
    setSidebar(false, true);
  }
  applyBoardPanel();
}

// The tooltip: one fixed-position element reused for every hovered row, so
// hover churn never stacks nodes. Shows the full card text the 1-line row
// truncates, plus the card id and column.
let boardPanelTip = null;

function hideBoardPanelTip() {
  if (boardPanelTip) {
    boardPanelTip.remove();
    boardPanelTip = null;
  }
}

function showBoardPanelTip(row, c) {
  hideBoardPanelTip();
  const col = BOARD_COLUMNS.find((k) => k.id === c.column);
  boardPanelTip = el("div", "board-panel-tip");
  const head = el("div", "board-panel-tip-head");
  head.innerHTML = `<span class="kanban-card-id">#${esc(c.id)}</span><span class="board-panel-tip-col">${esc(col ? col.label : c.column)}</span>`;
  boardPanelTip.appendChild(head);
  boardPanelTip.appendChild(el("div", "board-panel-tip-body", esc(c.title)));
  boardPanelTip.style.visibility = "hidden";
  document.body.appendChild(boardPanelTip);
  const r = row.getBoundingClientRect();
  const t = boardPanelTip.getBoundingClientRect();
  // prefer the right of the row (the panel hugs the left edge); when there is
  // no room, drop the tooltip below the row instead
  let left = r.right + 8;
  let top = r.top;
  if (left + t.width > window.innerWidth - 8) {
    left = Math.max(8, Math.min(r.right, window.innerWidth - t.width - 8));
    top = r.bottom + 6;
  }
  top = Math.max(8, Math.min(top, window.innerHeight - t.height - 8));
  boardPanelTip.style.left = left + "px";
  boardPanelTip.style.top = top + "px";
  boardPanelTip.style.visibility = "";
}

// Toggle the busy spinner on board-panel rows in place — same rationale as
// updateBoardSpinners, for the collapsed panel's card list.
function updateBoardPanelSpinners() {
  const list = $("#board-panel-list");
  if (!list || !S.boardPanelOpen) return;
  list.querySelectorAll(".board-panel-card[data-id]").forEach((row) => {
    const c = boardCardById(row.dataset.id);
    const want = !!(c && c.column === "inprogress" && c.sessionID && sessionBusy(c.sessionID));
    const has = !!row.querySelector("span.spinner");
    if (want === has) return;
    if (want) row.appendChild(el("span", "spinner", ICONS.spinner));
    else {
      const sp = row.querySelector("span.spinner");
      if (sp) sp.remove();
    }
  });
}

function renderBoardPanel() {
  const list = $("#board-panel-list");
  if (!list || !S.boardPanelOpen) return;
  hideBoardPanelTip();
  list.innerHTML = "";
  let total = 0;
  for (const col of BOARD_PANEL_COLUMNS) {
    const cards = boardCardsIn(col.id);
    total += cards.length;
    const group = el("div", "board-panel-group");
    group.dataset.column = col.id;
    const label = el("div", "board-panel-group-label");
    label.innerHTML = `${esc(col.label)}<span class="board-panel-group-count">${cards.length}</span>`;
    group.appendChild(label);
    if (!cards.length) group.appendChild(el("div", "board-panel-empty", "No cards"));
    for (const c of cards) {
      const row = el("div", "board-panel-card");
      row.dataset.id = c.id;
      row.appendChild(el("span", "board-panel-dot" + (col.id === "inprogress" ? " inprogress" : "")));
      row.appendChild(el("span", "kanban-card-id", "#" + esc(c.id)));
      row.appendChild(el("span", "board-panel-card-title", esc(c.title)));
      // scheduled start: same clock emoji + time as the board cards
      if (c.startAt) {
        row.appendChild(el("span", "board-panel-schedule", "🕐 " + esc(fmtSchedule(c.startAt))));
      }
      // same busy indicator as the board cards: spinner while the linked
      // session is actively working on this in-progress card
      if (c.column === "inprogress" && c.sessionID && sessionBusy(c.sessionID)) {
        row.appendChild(el("span", "spinner", ICONS.spinner));
      }
      row.onclick = () => {
        if (c.sessionID) viewCard(c);
        else navigate("/board"); // open cards have no session yet — show the full board
      };
      row.onmouseenter = () => showBoardPanelTip(row, c);
      row.onmouseleave = hideBoardPanelTip;
      group.appendChild(row);
    }
    list.appendChild(group);
  }
  $("#board-panel-count").textContent = total;
}

// Sessions with a pending wake-up (the schedule_wakeup tool): the ⏰
// indicator in the session header, tab and sidebar follows this map. It
// disappears as soon as the last alarm is gone — cancelled by the agent or
// claimed by the backend sweep, both of which broadcast alarm.updated.
async function loadAlarms() {
  const list = await get("/alarm") || [];
  S.alarms = {};
  for (const a of list) {
    if (!S.alarms[a.sessionID] || a.triggerAt < S.alarms[a.sessionID]) {
      S.alarms[a.sessionID] = a.triggerAt;
    }
  }
  renderSidebar(); // re-renders the tabs too
  renderHeader();
}

// Hover title for a session's ⏰ indicator: when the wake-up fires, in the
// device's own timezone (Date renders local time by default).
function alarmTitle(sid) {
  const ts = S.alarms[sid];
  if (!ts) return "Wake-up scheduled";
  const d = new Date(ts * 1000); // triggerAt is unix seconds
  return "Wake-up scheduled for " + d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

// "dd hh:mm:ss" duration for the wake-up modal's countdown labels (days
// prefix only when nonzero).
function ddhms(ms) {
  let s = Math.max(0, Math.floor(ms / 1000));
  const d = Math.floor(s / 86400); s %= 86400;
  const h = Math.floor(s / 3600); s %= 3600;
  const m = Math.floor(s / 60); s %= 60;
  const pad = (n) => String(n).padStart(2, "0");
  return (d > 0 ? d + "d " : "") + pad(h) + ":" + pad(m) + ":" + pad(s);
}

// The wake-up modal behind a click on a session's ⏰ indicator: every
// alarm of the session (pending and fired) with a live "will trigger in /
// triggered ... ago" label plus Trigger now / Delete buttons. The modal
// lives in #modal-root, so loadAlarms()'s re-renders never wipe it; the
// countdown ticks locally every second and the list refetches after each
// action (and on alarm.updated via loadAlarms for the indicators).
async function openAlarmsModal(sid) {
  const list = el("div", "alarm-list");
  const footer = el("div", "m-actions");
  const clearBtn = el("button", "btn ghost", "Clear finished");
  clearBtn.onclick = async () => {
    try {
      await post("/alarm/session/" + sid + "/clear-finished");
      await reload();
      loadAlarms().catch(() => {});
    } catch (e) { toast(e.message || "Failed to clear finished wake-ups", true); }
  };
  footer.appendChild(clearBtn);
  const ov = openModal("Wake-ups", (modal) => {
    modal.appendChild(el("p", "m-sub", "Wake-ups the agent scheduled for this session."));
    modal.appendChild(list);
    modal.appendChild(footer);
  });
  let alarms = [];

  async function reload() {
    try {
      alarms = await get("/alarm/session/" + sid) || [];
    } catch (e) {
      toast(e.message || "Failed to load wake-ups", true);
    }
    render();
  }

  function render() {
    list.innerHTML = "";
    footer.style.display = alarms.some((a) => a.fired) ? "" : "none";
    if (!alarms.length) {
      list.appendChild(el("p", "m-sub", "No wake-ups scheduled."));
      return;
    }
    for (const a of alarms) {
      const row = el("div", "m-row alarm-row");
      const info = el("div", "alarm-info");
      info.appendChild(el("div", "alarm-note", a.note || "Wake-up"));
      const when = el("div", "alarm-when");
      when.dataset.triggerAt = a.triggerAt;
      when.dataset.fired = a.fired ? "1" : "";
      info.appendChild(when);
      row.appendChild(info);
      const actions = el("div", "m-actions");
      if (!a.fired) {
        const trig = el("button", "btn ghost", "Trigger now");
        trig.onclick = async () => {
          try {
            await post("/alarm/" + a.id + "/trigger");
            await reload();
            loadAlarms().catch(() => {});
          } catch (e) { toast(e.message || "Failed to trigger wake-up", true); }
        };
        actions.appendChild(trig);
      }
      const rm = el("button", "btn danger", "Delete");
      rm.onclick = async () => {
        try {
          await del("/alarm/" + a.id);
          await reload();
          loadAlarms().catch(() => {});
        } catch (e) { toast(e.message || "Failed to delete wake-up", true); }
      };
      actions.appendChild(rm);
      row.appendChild(actions);
      list.appendChild(row);
    }
    tick();
  }

  function tick() {
    const now = Date.now();
    for (const when of list.querySelectorAll(".alarm-when")) {
      const ts = Number(when.dataset.triggerAt) * 1000; // unix seconds -> ms
      when.textContent = when.dataset.fired
        ? "triggered " + ddhms(now - ts) + " ago"
        : "will trigger in " + ddhms(ts - now);
    }
  }

  // local countdown while the modal is open; self-cleans when it closes
  const iv = setInterval(() => {
    if (!ov.isConnected) { clearInterval(iv); return; }
    tick();
  }, 1000);

  await reload();
}

// The "Add scheduled message" dialog (session header): arms a wake-up for
// the session — the same kind the agent sets itself via schedule_wakeup.
// At the chosen time the session receives a user message ("alarm-triggered"
// plus the note), so the agent continues on its own. Mirrors the kanban
// schedule dialog: presets fill the picker, the picker stays the source of
// truth, and a live preview shows the exact firing time.
function openScheduleMessageModal(sid) {
  openModal("Add scheduled message", (modal, ov) => {
    modal.appendChild(el("p", "m-sub", "The session receives this message at the chosen time and the agent continues by itself — same as a wake-up the agent schedules with its schedule_wakeup tool."));

    const msgField = el("div", "m-field");
    msgField.appendChild(el("label", "m-label", "Message (optional context)"));
    const note = el("textarea", "m-textarea");
    note.rows = 3;
    note.placeholder = "e.g. Check the build output and fix any errors";
    msgField.appendChild(note);
    modal.appendChild(msgField);

    const whenField = el("div", "m-field");
    whenField.appendChild(el("label", "m-label", "When"));
    const input = el("input");
    input.type = "datetime-local";
    input.step = 60;
    input.min = toLocalInputValue(new Date());
    input.value = toLocalInputValue(new Date(Date.now() + 15 * 60000)); // sensible default: ~15 min out
    whenField.appendChild(input);
    modal.appendChild(whenField);

    // quick offsets set the picker; the picker stays the source of truth so
    // a preset can still be fine-tuned by hand before saving
    const presets = el("div", "schedule-presets");
    for (const [label, ms] of [["In 5 min", 5 * 60000], ["In 15 min", 15 * 60000], ["In 30 min", 30 * 60000], ["In 1 hour", 36e5], ["Tomorrow 9:00", "tomorrow9"]]) {
      const b = el("button", "kanban-card-btn", "<span>" + label + "</span>");
      b.type = "button";
      b.onclick = () => {
        if (ms === "tomorrow9") {
          const d = new Date(); d.setDate(d.getDate() + 1); d.setHours(9, 0, 0, 0);
          input.value = toLocalInputValue(d);
        } else {
          input.value = toLocalInputValue(new Date(Date.now() + ms));
        }
        updatePreview();
      };
      presets.appendChild(b);
    }
    modal.appendChild(presets);

    const preview = el("div", "schedule-preview");
    const updatePreview = () => {
      const ts = input.value ? new Date(input.value).getTime() : 0;
      preview.textContent = ts
        ? `The session receives this message ${relTime(ts)} — ${new Date(ts).toLocaleString([], { weekday: "short", month: "short", day: "numeric", hour: "numeric", minute: "2-digit" })}`
        : "Pick a date and time";
    };
    input.oninput = updatePreview;
    updatePreview();

    const actions = el("div", "m-actions");
    const spacer = el("span");
    spacer.style.flex = "1";
    const cancel = el("button", "btn ghost", "Cancel");
    cancel.onclick = () => ov.remove();
    actions.append(spacer, cancel);

    const save = el("button", "btn primary", "Schedule");
    save.onclick = async () => {
      const ts = input.value ? Math.floor(new Date(input.value).getTime() / 1000) : 0;
      if (!ts) { input.focus(); return; }
      if (ts <= Math.floor(Date.now() / 1000)) {
        toast("Pick a time in the future", true);
        input.focus();
        return;
      }
      save.disabled = true;
      try {
        await post("/alarm", { sessionID: sid, triggerAt: ts, note: note.value.trim() });
        ov.remove();
        loadAlarms().catch(() => {}); // refresh the ⏰ indicators
        const when = new Date(ts * 1000).toLocaleString([], { weekday: "short", month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
        toast("Scheduled — the session receives the message " + relTime(ts * 1000) + " (" + when + ")");
      } catch (err) {
        save.disabled = false;
        toast(err.message || String(err), true);
      }
    };
    note.onkeydown = input.onkeydown = (e) => { if (e.key === "Enter" && e.target === input) { e.preventDefault(); save.click(); } };
    actions.appendChild(save);
    modal.appendChild(actions);
    note.focus();
  });
}

// The "Link card" dialog (session header): attach the session to a kanban
// card — either one still waiting in open, or a newly created one. The card
// moves to In Progress with the session linked, exactly like a card the
// session was started from: the agent learns about the card (board context
// in its system prompt) and can move it to done, which pings its watchers
// and auto-starts a follow-up card linked to it. Linking an existing
// session's card back to open (switching or unlinking) releases it for
// another start.
async function openLinkCardModal(sess) {
  const sid = sess.id;
  let cards = [];
  const list = el("div", "linkcard-list");
  const ov = openModal("Link card", (modal) => {
    modal.appendChild(el("p", "m-sub", "Link this session to a kanban card. The card moves to In Progress and behaves exactly like one the session was started from: the agent works on it, and when it is moved to done, its watchers are pinged and a follow-up card linked to it starts automatically."));
    modal.appendChild(list);

    const field = el("div", "m-field");
    field.appendChild(el("label", "m-label", "Or create a new card"));
    const input = el("input");
    input.type = "text";
    input.placeholder = "Describe the task for the agent…";
    field.appendChild(input);
    modal.appendChild(field);

    const actions = el("div", "m-actions");
    const spacer = el("span");
    spacer.style.flex = "1";
    const cancel = el("button", "btn ghost", "Cancel");
    cancel.onclick = () => ov.remove();
    actions.append(spacer, cancel);
    const create = el("button", "btn primary", "Create & link");
    create.onclick = async () => {
      const title = input.value.trim();
      if (!title) { input.focus(); return; }
      create.disabled = true;
      let card;
      try {
        card = await post("/board/card", { title, column: "open" });
      } catch (e) {
        create.disabled = false;
        toast(e.message || String(e), true);
        return;
      }
      // link() toasts, closes the modal and re-renders on failure
      await link(card.id);
    };
    input.onkeydown = (e) => { if (e.key === "Enter") { e.preventDefault(); create.click(); } };
    actions.appendChild(create);
    modal.appendChild(actions);
    input.focus();
  });

  let busy = false;
  const current = () => cards.find((c) => c.sessionID === sid);
  const openCards = () => cards.filter((c) => c.column === "open" && !c.sessionID);

  // link the card to this session (moving it to In Progress), releasing a
  // previously linked card back to open first so the session only ever has one
  async function link(cardID) {
    if (busy) return;
    busy = true;
    try {
      const prev = current();
      if (prev && prev.id !== cardID) {
        await patch("/board/card/" + prev.id, { column: "open", sessionID: "" });
      }
      await patch("/board/card/" + cardID, { column: "inprogress", sessionID: sid });
      ov.remove();
      toast("Card #" + cardID + " linked — it is now In Progress");
    } catch (e) {
      toast(e.message || String(e), true);
    }
    busy = false;
    await reload();
  }

  async function unlink() {
    if (busy) return;
    const cur = current();
    if (!cur) return;
    busy = true;
    try {
      await patch("/board/card/" + cur.id, { column: "open", sessionID: "" });
      toast("Card #" + cur.id + " unlinked — it is back in Open");
    } catch (e) {
      toast(e.message || String(e), true);
    }
    busy = false;
    await reload();
  }

  function render() {
    list.innerHTML = "";
    const cur = current();
    if (cur) {
      list.appendChild(el("p", "m-label", "Linked card"));
      const row = el("div", "m-row linkcard-row");
      row.appendChild(el("div", "linkcard-title", "#" + cur.id + " — " + cur.title));
      const actions = el("div", "m-actions");
      const un = el("button", "btn danger", "Unlink");
      un.onclick = unlink;
      actions.appendChild(un);
      row.appendChild(actions);
      list.appendChild(row);
    }
    const open = openCards();
    list.appendChild(el("p", "m-label", "Pick an open card"));
    if (!open.length) {
      list.appendChild(el("p", "m-sub", "No open cards — create a new one below."));
    }
    for (const c of open) {
      const row = el("div", "m-row linkcard-row");
      row.appendChild(el("div", "linkcard-title", "#" + c.id + " — " + c.title));
      const actions = el("div", "m-actions");
      const b = el("button", "btn ghost", "Link");
      b.onclick = () => link(c.id);
      actions.appendChild(b);
      row.appendChild(actions);
      list.appendChild(row);
    }
  }

  async function reload() {
    try {
      cards = await get("/board") || [];
    } catch (e) {
      toast(e.message || "Failed to load the board", true);
    }
    render();
  }

  await reload();
}

function renderSidebar() {
  const list = $("#session-list");
  list.innerHTML = "";
  // The workspace mirrors the tab bar: real sessions come from the server
  // session store, and pending tabs (sessions not created yet, e.g. when the
  // create failed) are rendered alongside them so both views always show
  // the same set of sessions.
  const items = [];
  const archivedItems = [];
  for (const s of S.sessions) {
    if (s.parentID) continue;
    if (s.time && s.time.archived) { archivedItems.push({ id: s.id, title: s.title, updated: s.time.updated, sess: s }); continue; }
    items.push({ id: s.id, title: s.title, updated: s.time.updated, sess: s });
  }
  for (const id of S.tabs) {
    if (!isPendingTab(id)) continue;
    items.push({ id, title: "New session", updated: Date.now(), pending: true });
  }
  items.sort((a, b) => b.updated - a.updated);
  archivedItems.sort((a, b) => b.updated - a.updated);
  if (!items.length && !archivedItems.length) {
    list.appendChild(el("div", "empty-sessions", "Nothing here yet<br>Create a session to get started"));
    return;
  }
  if (!items.length) {
    list.appendChild(el("div", "session-group", "No active chats"));
  }
  let lastGroup = "";
  for (const it of items) {
    const g = groupDay(it.updated);
    if (g !== lastGroup) {
      lastGroup = g;
      list.appendChild(el("div", "session-group", esc(g)));
    }
    const row = el("div", "session-row" + (it.id === S.current ? " active" : ""));
    row.onclick = () => navigate("/" + it.id);
    if (it.pending) {
      // not on the server yet: nothing to rename or archive
      row.appendChild(el("span", "dot", "&nbsp;"));
      row.appendChild(el("span", "title", esc(it.title)));
      list.appendChild(row);
      continue;
    }
    const s = it.sess;
    list.appendChild(buildSessionRow(s));
  }
  if (archivedItems.length) renderArchivedSection(list, archivedItems);
  renderTabs();
}

// Build one sidebar session row (dot/spinner state, title, alarm clock,
// archive button). Shared by renderSidebar and updateSidebarRow so a single
// row can be patched in place without rebuilding the list.
function buildSessionRow(s) {
  const row = el("div", "session-row" + (s.id === S.current ? " active" : ""));
  row.dataset.sid = s.id;
  row.onclick = () => navigate("/" + s.id);
  row.oncontextmenu = (e) => {
    e.preventDefault();
    openContextMenu(e.clientX, e.clientY, [
      { label: "Rename", icon: ICONS.edit, action: () => openRenamePopover(e.clientX, e.clientY, s) },
      { label: "Archive", icon: ICONS.archive, action: () => archiveSession(s) },
      { label: "Delete…", icon: ICONS.trash, action: () => deleteSessionConfirm(s) },
    ]);
  };
  if (S.statuses[s.id] && S.statuses[s.id].type !== "idle") {
    row.appendChild(el("span", "spinner", ICONS.spinner));
  } else if (S.permissions.some((p) => p.sessionID === s.id)) {
    row.appendChild(el("span", "dot dot-permission"));
  } else if (S.errored.has(s.id)) {
    row.appendChild(el("span", "dot dot-error"));
  } else if (S.unseen.has(s.id)) {
    row.appendChild(el("span", "dot dot-unseen"));
  } else {
    row.appendChild(el("span", "dot", "&nbsp;"));
  }
  row.appendChild(el("span", "title", esc(s.title || "New session")));
  if (S.alarms[s.id]) {
    const clock = el("span", "alarm-indicator", "⏰");
    clock.title = alarmTitle(s.id);
    clock.onclick = (e) => { e.stopPropagation(); openAlarmsModal(s.id); };
    row.appendChild(clock);
  }
  const arch = el("button", "archive", ICONS.archive);
  arch.title = "Archive";
  arch.onclick = (e) => { e.stopPropagation(); archiveSession(s); };
  row.appendChild(arch);
  return row;
}

// Patch a single session row in place (spinner/dot/title/alarm changes)
// instead of rebuilding the whole sidebar — status events fire per subagent
// lifecycle and the rebuild destroyed hover state each time. No-op for
// sessions that have no row (subagents, archived, pending tabs); falls back
// to a full renderSidebar when the row is missing from the DOM (the list is
// sorted by time.updated, which status events don't touch, so an existing
// row's position is always still correct).
function updateSidebarRow(sid) {
  const s = S.sessions.find((x) => x.id === sid);
  if (!s || s.parentID || (s.time && s.time.archived)) return;
  const row = $(`#session-list .session-row[data-sid="${cssId(sid)}"]`);
  if (!row) { renderSidebar(); return; }
  row.replaceWith(buildSessionRow(s));
  renderTabs();
}

// Archive is reversible: the session is flagged (time.archived) and hidden
// from the list, but nothing is removed — restore it from the Archived
// section. The session.updated broadcast re-renders the sidebar.
async function archiveSession(s) {
  try {
    await patch("/session/" + s.id, { time: { archived: Date.now() } });
    toast("Chat archived — find it under Archived");
  } catch (e) {
    toast(e.message || String(e), true);
  }
}

// Delete is permanent: the session directory is removed server-side. Asks
// for confirmation, since unlike archive there is no way back.
async function deleteSessionConfirm(s) {
  const ok = await confirmModal(
    "Delete chat",
    `Delete "${esc(s.title || "New session")}" permanently? The conversation and its messages are removed for good. This cannot be undone.`,
    "Delete"
  );
  if (!ok) return;
  try {
    await del("/session/" + s.id);
    toast("Chat deleted");
  } catch (e) {
    toast(e.message || String(e), true);
  }
}

// Collapsed "Archived" section at the bottom of the sidebar: every archived
// chat with Restore (unarchive) and Delete (permanent) actions.
let archivedOpen = false;

function renderArchivedSection(list, archivedItems) {
  const head = el("button", "archived-head");
  head.innerHTML = `${ICONS.chev}<span>Archived (${archivedItems.length})</span>`;
  head.classList.toggle("open", archivedOpen);
  head.onclick = () => { archivedOpen = !archivedOpen; renderSidebar(); };
  list.appendChild(head);

  const wrap = el("div", "archived-list");
  if (!archivedOpen) wrap.style.display = "none";
  for (const it of archivedItems) {
    const row = el("div", "session-row archived-row" + (it.id === S.current ? " active" : ""));
    row.title = "Archived " + new Date(it.sess.time.archived).toLocaleString();
    row.onclick = () => navigate("/" + it.id);
    row.appendChild(el("span", "title", esc(it.title || "New session")));
    const restore = el("button", "archive", ICONS.archive);
    restore.title = "Restore";
    restore.onclick = async (e) => {
      e.stopPropagation();
      try {
        await patch("/session/" + it.id, { time: { archived: 0 } });
        toast("Chat restored");
      } catch (err) {
        toast(err.message || String(err), true);
      }
    };
    const rm = el("button", "archive", ICONS.trash);
    rm.title = "Delete permanently";
    rm.onclick = (e) => { e.stopPropagation(); deleteSessionConfirm(it.sess); };
    row.append(restore, rm);
    wrap.appendChild(row);
  }
  list.appendChild(wrap);
}

function navigate(path) {
  if (isNarrow() && S.sidebarOpen) setSidebar(false);
  rememberLastSession(path.replace(/^\//, ""));
  location.hash = path === "" ? "" : "#" + path;
}

// Remember the last open chat per workspace (wsGet/wsSet are DIR-scoped) so
// switching back to a workspace — or reloading the page — restores it. Only
// real sessions count: the landing, board and cron views never overwrite it.
function rememberLastSession(id) {
  if (id && !isPendingTab(id) && id !== "board" && id !== "cron") {
    wsSet("lastSession", id);
  }
}

// The workspace's last open chat, if it still exists (deleted, archived and
// subagent sessions are not restored).
function restorableSession() {
  const last = wsGet("lastSession");
  if (!last || isPendingTab(last)) return null;
  return S.sessions.find((s) => s.id === last && !s.parentID && !(s.time && s.time.archived)) || null;
}

// Only the last MESSAGE_WINDOW display units are fetched and rendered. The
// unit count follows the chat's display rules — a user prompt, a response
// text, a thinking block and a tool call are each one message; auxiliary
// parts (step markers, patches, ...) are free. The server trims whole
// messages, so a fetch can return slightly more than the window; the client
// re-trims to at most the window (see trimWindow). Long sessions stay smooth
// (the timeline is fully rebuilt on every render) and a page refresh
// transfers a bounded payload. Older messages are simply not shown; SSE
// events for them are ignored so they can't land out of order.
const MESSAGE_WINDOW = 100;

// Display-unit accounting for one message, mirroring the server's rule and
// what renderPart/renderUserMessage actually draw.
function countUnits(store, mid) {
  const info = store.info[mid];
  if (!info) return 0;
  const parts = Object.values(store.parts[mid] || {});
  if (info.role === "user") {
    return parts.some((p) => p.type === "file" || (p.type === "text" && !p.synthetic)) ? 1 : 0;
  }
  return parts.filter((p) =>
    p.type === "tool" || p.type === "reasoning" ||
    (p.type === "text" && !p.synthetic && !p.ignored)
  ).length;
}

// Keeps a live timeline bounded: as new messages and parts stream in, the
// oldest whole messages are dropped once the window exceeds MESSAGE_WINDOW
// units. The newest message is never dropped (a single message may alone
// exceed the window), mirroring the server's whole-message trim.
function trimWindow(store) {
  let total = 0;
  for (const mid of store.order) total += countUnits(store, mid);
  while (total > MESSAGE_WINDOW && store.order.length > 1) {
    const mid = store.order[0];
    total -= countUnits(store, mid);
    store.order.shift();
    delete store.info[mid];
    delete store.parts[mid];
  }
}

async function loadMessages(sid) {
  const msgs = await get(`/session/${sid}/message?limit=${MESSAGE_WINDOW}`);
  const store = msgStore(sid);
  // Text/reasoning parts are persisted server-side only at their start (empty
  // text) and end (full text) — deltas are SSE-only events. Re-fetching
  // mid-stream must not wipe what the client has buffered so far, or switching
  // back to a tab where the agent is thinking would show only the tokens that
  // arrive after the switch. Keep whichever text is longer: while a part is
  // streaming the local buffer is ahead (server has ""), and once it ends both
  // hold the same full text.
  const prevParts = store.parts;
  store.order = [];
  store.info = {};
  store.parts = {};
  for (const m of msgs) {
    store.order.push(m.info.id);
    store.info[m.info.id] = m.info;
    store.parts[m.info.id] = {};
    for (const part of m.parts || []) {
      const old = prevParts[m.info.id] && prevParts[m.info.id][part.id];
      if (old && (part.type === "text" || part.type === "reasoning") &&
          (old.text || "").length > (part.text || "").length) {
        part.text = old.text;
      }
      store.parts[m.info.id][part.id] = part;
    }
  }
  // the server guarantees at least the window, in whole messages; enforce
  // the strict cap here so the display never exceeds it either
  trimWindow(store);
}

/* ---------- main view ---------- */

function renderMain() {
  const main = $("#main");
  // full rebuild — carry composer focus/caret across (navigation happens
  // right after a send, and typing must survive it)
  const oldTa = document.activeElement && document.activeElement.tagName === "TEXTAREA"
    && document.activeElement.closest("#composer-region") ? document.activeElement : null;
  let selS = 0, selE = 0;
  if (oldTa) { try { selS = oldTa.selectionStart; selE = oldTa.selectionEnd; } catch {} }
  main.innerHTML = "";
  main.appendChild(buildWorkspaceBar());
  main.appendChild(buildTabBar());
  renderTabs();
  // the animated background is global: bg.js applies it at the root level and
  // starts it on load, so it is visible on every view. Here we only pick the
  // strength — full on the landing (new session) view, dimmed behind the
  // panels everywhere else (see style.css). The setActive re-assert is a
  // no-op unless the canvas was wiped from the DOM and re-attached.
  const landing = S.view === "sessions" && (!S.current || isPendingTab(S.current));
  document.body.classList.toggle("bg-live", landing);
  document.body.classList.toggle("bg-dim", !landing);
  if (window.IntellexorBG) IntellexorBG.setActive(S.bg);
  if (S.view === "board") {
    renderBoardView(main);
    return;
  }
  if (S.view === "cron") {
    renderCronView(main);
    return;
  }
  if (!S.current || isPendingTab(S.current)) {
    renderNewSessionView(main);
  } else {
    renderSessionView(main);
  }
  if (oldTa) {
    const ta = $("#composer-region textarea");
    if (ta) {
      ta.focus();
      try { ta.setSelectionRange(selS, selE); } catch {}
    }
  }
}

function renderNewSessionView(main) {
  const view = el("div", "new-session-view");
  const inner = el("div", "new-session-inner");
  view.appendChild(inner);
  const brand = el("div", "brand-mark", '<img src="/logo.png" alt="Intellexor">');
  inner.appendChild(brand);
  inner.appendChild(el("div", "wordmark", "Intellexor"));
  inner.appendChild(el("div", "new-hint", "Ask anything, / for commands, @ for context…"));
  const region = el("div", "composer-region");
  region.id = "composer-region";
  region.style.marginTop = "32px";
  inner.appendChild(region);
  buildComposerRegion(region);
  main.appendChild(view);
}

function renderSessionView(main) {
  const sess = sessionInfo(S.current);
  if (sess && sess.parentID) {
    renderSubagentView(main, sess);
    return;
  }
  const wrap = el("div");
  wrap.style.cssText = "flex:1;display:flex;flex-direction:column;min-height:0;";

  // header
  const header = el("header", "chat-header");
  header.id = "chat-header";
  main.appendChild(buildHeader(sess));
  main.appendChild(buildTimeline());
  main.appendChild(buildComposerRegionRoot());
  renderTimeline();
}

function renderSubagentView(main, sess) {
  const header = el("header", "chat-header subagent-header");
  const back = el("button", "btn ghost", "← Back to parent");
  back.onclick = () => navigate("/" + sess.parentID);
  header.appendChild(back);
  header.appendChild(el("span", "ro-badge", "subagent · read-only"));
  header.appendChild(el("div", "spacer"));
  header.appendChild(el("span", "title", esc((sess.title || "Subagent").replace(/\s+\(@[^)]+ subagent\)$/, ""))));
  main.appendChild(header);
  main.appendChild(buildTimeline());
  renderTimeline();
}

function buildHeader(sess) {
  const main = $("#main");
  let header = $("#chat-header");
  if (!header) {
    header = el("header", "chat-header");
    header.id = "chat-header";
  }
  header.innerHTML = "";
  if (sess) {
    const t = el("div", "title", esc(sess.title || "New session"));
    t.title = "Click to rename";
    t.ondblclick = () => {
      t.contentEditable = "true";
      t.focus();
      document.getSelection().selectAllChildren(t);
    };
    t.onblur = async () => {
      t.contentEditable = "false";
      const v = t.textContent.trim();
      if (v && v !== sess.title) {
        await patch("/session/" + sess.id, { title: v });
        sess.title = v;
        renderSidebar();
      }
    };
    header.appendChild(t);
    if (S.alarms[sess.id]) {
      const clock = el("span", "alarm-indicator", "⏰");
      clock.title = alarmTitle(sess.id);
      clock.onclick = () => openAlarmsModal(sess.id);
      header.appendChild(clock);
    }
  }
  header.appendChild(el("div", "spacer"));
  // Export/Import sit after the spacer so they hug the right edge, exactly
  // like the board header's links (title · spacer · actions).
  if (sess) header.appendChild(buildHeaderActions(sess));
  header.appendChild(buildContextCircle(sess));
  return header;
}

// Export / Import links. Export downloads the full session as JSON; Import
// is offered only while the session has no messages (a fresh session) and
// loads an exported file into a brand-new session.
function buildHeaderActions(sess) {
  const wrap = el("div", "header-actions");
  const exp = el("a", "header-link", "Export");
  exp.href = "#";
  exp.title = "Download the full session as a JSON file";
  exp.onclick = (e) => { e.preventDefault(); exportSession(sess); };
  wrap.appendChild(exp);
  if (sessionImportable(sess.id)) {
    const imp = el("a", "header-link", "Import");
    imp.href = "#";
    imp.title = "Import a session JSON file into a new session";
    imp.onclick = (e) => { e.preventDefault(); importSessionFile(); };
    wrap.appendChild(imp);
  }
  const sched = el("a", "header-link", "Add scheduled message");
  sched.href = "#";
  sched.title = "Schedule a message the session receives at a chosen time (like an alarm the agent sets itself)";
  sched.onclick = (e) => { e.preventDefault(); openScheduleMessageModal(sess.id); };
  wrap.appendChild(sched);
  const link = el("a", "header-link", "Link card");
  link.href = "#";
  link.title = "Link this session to a kanban card — an existing open one or a newly created one; the card moves to In Progress";
  link.onclick = (e) => { e.preventDefault(); openLinkCardModal(sess); };
  wrap.appendChild(link);
  return wrap;
}

// Import is only available on an empty session: the message store must be
// loaded (loadSession ran) and hold nothing. buildHeader runs again after
// loadSession and on the first incoming message, so the link appears and
// retires at the right moments.
function sessionImportable(sid) {
  if (!sid || isPendingTab(sid)) return false;
  if (!S.msgs.has(sid)) return false;
  return !S.msgs.get(sid).order.length;
}

// Download the full session (every message, tool call and part — not just
// the rendered window) as a JSON file.
async function exportSession(sess) {
  try {
    const data = await get(`/session/${sess.id}/export`);
    const name = `intellexor-session-${sess.slug || sess.id}.json`;
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = name;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
    toast("Session exported");
  } catch (e) {
    toast("Export failed: " + (e && e.message ? e.message : e), true);
  }
}

// Browse for an exported session JSON and load it as a completely new
// session (fresh id, full history in the backend). Navigating to it renders
// the usual last-100-units window, exactly like a page refresh would.
function importSessionFile() {
  const input = document.createElement("input");
  input.type = "file";
  input.accept = "application/json,.json";
  input.onchange = async () => {
    const file = input.files && input.files[0];
    if (!file) return;
    try {
      const data = JSON.parse(await file.text());
      const dup = await post("/session/import", data);
      if (!dup || !dup.id) throw new Error("server returned no session");
      trackSession(dup);
      toast("Session imported");
      navigate("/" + dup.id);
    } catch (e) {
      toast("Import failed: " + (e && e.message ? e.message : e), true);
    }
  };
  input.click();
}

// rebuild the header of the current session in place
function renderHeader() {
  const sess = S.sessions.find((s) => s.id === S.current);
  if (!sess || !$("#chat-header")) return;
  buildHeader(sess);
}

// Targeted header refresh for session.updated — the server publishes it once
// per LLM step (context/token counts move) and a full rebuild each time was
// wasted work. Patches only what the event can change: title text, alarm
// clock, action links (Import appears/retires) and the context circle.
// Falls back to a full buildHeader when the header isn't built yet.
function updateHeaderSession(sess) {
  const header = $("#chat-header");
  if (!header || !sess || !header.querySelector(".spacer")) { renderHeader(); return; }
  const t = header.querySelector(".title");
  if (t && t.contentEditable !== "true") {
    const want = sess.title || "New session";
    if (t.textContent !== want) t.textContent = want;
  }
  const clock = header.querySelector(".alarm-indicator");
  if (S.alarms[sess.id] && !clock) {
    const c = el("span", "alarm-indicator", "⏰");
    c.title = alarmTitle(sess.id);
    c.onclick = () => openAlarmsModal(sess.id);
    header.insertBefore(c, header.querySelector(".spacer"));
  } else if (!S.alarms[sess.id] && clock) {
    clock.remove();
  }
  const actions = header.querySelector(".header-actions");
  if (actions) actions.replaceWith(buildHeaderActions(sess));
  const circle = header.querySelector(".ctx-circle");
  const fresh = buildContextCircle(sess);
  if (circle) circle.replaceWith(fresh);
  else header.appendChild(fresh);
}

function buildContextCircle(sess) {
  const size = 16;
  const wrap = el("span", "ctx-circle");
  const total = ctxTokens(sess);
  const limit = ctxLimit(sess);
  const pct = limit > 0 ? Math.min(1, total / limit) : 0;
  const r = 6, c = 2 * Math.PI * r;
  wrap.innerHTML =
    `<svg width="${size}" height="${size}" viewBox="0 0 16 16">
      <circle cx="8" cy="8" r="${r}" fill="none" stroke="var(--layer-3)" stroke-width="2"/>
      <circle cx="8" cy="8" r="${r}" fill="none" stroke="var(--accent)" stroke-width="2"
        stroke-dasharray="${(pct * c).toFixed(1)} ${c.toFixed(1)}" transform="rotate(-90 8 8)"/>
    </svg>`;
  wrap.title = limit
    ? `Context: ${fmtNum(total)} / ${fmtNum(limit)} (${((total / limit) * 100).toFixed(1)}%)`
    : `Context: ${fmtNum(total)} tokens`;
  wrap.onclick = (e) => {
    openPopover(wrap, ctxPopover(sess));
    e.stopPropagation();
  };
  return wrap;
}

function tokensTotal(sess) {
  if (!sess || !sess.tokens) return 0;
  const t = sess.tokens;
  const cache = t.cache || {};
  return (t.input || 0) + (t.output || 0) + (t.reasoning || 0) + (cache.read || 0) + (cache.write || 0);
}

// Current context size: what the NEXT request will contain. sess.context is
// tracked by the backend as the last step's own usage (input + output +
// reasoning + cache read/write). Tokens on the session, in contrast, are
// cumulative across all steps — every step re-counts the prompt and every
// cached request re-counts the whole cached prefix, so that total is a
// billing metric, not a capacity one; using it here made the circle race to
// ~100% on short conversations.
function ctxTokens(sess) {
  if (sess && typeof sess.context === "number" && isFinite(sess.context)) return sess.context;
  const last = lastAssistantTokens(sess && sess.id);
  if (last != null) return last;
  return tokensTotal(sess);
}

// Fallback for sessions stored before the context field existed: the last
// assistant message carries its own step's usage, which is the best estimate
// of the current context. Returns null when messages aren't loaded.
function lastAssistantTokens(sid) {
  if (!sid || !S.msgs.has(sid)) return null;
  const store = S.msgs.get(sid);
  for (let i = store.order.length - 1; i >= 0; i--) {
    const info = store.info[store.order[i]];
    if (info && info.role === "assistant" && info.tokens) {
      const t = info.tokens;
      const cache = t.cache || {};
      const total = (t.input || 0) + (t.output || 0) + (t.reasoning || 0) + (cache.read || 0) + (cache.write || 0);
      if (isFinite(total)) return total;
    }
  }
  return null;
}

function ctxLimit(sess) {
  const m = modelForSession(sess);
  if (!m) return 0;
  return (m.limit && m.limit.context) || 0;
}

function ctxPopover(sess) {
  const box = el("div", "ctx-rows");
  const total = tokensTotal(sess);
  const ctx = ctxTokens(sess);
  const limit = ctxLimit(sess);
  const cost = (sess && sess.cost) || 0;
  const row = (k, v) => box.appendChild(el("div", "r", `<span>${k}</span><b>${v}</b>`));
  row("Total Tokens", fmtNum(total));
  row("Context", fmtNum(ctx));
  if (limit) row("Context Limit", fmtNum(limit));
  if (limit) row("Usage", ((ctx / limit) * 100).toFixed(1) + "%");
  const t = (sess && sess.tokens) || {};
  row("Input", fmtNum(t.input || 0));
  row("Output", fmtNum(t.output || 0));
  row("Reasoning", fmtNum(t.reasoning || 0));
  row("Cache Read", fmtNum((t.cache && t.cache.read) || 0));
  row("Cache Write", fmtNum((t.cache && t.cache.write) || 0));
  row("Total Cost", "$" + cost.toFixed(4));
  return box;
}

function fmtNum(n) {
  if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + "k";
  return String(Math.round(n));
}

/* ---------- timeline ---------- */

function buildTimeline() {
  const tl = el("div", "timeline");
  tl.id = "timeline";
  const inner = el("div", "timeline-inner");
  inner.id = "timeline-inner";
  tl.appendChild(inner);
  // floating "to bottom" button. It is a DOM child of the scroller but
  // .timeline is not positioned, so its containing block is #main
  // (position:relative) — it stays pinned above the composer instead of
  // scrolling away with the messages.
  const jb = el("button", "jump-latest", ICONS.down);
  jb.id = "jump-latest";
  jb.title = "Jump to latest message";
  jb.onclick = () => {
    S.scrollStick = true;
    tl.scrollTop = tl.scrollHeight;
  };
  tl.appendChild(jb);
  tl.onscroll = () => updateJumpButton(tl);
  return tl;
}

// show the button when the user is scrolled up far enough that new content
// is arriving out of view; hide again near the bottom
function updateJumpButton(tl) {
  const nearBottom = tl.scrollHeight - tl.scrollTop - tl.clientHeight < 60;
  S.scrollStick = nearBottom;
  const jb = $("#jump-latest");
  if (jb) jb.classList.toggle("show", !nearBottom && tl.scrollHeight > tl.clientHeight + 200);
}

function buildComposerRegionRoot() {
  const region = el("div", "composer-region");
  region.id = "composer-region";
  buildComposerRegion(region);
  return region;
}

async function renderTimeline() {
  const inner = $("#timeline-inner");
  if (!inner || !S.current) return;
  const store = msgStore(S.current);
  const tl = $("#timeline");
  const wasStick = S.scrollStick;
  // Anchor for non-stick rebuilds: wiping the DOM collapses #timeline-inner,
  // the browser clamps scrollTop to 0, and the user is dumped at the top.
  // Remember the offset and put it back after the rebuild.
  const prevTop = tl ? tl.scrollTop : 0;
  inner.innerHTML = "";

  const sess = S.sessions.find((s) => s.id === S.current);
  let turnFiles = [];

  if (sess && sess.revert) {
    const banner = el("div", "revert-banner");
    banner.appendChild(el("span", "revert-text", ICONS.revert + " Conversation reverted — later messages are hidden and file changes were restored."));
    const restore = el("button", "revert-restore", ICONS.restore + " Restore");
    restore.title = "Bring back the reverted messages and re-apply the file changes";
    restore.onclick = async () => {
      try {
        await post(`/session/${S.current}/unrevert`);
        await loadMessages(S.current);
        await refreshSession(S.current);
        S.scrollStick = true; // restoring is explicit — land on the latest
        renderTimeline();
        toast("Messages restored");
      } catch (e) {
        toast(e.message || String(e), true);
      }
    };
    banner.appendChild(restore);
    inner.appendChild(banner);
  }

  for (const mid of store.order) {
    const info = store.info[mid];
    const parts = Object.values(store.parts[mid] || {}).sort((a, b) => (a.id < b.id ? -1 : 1));
    if (!info) continue;
    if (info.role === "user") {
      const userNode = renderUserMessage(info, parts);
      if (userNode) inner.appendChild(userNode);
      turnFiles = [];
    } else {
      inner.appendChild(renderAssistantMessage(info, parts));
    }
  }
  // thinking indicator — the agent · model label lives inside the bar
  // (right-aligned) instead of on its own line under it.
  // While the session is compacting (manual /compact publishes a "compact"
  // status; auto compaction sets time.compacting inside the busy run) the
  // row is labelled "Compacting" instead of "Thinking" — opencode shows the
  // same distinction in its status line.
  const st = S.statuses[S.current];
  const curInfo = S.sessions.find((s) => s.id === S.current);
  const compacting = !!st && (st.type === "compact" ||
    (st.type === "busy" && curInfo && curInfo.time && !!curInfo.time.compacting));
  const think = buildThinkingBlock(st, {
    compacting,
    // busy only shows a row when the tail block has no visible content yet;
    // retry and compacting always show
    hidden: !!st && st.type === "busy" && !compacting && lastBlockHasText(store),
    runMeta: thinkMeta(currentRunLabel(store)),
  });
  if (think) inner.appendChild(think);
  // turn files summary
  if (turnFiles.length) {
    inner.appendChild(renderTurnFiles(turnFiles));
  }
  if (wasStick) tl.scrollTop = tl.scrollHeight;
  else {
    // restore the pre-rebuild offset (clamped if content shrank), then
    // refresh jump-button visibility (no scroll event fires if the offset
    // didn't actually change)
    tl.scrollTop = prevTop;
    updateJumpButton(tl);
  }
}

function lastBlockHasText(store) {
  for (let i = store.order.length - 1; i >= 0; i--) {
    const info = store.info[store.order[i]];
    if (info && info.role === "user") return false;
    const parts = Object.values(store.parts[store.order[i]] || {});
    for (const p of parts) {
      if ((p.type === "text" || p.type === "tool" || p.type === "reasoning")) return true;
    }
  }
  return false;
}

// Build the "Thinking / Compacting / retrying" indicator block. Shared by the
// main timeline and the board chat panel so both render identically.
// opts.compacting labels the row "Compacting"; opts.hidden suppresses the row
// (busy with visible tail content); opts.runMeta is the right-aligned
// agent · model label. Returns null when nothing should be shown.
function buildThinkingBlock(st, opts = {}) {
  if (!st || (st.type !== "busy" && st.type !== "retry" && st.type !== "compact")) return null;
  if (opts.hidden) return null;
  const row = el("div", "thinking-row");
  if (st.type === "retry") {
    row.appendChild(el("span", "retry", `retrying in ${Math.max(0, Math.round(((st.next || 0) - Date.now()) / 1000))}s — attempt #${st.attempt}`));
  } else if (opts.compacting) {
    row.appendChild(el("span", "ricon", "✳"));
    row.appendChild(el("span", "shimmer", "Compacting"));
    row.appendChild(thinkingDots(true));
  } else {
    row.appendChild(el("span", "ricon", "🧠"));
    row.appendChild(el("span", "shimmer", "Thinking"));
    row.appendChild(thinkingDots(true));
  }
  if (opts.runMeta) row.appendChild(opts.runMeta);
  const think = el("div", "assistant-block thinking-block");
  think.appendChild(row);
  return think;
}

// Show/hide/refresh the thinking indicator at the end of a timeline without a
// full rebuild — session.status busy/idle used to re-render the whole
// timeline just for this. `inner` is the timeline container, `sid` the
// conversation the status belongs to, `opts` forwarded to buildThinkingBlock
// (plus opts.stick: the container's stick-to-bottom check, since the board
// chat panel has its own).
function updateThinkingIndicatorFor(inner, sid, opts = {}) {
  if (!inner) return;
  const old = inner.querySelector(":scope > .thinking-block");
  const st = S.statuses[sid];
  const fresh = st ? buildThinkingBlock(st, opts) : null;
  // identical markup (busy → busy): keep the existing node so the dot
  // animation and any text selection inside survive
  if (old && fresh && old.innerHTML === fresh.innerHTML) return;
  if (old) old.remove();
  if (!fresh) return;
  inner.appendChild(fresh);
  const tl = inner.parentElement;
  const stick = opts.stick ? opts.stick(tl) : S.scrollStick;
  if (tl && stick) tl.scrollTop = tl.scrollHeight;
}

// Main-timeline wrapper: recompute the visibility/label opts for S.current.
function updateThinkingIndicator() {
  const inner = $("#timeline-inner");
  if (!inner || !S.current) return;
  const store = msgStore(S.current);
  const st = S.statuses[S.current];
  const curInfo = S.sessions.find((s) => s.id === S.current);
  const compacting = !!st && (st.type === "compact" ||
    (st.type === "busy" && curInfo && curInfo.time && !!curInfo.time.compacting));
  updateThinkingIndicatorFor(inner, S.current, {
    compacting,
    hidden: !!st && st.type === "busy" && !compacting && lastBlockHasText(store),
    runMeta: thinkMeta(currentRunLabel(store)),
  });
}

// Board-chat wrapper: same, for the orchestrator conversation panel.
function updateBoardChatThinking() {
  const inner = $("#board-chat-timeline-inner");
  const sid = boardChatSessionId();
  if (!inner) return;
  const store = sid ? S.msgs.get(sid) : null;
  const st = sid ? S.statuses[sid] : null;
  updateThinkingIndicatorFor(inner, sid, {
    compacting: !!st && st.type === "compact",
    hidden: !!st && st.type === "busy" && (!store || lastBlockHasText(store)),
    stick: (tl) => boardChatNearBottom(tl),
  });
}

// "agent · model" for the run in progress — taken from the streaming
// assistant message when it exists (it is the last one and not yet
// completed), else from the composer selection
function currentRunLabel(store) {
  for (let i = store.order.length - 1; i >= 0; i--) {
    const info = store.info[store.order[i]];
    if (info && info.role === "assistant") {
      if (!(info.time && info.time.completed)) {
        return [info.agent, info.modelID].filter(Boolean).join(" · ");
      }
      break;
    }
  }
  return [S.agent, S.model && S.model.modelID].filter(Boolean).join(" · ");
}

// right-aligned agent · model label embedded at the end of a thinking bar
// (null when there is nothing to show)
function thinkMeta(text) {
  if (!text) return null;
  return el("span", "think-meta", esc(text));
}

// sid: the session the message belongs to. Defaults to S.current (the main
// timeline); the board chat passes its orchestrator session so revert/fork
// act on the right conversation.
function renderUserMessage(info, parts, sid) {
  const target = sid || S.current;
  const wrap = el("div", "msg-user");
  const text = parts.filter((p) => p.type === "text" && !p.synthetic).map((p) => p.text || "").join("\n");
  const files = parts.filter((p) => p.type === "file");
  // synthetic-only user messages (e.g. the post-compaction resume prompt) render nothing
  if (!text && !files.length) return null;
  if (files.length) {
    const row = el("div", "attach-row");
    for (const f of files) {
      if (f.mime && f.mime.startsWith("image/")) {
        const chip = el("div", "attach-chip");
        chip.innerHTML = `<img src="${esc(f.url)}"><span>${esc(f.filename || "image")}</span>`;
        row.appendChild(chip);
      } else {
        row.appendChild(el("div", "attach-chip", ICONS.file + `<span>${esc(f.filename || "file")}</span>`));
      }
    }
    wrap.appendChild(row);
  }
  if (text) {
    wrap.appendChild(el("div", "bubble", esc(text)));
  }
  const meta = el("div", "meta");
  meta.appendChild(el("span", "", `${esc(info.agent || "")}${info.model ? " · " + esc(info.model.modelID || "") : ""}`));
  meta.appendChild(el("span", "", fmtTime(info.time.created)));
  const revert = el("button", "msg-revert", ICONS.revert);
  revert.title = "Revert to here — removes this message and everything after it, restoring file changes";
  revert.onclick = async () => {
    if (!target) return;
    try {
      await post(`/session/${target}/revert`, { messageID: info.id });
      await loadMessages(target);
      await refreshSession(target);
      // reverting is an explicit user action — always land on the newest
      // visible message, even if the user was scrolled up
      if (target === S.current) { S.scrollStick = true; renderTimeline(); }
      else renderBoardChat(true);
      if (text) {
        if (target === S.current) setComposerText(text);
        else if (target === boardChatSessionId()) setBoardChatText(text);
      }
      toast("Reverted — message returned to the chat box, file changes restored");
    } catch (e) {
      toast(e.message || String(e), true);
    }
  };
  meta.appendChild(revert);
  const copy = el("button", "", ICONS.copy);
  copy.title = "Copy message";
  copy.onclick = () => { navigator.clipboard.writeText(text); copy.innerHTML = "copied"; };
  meta.appendChild(copy);
  wrap.appendChild(meta);
  return wrap;
}

// sid: the session the message belongs to — see renderUserMessage.
function renderAssistantMessage(info, parts, sid) {
  const target = sid || S.current;
  const block = el("div", "assistant-block");
  // lets appendPartIn add new parts to this block without a full rebuild
  block.dataset.msg = info.id;
  // a compaction summary gets the same header opencode gives its compaction
  // row — the streamed text below it is the summary itself
  if (info.summary) block.appendChild(el("div", "compaction-label", "Compaction"));
  let hasVisible = false;
  for (const p of parts) {
    const node = renderPart(p, info);
    if (node) { block.appendChild(node); hasVisible = true; }
  }
  if (info.error) {
    const card = el("div", "assistant-block");
    card.appendChild(el("div", "err-card", esc(info.error.data && info.error.data.message || "Error")));
    block.appendChild(card);
    hasVisible = true;
  }
  const meta = el("div", "assistant-meta");
  const dur = info.time.completed && info.time.created
    ? durationText(info.time.created, info.time.completed) : "";
  meta.appendChild(el("span", "", [info.agent, info.modelID, dur].filter(Boolean).join(" · ")));
  const fork = el("button", "msg-fork", ICONS.fork);
  fork.title = "Fork from here — clones the session up to this message into a new session";
  fork.onclick = async (e) => {
    e.stopPropagation(); // embedded in the tool row: don't toggle it
    if (!target) return;
    try {
      const dup = await post(`/session/${target}/fork`, { messageID: info.id });
      await refreshSession(dup.id);
      navigate("/" + dup.id);
      toast("Session forked — you are now in the copy");
    } catch (e) {
      toast(e.message || String(e), true);
    }
  };
  meta.appendChild(fork);
  // while the message is still streaming, the agent · model label is embedded
  // in the thinking bar instead; once completed, the label (with duration)
  // moves to the meta line — or, when the message ends with a tool call,
  // inside that tool row (right-aligned, like the live thinking bar) instead
  // of on its own line under it
  if (hasVisible && info.time && info.time.completed) {
    const lastNode = block.lastElementChild;
    const row = lastNode && lastNode.classList.contains("tool")
      ? lastNode.querySelector(".tool-row") : null;
    if (row) {
      meta.classList.add("inline");
      row.appendChild(meta);
    } else {
      block.appendChild(meta);
    }
  }
  return block;
}

function durationText(a, b) {
  const s = Math.max(0, Math.round((b - a) / 1000));
  if (s < 60) return s + "s";
  return Math.floor(s / 60) + "m " + (s % 60) + "s";
}

// Three-dot ellipsis used after the "Thinking" and tool row labels; animated
// only while `on` (the part is still streaming / the tool is still running).
function thinkingDots(on) {
  const d = el("span", "dots" + (on ? " animate" : ""));
  for (let i = 0; i < 3; i++) d.appendChild(el("i"));
  return d;
}

function renderPart(p, info) {
  switch (p.type) {
    case "text": {
      if (p.synthetic || p.ignored) return null;
      if (!(p.text || "").trim()) return null;
      const node = el("div", "assistant-text", renderMarkdown(p.text));
      node.dataset.part = p.id;
      return node;
    }
    case "reasoning": {
      const streaming = S.streaming.has(p.id);
      if (!(p.text || "").trim() && !streaming) return null;
      const r = el("div", "reasoning" + (streaming ? " streaming" : ""));
      // collapsed header: 🧠 Thinking [chars] ... — dots animate while streaming
      const row = el("div", "reasoning-row");
      row.appendChild(el("span", "ricon", "🧠"));
      row.appendChild(el("span", "rname", "Thinking"));
      const count = el("span", "rcount", `[${fmtNum((p.text || "").length)}]`);
      count.title = `${(p.text || "").length} characters of reasoning`;
      count.dataset.rcount = p.id;
      row.appendChild(count);
      row.appendChild(thinkingDots(streaming));
      const chev = el("span", "chev", ICONS.chev);
      row.appendChild(chev);
      // while this bar is the live one, the agent · model label lives here
      // (right-aligned) instead of in a meta line under the message — the
      // part is open (no end time) and the message not completed yet
      if (!(info.time && info.time.completed) && !(p.time && p.time.end)) {
        const m = thinkMeta([info.agent, info.modelID].filter(Boolean).join(" · "));
        if (m) row.appendChild(m);
      }
      const body = el("div", "reasoning-body");
      const node = el("div", "assistant-text", renderMarkdown(p.text || ""));
      node.dataset.part = p.id;
      body.appendChild(node);
      // collapsed by default; an explicit user choice wins and survives re-renders
      const open = S.reasoningOpen.has(p.id) ? S.reasoningOpen.get(p.id) : false;
      body.style.display = open ? "block" : "none";
      chev.classList.toggle("open", open);
      row.onclick = () => {
        const o = body.style.display === "block";
        S.reasoningOpen.set(p.id, !o);
        body.style.display = o ? "none" : "block";
        chev.classList.toggle("open", !o);
      };
      r.appendChild(row);
      r.appendChild(body);
      return r;
    }
    case "tool":
      return renderTool(p, info);
    case "file": {
      const row = el("div", "attach-row");
      row.appendChild(el("div", "attach-chip", ICONS.file + `<span>${esc(p.filename || "file")}</span>`));
      return row;
    }
    case "retry":
      return el("div", "retry", `retrying — attempt ${p.attempt}`);
    case "compaction":
      return el("div", "divider", "<span>Session compacted</span>");
    default:
      return null;
  }
}

/* ---------- tool rendering ---------- */

function toolState(p) { return p.state || { status: "pending" }; }

function renderTool(p, info) {
  const st = toolState(p);
  const name = p.tool;
  const titleRow = el("div", "tool-row");
  const wrap = el("div", "tool" + (st.status === "error" ? " error" : ""));
  // lets patchPartIn swap this row in place on tool state changes instead of
  // rebuilding the whole timeline (which resets the scroll position)
  wrap.dataset.part = p.id;
  const input = st.input || {};
  const running = st.status === "running";

  let title = name;
  let sub = "";
  let args = "";
  let icon = "";
  let changes = null;
  let bodyFn = null;
  let defaultOpen = false;
  let taskLink = null; // set for task parts: the row navigates to the subagent session

  const inStr = (v) => (typeof v === "string" ? v : v ? JSON.stringify(v) : "");

  switch (name) {
    case "bash":
      title = "Shell"; sub = truncate(inStr(input.command), 60);
      bodyFn = () => {
        const pre = el("pre");
        const out = (st.metadata && st.metadata.output) || st.output || "";
        pre.textContent = "$ " + inStr(input.command) + "\n\n" + out;
        return pre;
      };
      break;
    case "read":
      title = "Read"; sub = baseName(inStr(input.filePath));
      bodyFn = () => { const pre = el("pre"); pre.textContent = st.output || ""; return pre; };
      break;
    case "edit":
      title = "Edit"; sub = baseName(inStr(input.filePath));
      changes = changesNode(st.metadata);
      bodyFn = () => renderDiff(st.metadata && st.metadata.diff);
      defaultOpen = true;
      break;
    case "write":
      title = "Write"; sub = baseName(inStr(input.filePath));
      changes = changesNode(st.metadata);
      bodyFn = () => renderDiff(st.metadata && st.metadata.diff);
      defaultOpen = true;
      break;
    case "apply_patch":
      title = "Patch"; sub = patchSummary(st);
      changes = changesNode(st.metadata);
      bodyFn = () => renderDiff(st.metadata && st.metadata.diff);
      break;
    case "task": {
      title = (input.subagent_type ? cap(input.subagent_type) : "Agent");
      sub = inStr(input.description);
      const childSid = st.metadata && typeof st.metadata.sessionId === "string" ? st.metadata.sessionId : "";
      if (childSid) {
        // lets updateTaskRows find this row when the subagent's status changes
        wrap.dataset.taskSession = childSid;
        // opencode-style: the row opens the subagent's session (read-only)
        taskLink = childSid;
      } else {
        bodyFn = () => {
          const pre = el("pre");
          const out = (st.output || "").replace(/^<task[^>]*>\s*<task_result>\n?/, "").replace(/\n?<\/task_result>\s*<\/task>$/, "");
          pre.textContent = out || st.error || (running ? "running…" : "(no output)");
          return pre;
        };
      }
      break;
    }
    case "todowrite":
      return null;
    case "grep":
      title = "Search"; sub = inStr(input.pattern);
      bodyFn = () => { const pre = el("pre"); pre.textContent = st.output || ""; return pre; };
      break;
    case "glob":
      title = "Find files"; sub = inStr(input.pattern);
      bodyFn = () => { const pre = el("pre"); pre.textContent = st.output || ""; return pre; };
      break;
    case "webfetch":
      title = "Fetch"; sub = truncate(inStr(input.url), 60);
      bodyFn = () => { const pre = el("pre"); pre.textContent = st.output || ""; return pre; };
      break;
    case "question":
      title = "Questions";
      break;
    case "invalid":
      title = "Invalid Tool";
      break;
    default:
      title = cap(name);
      bodyFn = () => { const pre = el("pre"); pre.textContent = st.output || ""; return pre; };
  }

  const body = el("div", "tool-body");
  // header mirrors the reasoning row: 🔨 Name [sub] with the animated ellipsis
  // dots while the tool is running (same design as 🧠 Thinking [N] ...)
  titleRow.appendChild(el("span", "ricon", "🔨"));
  titleRow.appendChild(el("span", "tname", esc(title)));
  if (sub) titleRow.appendChild(el("span", "tsub", esc(sub)));
  if (!taskLink) titleRow.appendChild(thinkingDots(running));
  // while this row is the live one, the agent · model label lives here
  // (right-aligned) instead of in a meta line under the message — same as
  // the thinking bar; it moves to the meta line once the message completes
  const live = !(info.time && info.time.completed) && (st.status === "running" || st.status === "pending");
  const runMeta = live ? thinkMeta([info.agent, info.modelID].filter(Boolean).join(" · ")) : null;
  if (taskLink) {
    titleRow.classList.add("task-link");
    // while the subagent session is still ongoing, a spinner after the 🔨
    // tells the user it is still working; the row itself stays
    // clickable and opens the session (read-only)
    const childRunning = sessionBusy(taskLink);
    titleRow.title = childRunning
      ? "Subagent still working — click to open its session (read-only)"
      : "Open subagent session (read-only)";
    if (childRunning) {
      // keep the 🔨 at the beginning of the line; the spinner goes right after it
      const sp = el("span", "spinner", ICONS.spinner);
      const icon = titleRow.firstChild;
      if (icon) titleRow.insertBefore(sp, icon.nextSibling);
      else titleRow.prepend(sp);
    }
    titleRow.appendChild(el("span", "tsub ro-open", "open ↗"));
    titleRow.appendChild(el("span", "spacer"));
    if (runMeta) titleRow.appendChild(runMeta);
    titleRow.onclick = () => navigate("/" + taskLink);
    wrap.appendChild(titleRow);
    wrap.appendChild(body);
    body.style.display = "none";
    return wrap;
  }
  const chev = el("span", "chev", ICONS.chev);
  chev.id = "ch-" + p.id;
  titleRow.appendChild(chev);
  titleRow.appendChild(el("span", "spacer"));
  if (changes) titleRow.appendChild(changes);
  if (runMeta) titleRow.appendChild(runMeta);
  if (running && st.metadata && st.metadata.exit !== undefined && st.metadata.exit !== null) {
    // bash exit indicator
  }
  wrap.appendChild(titleRow);

  // The timeline is fully rebuilt on every streaming event, so the user's
  // expand/collapse choice is kept in S.toolOpen and re-applied here.
  // An explicit user choice wins over the default; otherwise edit/write rows
  // (and errored rows) start open.
  const errored = st.status === "error";
  const startOpen = S.toolOpen.has(p.id) ? S.toolOpen.get(p.id) : defaultOpen || errored;
  let filled = false;
  const fillBody = () => {
    if (filled) return;
    filled = true;
    const content = renderToolContent(p, st, name);
    if (content) body.appendChild(content);
  };

  body.style.display = startOpen ? "block" : "none";
  chev.classList.toggle("open", startOpen);
  if (startOpen) fillBody();

  titleRow.onclick = () => {
    const open = body.style.display === "block";
    S.toolOpen.set(p.id, !open);
    if (!open) fillBody();
    body.style.display = open ? "none" : "block";
    chev.classList.toggle("open", !open);
  };
  wrap.appendChild(body);

  if (errored) {
    const card = el("div", "err-card");
    card.textContent = (st.error || "Failed").toString();
    body.appendChild(card);
  }
  return wrap;
}

function renderToolContent(p, st, name) {
  if (name === "edit" || name === "write" || name === "apply_patch") {
    return renderDiff(st.metadata && st.metadata.diff);
  }
  if (name === "bash") {
    const pre = el("pre");
    const inp = st.input || {};
    pre.textContent = "$ " + (inp.command || "") + "\n\n" + ((st.metadata && st.metadata.output) || st.output || "");
    return pre;
  }
  const pre = el("pre");
  pre.textContent = st.output || "";
  return pre;
}

function patchSummary(st) {
  const files = st.metadata && st.metadata.files;
  if (Array.isArray(files) && files.length) {
    return files.length === 1 ? files[0].relativePath : files.length + " files";
  }
  return "";
}

function changesNode(md) {
  const fd = md && md.filediff;
  if (!fd || typeof fd.additions !== "number") return null;
  const n = el("span", "changes");
  n.innerHTML = `<span class="add">+${fd.additions}</span> <span class="del">-${fd.deletions}</span>`;
  return n;
}

function baseName(p) { return String(p || "").split("/").pop(); }
function truncate(s, n) { return s && s.length > n ? s.slice(0, n - 1) + "…" : s; }
function cap(s) { return s ? s[0].toUpperCase() + s.slice(1) : s; }

function renderTurnFiles(files) {
  const box = el("div", "turn-files");
  return box;
}

/* ---------- kanban board (first tab) ---------- */

const BOARD_COLUMNS = [
  { id: "open", label: "Open" },
  { id: "inprogress", label: "In Progress" },
  { id: "done", label: "Done" },
];

// Per-column collapsed state, persisted like the other UI prefs. A collapsed
// column renders as a narrow vertical button in a rail on the left of the
// board; collapsed columns stack below one another and stay droppable.
let boardCollapsed = {};
try { boardCollapsed = JSON.parse(localStorage.getItem("intellexor.boardCollapsed") || "{}"); } catch { boardCollapsed = {}; }

function setBoardCollapsed(id, collapsed) {
  boardCollapsed[id] = collapsed;
  try { localStorage.setItem("intellexor.boardCollapsed", JSON.stringify(boardCollapsed)); } catch {}
  renderBoard();
}

let boardCards = [];

// The unsaved "Add card…" draft (text + attachments) lives here, not in the
// column's closure: renderBoard() rebuilds the whole board DOM on every
// board.updated / session-status change, which would otherwise wipe whatever
// was being typed the moment an agent moves a card or a session goes idle.
let kanbanAddDraft = "";
let kanbanAddFiles = [];

// Cards that just started are detected by comparing columns across loads: a
// card that moved from open to inprogress with a session attached was
// started (via a link trigger, the agent, or another client/window) and gets
// a tab like the Start button creates — without switching to it. The first
// load only seeds the snapshot, so nothing fires spuriously.
let boardColByCard = {};

async function trackStartedCards(cards) {
  const prev = boardColByCard;
  const next = {};
  const started = [];
  for (const c of cards) {
    next[c.id] = c.column;
    if (c.column === "inprogress" && c.sessionID && prev[c.id] === "open") started.push(c.sessionID);
  }
  boardColByCard = next;
  for (const sid of started) {
    // the session normally arrives via its session.created event before the
    // board update; fetch it if it slipped through
    if (!S.sessions.some((s) => s.id === sid)) await refreshSession(sid).catch(() => {});
    openTab(sid);
  }
  if (started.length) renderTabs();
}

async function loadBoard() {
  try {
    const cards = (await get("/board")) || [];
    await trackStartedCards(cards);
    boardCards = cards;
  } catch {
    boardCards = [];
  }
  renderBoardPanel(); // keep the side panel in sync wherever the board changes
}

async function renderBoardView(main) {
  const header = el("header", "chat-header");
  header.appendChild(el("div", "title", "Board"));
  header.appendChild(el("div", "spacer"));
  header.appendChild(el("span", "board-hint", "Drag cards to move · drag ⌖ onto an Open card to link · double-click an Open card title to edit"));
  header.appendChild(buildBoardActions());
  const addBtn = el("button", "btn secondary board-add", ICONS.plus + "<span>Add card</span>");
  addBtn.title = "Create a new card in Open";
  addBtn.onclick = () => openAddCardModal();
  header.appendChild(addBtn);
  main.appendChild(header);
  // the orchestrator chat is a column of the board: it lives in the same
  // flex row as the status columns and scrolls with them
  const board = el("div", "kanban");
  board.id = "kanban";
  main.appendChild(board);
  await loadBoard();
  renderBoard();
  board.appendChild(buildBoardChat());
  initBoardChat();
}

/* ---------- board chat (orchestrator) ---------- */

// A chat area beside the kanban columns, hardcoded to the orchestrator agent:
// the coordination agent that curates the board, starts cards and schedules
// cron jobs. The conversation lives in a real session (so it is fully
// rendered by the same message renderers and streams over the same SSE
// events); its id is kept per-project in localStorage so returning to the
// board continues the same conversation. "New chat" starts a fresh session —
// the old one stays in the workspace list.

const ORCHESTRATOR_AGENT = "orchestrator";
let boardChatLoaded = false; // messages fetched for the current board view

function boardChatKey() {
  return "intellexor.boardChat." + (S.dir || DIR || "");
}

function boardChatSessionId() {
  try { return localStorage.getItem(boardChatKey()) || ""; } catch { return ""; }
}

function setBoardChatSessionId(id) {
  try {
    if (id) localStorage.setItem(boardChatKey(), id);
    else localStorage.removeItem(boardChatKey());
  } catch {}
}

function boardChatBusy() {
  const sid = boardChatSessionId();
  const st = sid && S.statuses[sid];
  return !!(st && st.type !== "idle");
}

/* ---------- board chat collapse ---------- */

// The chat can be collapsed to a slim vertical strip (title reads
// bottom-to-top) to give the kanban columns room; the state is persisted in
// localStorage and re-applied on every board render.
const BOARD_CHAT_COLLAPSED_KEY = "intellexor.boardChatCollapsed";

function boardChatCollapsed() {
  try { return localStorage.getItem(BOARD_CHAT_COLLAPSED_KEY) === "1"; } catch { return false; }
}

function setBoardChatCollapsed(v) {
  try { localStorage.setItem(BOARD_CHAT_COLLAPSED_KEY, v ? "1" : "0"); } catch {}
  applyBoardChatCollapsed();
}

function toggleBoardChatCollapsed() {
  setBoardChatCollapsed(!boardChatCollapsed());
}

// Apply the collapsed class and the matching width. Expanding restores the
// pinned resize width (or the default flex sizing when never resized) and
// re-renders, since streaming updates were skipped while hidden.
function applyBoardChatCollapsed() {
  const panel = $("#board-chat");
  if (!panel) return;
  const collapsed = boardChatCollapsed();
  panel.classList.toggle("collapsed", collapsed);
  const btn = $("#board-chat-collapse");
  if (btn) btn.title = collapsed ? "Expand" : "Collapse";
  if (collapsed) {
    panel.style.flex = "0 0 46px";
    panel.style.maxWidth = "46px";
  } else {
    let saved = 0;
    try { saved = Number(localStorage.getItem(BOARD_CHAT_WIDTH_KEY)) || 0; } catch {}
    if (saved > 0) applyBoardChatWidth(panel, saved);
    else { panel.style.flex = ""; panel.style.maxWidth = ""; }
    renderBoardChat();
  }
}

/* ---------- board chat resize ---------- */

// The orchestrator chat column is horizontally resizable: dragging its right
// edge adjusts the width (persisted in localStorage and re-applied on every
// board render); double-clicking the edge resets to the default flex sizing.
const BOARD_CHAT_MIN_W = 230;
const BOARD_CHAT_MAX_W = 720;
const BOARD_CHAT_WIDTH_KEY = "intellexor.boardChatWidth";

function applyBoardChatWidth(panel, w) {
  w = Math.round(Math.max(BOARD_CHAT_MIN_W, Math.min(BOARD_CHAT_MAX_W, w)));
  // a resized chat is pinned to the dragged width — the default flex sizing
  // (grow up to 420px) only applies until the user resizes it themselves
  panel.style.flex = `0 0 ${w}px`;
  panel.style.maxWidth = "none";
  return w;
}

function initBoardChatResize(panel) {
  const grip = el("div", "board-chat-resize");
  grip.title = "Drag to resize · double-click to reset";
  grip.ondblclick = (e) => {
    e.preventDefault();
    panel.style.flex = "";
    panel.style.maxWidth = "";
    try { localStorage.removeItem(BOARD_CHAT_WIDTH_KEY); } catch {}
  };
  grip.onpointerdown = (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    const startX = e.clientX;
    const startW = panel.getBoundingClientRect().width;
    // the grip sits on the right edge: dragging right widens the chat
    const move = (ev) => applyBoardChatWidth(panel, startW + (ev.clientX - startX));
    const up = () => {
      document.body.classList.remove("board-chat-resizing");
      window.removeEventListener("pointermove", move);
      window.removeEventListener("pointerup", up);
      window.removeEventListener("pointercancel", up);
      const w = panel.getBoundingClientRect().width;
      if (w > 0) {
        try { localStorage.setItem(BOARD_CHAT_WIDTH_KEY, String(Math.round(w))); } catch {}
      }
    };
    document.body.classList.add("board-chat-resizing");
    window.addEventListener("pointermove", move);
    window.addEventListener("pointerup", up);
    window.addEventListener("pointercancel", up);
  };
  panel.appendChild(grip);
  if (boardChatCollapsed()) return; // the strip width wins over the saved one
  let saved = 0;
  try { saved = Number(localStorage.getItem(BOARD_CHAT_WIDTH_KEY)) || 0; } catch {}
  if (saved > 0) applyBoardChatWidth(panel, saved);
}

// The chat panel skeleton: header, timeline and composer. Built once per
// board view render; updates go through renderBoardChat, which only touches
// the timeline and the chrome — typing is never interrupted.
function buildBoardChat() {
  boardChatLoaded = false;
  const panel = el("aside", "board-chat");
  panel.id = "board-chat";
  const head = el("div", "board-chat-head");
  head.innerHTML =
    `<span class="agent-dot" style="background:#0d9f6f"></span>` +
    `<span class="board-chat-title">Orchestrator</span>` +
    `<span class="spinner board-chat-spin" id="board-chat-spin" style="display:none">${ICONS.spinner}</span>`;
  const spacer = el("span", "board-chat-spacer");
  head.appendChild(spacer);
  const fresh = el("button", "board-chat-btn", ICONS.plus);
  fresh.id = "board-chat-new";
  fresh.title = "Previous chats · start a new conversation";
  fresh.onclick = boardChatReset;
  head.appendChild(fresh);
  const collapse = el("button", "board-chat-btn board-chat-collapse", ICONS.chev);
  collapse.id = "board-chat-collapse";
  collapse.title = "Collapse";
  collapse.onclick = toggleBoardChatCollapsed;
  head.appendChild(collapse);
  panel.appendChild(head);
  // clicking the vertical title of a collapsed chat expands it again
  const titleEl = head.querySelector(".board-chat-title");
  titleEl.onclick = () => { if (boardChatCollapsed()) setBoardChatCollapsed(false); };
  panel.classList.toggle("collapsed", boardChatCollapsed());

  const tl = el("div", "board-chat-timeline");
  tl.id = "board-chat-timeline";
  const inner = el("div", "board-chat-timeline-inner");
  inner.id = "board-chat-timeline-inner";
  tl.appendChild(inner);
  panel.appendChild(tl);

  panel.appendChild(buildBoardChatComposer());
  initBoardChatResize(panel);
  return panel;
}

function buildBoardChatComposer() {
  const wrap = el("div", "board-chat-composer");
  const ta = el("textarea");
  ta.id = "board-chat-input";
  ta.rows = 1;
  ta.placeholder = "Ask the orchestrator to coordinate work…";
  ta.oninput = () => autosize(ta);
  ta.onkeydown = (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      boardChatSend();
    } else if (e.key === "Escape" && boardChatBusy()) {
      e.preventDefault();
      boardChatAbort();
    }
  };
  wrap.appendChild(ta);
  const row = el("div", "board-chat-actions");
  const hint = el("span", "board-chat-hint", "manages the board · starts cards · schedules jobs");
  const send = el("button", "c-btn send", ICONS.up);
  send.id = "board-chat-send";
  send.title = "Send";
  send.onclick = () => (boardChatBusy() ? boardChatAbort() : boardChatSend());
  row.append(hint, send);
  wrap.appendChild(row);
  setTimeout(() => autosize(ta), 0);
  return wrap;
}

// Fetch the persisted conversation (verifying the session still exists —
// it may have been archived) and render it.
async function initBoardChat() {
  boardChatLoaded = false;
  const sid = boardChatSessionId();
  if (sid) {
    const ok = await get("/session/" + sid).then(() => true).catch(() => false);
    if (ok) await loadMessages(sid).catch(() => {});
    else setBoardChatSessionId("");
  }
  boardChatLoaded = true;
  renderBoardChat();
}

// Session creation for the board chat, kept per call burst: a double-Enter
// (or a send racing the create) must not mint two orchestrator sessions —
// later sends await the same promise, like the pending-tab flow.
let boardChatCreate = null;

async function ensureBoardChatSession() {
  const sid = boardChatSessionId();
  if (sid) {
    const ok = await get("/session/" + sid).then(() => true).catch(() => false);
    if (ok) return sid;
    setBoardChatSessionId(""); // archived/deleted meanwhile — start fresh
  }
  if (!boardChatCreate) {
    boardChatCreate = post("/session", { agent: ORCHESTRATOR_AGENT, title: "Orchestrator" })
      .then((sess) => {
        trackSession(sess);
        setBoardChatSessionId(sess.id);
        return sess.id;
      })
      .finally(() => { boardChatCreate = null; });
  }
  return boardChatCreate;
}

async function boardChatSend() {
  const ta = $("#board-chat-input");
  if (!ta) return;
  const text = ta.value.trim();
  if (!text) return;
  ta.value = "";
  autosize(ta);
  try {
    const sid = await ensureBoardChatSession();
    await loadMessages(sid).catch(() => {});
    await post(`/session/${sid}/prompt_async`, { agent: ORCHESTRATOR_AGENT, parts: [{ type: "text", text }] });
    await refreshSession(sid);
    renderBoardChat(true); // explicit send — always land on the latest
  } catch (e) {
    if (ta && !ta.value) ta.value = text; // give the draft back — nothing was sent
    autosize(ta);
    toast(e.message || String(e), true);
  }
}

async function boardChatAbort() {
  const sid = boardChatSessionId();
  if (!sid) return;
  soundSuppressedUntil = Date.now() + 10000; // the user stopped it — no chime
  await post(`/session/${sid}/abort`).catch(() => {});
}

// The "+" button opens the chat picker: every previous orchestrator
// conversation (sessions with the orchestrator agent, newest first) can be
// reopened in the panel, or a fresh one started. Old sessions stay in the
// workspace list either way — this only moves the panel's pointer.
async function boardChatReset() {
  let sessions = [];
  try { sessions = (await get("/session")) || []; } catch {}
  const cur = boardChatSessionId();
  const prev = sessions
    .filter((s) => !s.parentID && s.agent === ORCHESTRATOR_AGENT && s.id !== cur)
    .sort((a, b) => ((b.time && b.time.updated) || 0) - ((a.time && a.time.updated) || 0));
  openModal("Orchestrator chats", (modal, ov) => {
    const list = el("div", "board-chat-history");
    if (!prev.length) {
      list.appendChild(el("div", "board-chat-empty", "No previous orchestrator chats yet."));
    }
    for (const s of prev) {
      const ts = (s.time && (s.time.updated || s.time.created)) || 0;
      const row = el("button", "board-chat-history-item");
      row.appendChild(el("span", "bch-title", s.title || "Orchestrator"));
      row.appendChild(el("span", "bch-meta", ts ? relTime(ts) : ""));
      row.onclick = async () => {
        ov.remove();
        trackSession(s);
        setBoardChatSessionId(s.id);
        await loadMessages(s.id).catch(() => {});
        renderBoardChat();
      };
      list.appendChild(row);
    }
    modal.appendChild(list);
    const actions = el("div", "m-actions");
    const cancel = el("button", "btn ghost", "Cancel");
    cancel.onclick = () => ov.remove();
    const fresh = el("button", "btn primary", "New chat");
    fresh.onclick = () => {
      ov.remove();
      setBoardChatSessionId("");
      renderBoardChat();
      const ta = $("#board-chat-input");
      if (ta) ta.focus();
    };
    actions.append(cancel, fresh);
    modal.appendChild(actions);
  });
}

// Put text into the board chat composer (the revert flow uses it).
function setBoardChatText(text) {
  const ta = $("#board-chat-input");
  if (!ta) return;
  ta.value = text;
  autosize(ta);
  ta.focus();
  ta.selectionStart = ta.selectionEnd = ta.value.length;
}

// True when the timeline is scrolled to (or near) the bottom — new content
// should keep it there instead of yanking the user around.
function boardChatNearBottom(tl) {
  return !tl || tl.scrollHeight - tl.scrollTop - tl.clientHeight < 60;
}

// Render the orchestrator conversation into the panel. A full rebuild, like
// renderTimeline — cheap at chat scale and immune to partial-update bugs.
// forceBottom scrolls to the latest message regardless of the current scroll
// position (used after explicit user actions: send, revert).
function renderBoardChat(forceBottom) {
  if (boardChatCollapsed()) return; // hidden strip — nothing to paint
  const inner = $("#board-chat-timeline-inner");
  if (!inner) return;
  const tl = $("#board-chat-timeline");
  const stick = forceBottom || boardChatNearBottom(tl);
  // preserve the offset across the rebuild — wiping the DOM clamps scrollTop
  // to 0 and would dump a reader scrolled into an expanded tool call at the top
  const prevTop = tl ? tl.scrollTop : 0;
  inner.innerHTML = "";
  const sid = boardChatSessionId();
  if (sid) {
    const store = S.msgs.get(sid);
    if (store) {
      for (const mid of store.order) {
        const info = store.info[mid];
        if (!info) continue;
        const parts = Object.values(store.parts[mid] || {}).sort((a, b) => (a.id < b.id ? -1 : 1));
        if (info.role === "user") {
          const node = renderUserMessage(info, parts, sid);
          if (node) inner.appendChild(node);
        } else {
          inner.appendChild(renderAssistantMessage(info, parts, sid));
        }
      }
    }
    // thinking indicator while the orchestrator works (shared builder —
    // same shapes as the main timeline's)
    const st = S.statuses[sid];
    const think = buildThinkingBlock(st, {
      compacting: !!st && st.type === "compact",
      hidden: !!st && st.type === "busy" && (!store || lastBlockHasText(store)),
    });
    if (think) inner.appendChild(think);
  } else if (boardChatLoaded) {
    inner.appendChild(el("div", "board-chat-empty",
      "Chat with the orchestrator — it coordinates the work on this board: breaking tasks into cards, starting sessions on them, linking follow-ups and scheduling cron jobs."));
  }
  updateBoardChatChrome();
  if (tl) {
    if (stick) tl.scrollTop = tl.scrollHeight;
    else tl.scrollTop = prevTop;
  }
}

// Send/stop button and header spinner follow the busy state.
function updateBoardChatChrome() {
  const busy = boardChatBusy();
  const send = $("#board-chat-send");
  if (send) {
    send.className = "c-btn " + (busy ? "stop" : "send");
    send.innerHTML = busy ? ICONS.stop : ICONS.up;
    send.title = busy ? "Stop" : "Send";
  }
  const spin = $("#board-chat-spin");
  if (spin) spin.style.display = busy ? "" : "none";
}

// Live text deltas for the board chat — the panel counterpart of
// renderStreamingText. Only parts belonging to the orchestrator session are
// touched; everything else in S.streaming belongs to another timeline.
function renderBoardChatStreaming() {
  const inner = $("#board-chat-timeline-inner");
  if (!inner) return;
  const sid = boardChatSessionId();
  if (!sid) return;
  const store = S.msgs.get(sid);
  if (!store) return;
  let touched = false, needFull = false;
  for (const [pid, mid] of S.streaming) {
    if (!store.info[mid]) continue; // another conversation's part
    const part = store.parts[mid] && store.parts[mid][pid];
    if (!part) { needFull = true; continue; }
    if (deltaPaintedLenBoard.get(pid) === (part.text || "").length) continue;
    const node = inner.querySelector(`[data-part="${cssId(pid)}"]`);
    if (!node) {
      // first delta before the bubble exists — append instead of full rebuild
      if (appendPartIn(inner, part, store)) {
        deltaPaintedLenBoard.set(pid, (part.text || "").length);
        updateBoardChatThinking();
        touched = true;
        continue;
      }
      needFull = true;
      continue;
    }
    node.innerHTML = renderMarkdown(part.text || "");
    deltaPaintedLenBoard.set(pid, (part.text || "").length);
    if (part.type === "reasoning") {
      const c = inner.querySelector(`[data-rcount="${cssId(pid)}"]`);
      if (c) {
        c.textContent = `[${fmtNum((part.text || "").length)}]`;
        c.title = `${(part.text || "").length} characters of reasoning`;
      }
    }
    touched = true;
  }
  if (needFull) {
    renderBoardChat();
    for (const [pid, mid] of S.streaming) {
      const part = store.parts[mid] && store.parts[mid][pid];
      if (part) deltaPaintedLenBoard.set(pid, (part.text || "").length);
    }
    return;
  }
  if (!touched) return;
  const tl = $("#board-chat-timeline");
  if (tl && boardChatNearBottom(tl)) tl.scrollTop = tl.scrollHeight;
}

// Export / Import links for the board header, styled like the session
// header's. Export downloads the whole board as JSON; Import offers a
// file and asks whether to replace the board with it or merge it in.
function buildBoardActions() {
  const wrap = el("div", "header-actions");
  const exp = el("a", "header-link", "Export");
  exp.href = "#";
  exp.title = "Download the whole board as a JSON file";
  exp.onclick = (e) => { e.preventDefault(); exportBoard(); };
  wrap.appendChild(exp);
  const imp = el("a", "header-link", "Import");
  imp.href = "#";
  imp.title = "Import a board JSON file — replace the board or merge into it";
  imp.onclick = (e) => { e.preventDefault(); importBoardFile(); };
  wrap.appendChild(imp);
  return wrap;
}

// Download the whole board (every card, all columns) as a JSON file — the
// board counterpart of the session header's Export link.
async function exportBoard() {
  try {
    const data = await get("/board/export");
    const base = DIR.split("/").filter(Boolean).pop() || "";
    const name = `intellexor-board${base ? "-" + base : ""}.json`;
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = name;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
    toast("Board exported");
  } catch (e) {
    toast("Export failed: " + (e && e.message ? e.message : e), true);
  }
}

// Browse for an exported board JSON, then ask how it should land: replace
// the current board with it, or merge its cards into the board (fresh ids,
// done-links remapped). The choice is a modal because replace discards the
// current cards — a decision the file picker alone cannot make.
function importBoardFile() {
  const input = document.createElement("input");
  input.type = "file";
  input.accept = "application/json,.json";
  input.onchange = async () => {
    const file = input.files && input.files[0];
    if (!file) return;
    let data;
    try {
      data = JSON.parse(await file.text());
    } catch (e) {
      toast("Import failed: " + (e && e.message ? e.message : e), true);
      return;
    }
    if (!data || typeof data !== "object" || (data.format && data.format !== "intellexor.board")) {
      toast("Import failed: not an intellexor board export", true);
      return;
    }
    const n = Array.isArray(data.cards) ? data.cards.length : 0;
    openModal("Import board", (modal, ov) => {
      modal.appendChild(el("p", "m-sub",
        `“${esc(file.name)}” holds ${n} card${n === 1 ? "" : "s"}; the current board has ${boardCards.length}. How should it be imported?`));
      const actions = el("div", "m-actions");
      const cancel = el("button", "btn ghost", "Cancel");
      cancel.onclick = () => ov.remove();
      const merge = el("button", "btn secondary", "Merge into board");
      merge.title = "Keep the current board and append the imported cards";
      merge.onclick = () => { ov.remove(); importBoardData(data, "merge"); };
      const replace = el("button", "btn danger", "Replace board");
      replace.title = "Discard the current board and install the imported one";
      replace.onclick = () => { ov.remove(); importBoardData(data, "replace"); };
      actions.append(cancel, merge, replace);
      modal.appendChild(actions);
    });
  };
  input.click();
}

// Send the parsed board file to the server with the chosen mode, then
// refresh (the board.updated broadcast refreshes every other client too).
async function importBoardData(data, mode) {
  try {
    const res = await post(`/board/import?mode=${mode}`, data);
    await loadBoard();
    renderBoard();
    const n = res && res.imported ? res.imported : 0;
    toast(`Board imported — ${n} card${n === 1 ? "" : "s"} ${mode === "merge" ? "merged" : "installed"}`);
  } catch (e) {
    toast("Import failed: " + (e && e.message ? e.message : e), true);
  }
}

/* ---------- add-card modal ---------- */

// Generic modal helper: builds an overlay + .modal box, calls build(modal, ov)
// to populate it, and removes itself when the backdrop is clicked.
function openModal(title, build) {
  const ov = el("div", "overlay");
  const modal = el("div", "modal");
  ov.onclick = (e) => { if (e.target === ov) ov.remove(); };
  modal.appendChild(el("h2", "", title));
  build(modal, ov);
  ov.appendChild(modal);
  $("#modal-root").appendChild(ov);
  return ov;
}

// Cards can only be created in the Open column (work starts by moving a
// card to In Progress), so there is no column picker here. The title is
// multiline: Enter creates the card, Ctrl+Enter (or Shift+Enter) inserts a
// newline. Images/files can be attached or pasted, same as the composer.
function openAddCardModal() {
  openModal("Add card", (modal, ov) => {
    const field = el("div", "m-field");
    field.appendChild(el("label", "m-label", "Task"));
    const input = el("textarea", "m-textarea");
    input.rows = 1;
    input.placeholder = "Describe the task for the agent…";
    field.appendChild(input);
    modal.appendChild(field);

    const files = [];
    const attachRow = el("div", "attach-row");
    const redraw = () => {
      renderAttachChipsInto(attachRow, files, (a) => {
        files.splice(files.indexOf(a), 1);
        redraw();
      });
      autosize(input);
    };
    const addFiles = async (fl) => {
      files.push(...(await filesToAttachments(fl)));
      redraw();
    };
    // paste images/files straight in (screenshots, copied files)
    input.onpaste = (e) => {
      const fl = e.clipboardData && e.clipboardData.files;
      if (!fl || !fl.length) return; // plain text paste: default behaviour
      e.preventDefault();
      addFiles([...fl]);
    };
    input.oninput = () => autosize(input);

    const actions = el("div", "m-actions");
    const attachBtn = el("button", "c-btn", ICONS.plus);
    attachBtn.title = "Attach files";
    const fileInput = document.createElement("input");
    fileInput.type = "file";
    fileInput.multiple = true;
    fileInput.style.display = "none";
    fileInput.onchange = () => { addFiles(fileInput.files); fileInput.value = ""; };
    attachBtn.onclick = () => fileInput.click();
    const spacer = el("span");
    spacer.style.flex = "1";
    const cancel = el("button", "btn ghost", "Cancel");
    cancel.onclick = () => ov.remove();
    const create = el("button", "btn primary", "Create card");
    const commit = async () => {
      const title = input.value.trim();
      if (!title) { input.focus(); return; }
      create.disabled = true;
      try {
        await post("/board/card", { title, column: "open", attachments: files });
        ov.remove();
        await loadBoard();
        renderBoard();
        toast("Card added");
      } catch (e) {
        create.disabled = false;
        toast(e.message || String(e), true);
      }
    };
    create.onclick = commit;
    input.onkeydown = (e) => {
      // Enter creates; Ctrl+Enter (and Shift+Enter) inserts a newline
      if (e.key === "Enter" && !e.ctrlKey && !e.metaKey && !e.shiftKey) { e.preventDefault(); commit(); }
    };
    actions.append(attachBtn, fileInput, spacer, cancel, create);
    modal.appendChild(attachRow);
    modal.appendChild(actions);
    setTimeout(() => { autosize(input); input.focus(); }, 0);
  });
}

/* ---------- schedule a timed start ---------- */

// A card in Open can be given a startAt (unix ms): a background scheduler on
// the server starts it when the time passes — same flow as the Start button,
// so it works with no client connected. The dialog offers quick offsets plus
// a free date/time picker.

// Local timestamp formatted for the card badge: just the time today, a
// "tomorrow" prefix the next day, otherwise a short date.
function fmtSchedule(ts) {
  const d = new Date(ts);
  const time = d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  const today = new Date(); today.setHours(0, 0, 0, 0);
  if (ts >= today.getTime() && ts < today.getTime() + 86400000) return time;
  if (ts >= today.getTime() + 86400000 && ts < today.getTime() + 2 * 86400000) return "tomorrow " + time;
  return d.toLocaleDateString([], { month: "short", day: "numeric" }) + " " + time;
}

// Coarse relative time for tooltips/toasts: "in 2h 5m", "in 45m", "3d ago".
function relTime(ts) {
  let s = Math.round((ts - Date.now()) / 1000);
  const past = s < 0;
  s = Math.abs(s);
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  const parts = [];
  if (d) parts.push(d + "d");
  if (h) parts.push(h + "h");
  if (m) parts.push(m + "m");
  if (!parts.length) return past ? "just now" : "in <1m";
  const txt = parts.join(" ");
  return past ? txt + " ago" : "in " + txt;
}

// datetime-local inputs want "YYYY-MM-DDTHH:mm" in the local zone.
function toLocalInputValue(d) {
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

function openScheduleModal(c) {
  openModal(`Schedule card #${esc(c.id)}`, (modal, ov) => {
    const field = el("div", "m-field");
    field.appendChild(el("label", "m-label", "Start at"));
    const input = el("input");
    input.type = "datetime-local";
    input.step = 60;
    input.min = toLocalInputValue(new Date());
    field.appendChild(input);
    modal.appendChild(field);

    // quick offsets set the picker; the picker stays the source of truth so
    // a preset can still be fine-tuned by hand before saving
    const presets = el("div", "schedule-presets");
    for (const [label, ms] of [["In 1 hour", 36e5], ["In 2 hours", 2 * 36e5], ["In 8 hours", 8 * 36e5], ["In 24 hours", 24 * 36e5]]) {
      const b = el("button", "kanban-card-btn", "<span>" + label + "</span>");
      b.type = "button";
      b.onclick = () => { input.value = toLocalInputValue(new Date(Date.now() + ms)); updatePreview(); };
      presets.appendChild(b);
    }
    modal.appendChild(presets);

    const preview = el("div", "schedule-preview");
    const updatePreview = () => {
      const ts = input.value ? new Date(input.value).getTime() : 0;
      preview.textContent = ts ? `The agent starts this card ${relTime(ts)} — ${new Date(ts).toLocaleString([], { weekday: "short", month: "short", day: "numeric", hour: "numeric", minute: "2-digit" })}` : "Pick a date and time";
    };
    input.oninput = updatePreview;
    if (c.startAt) { input.value = toLocalInputValue(new Date(c.startAt)); }
    updatePreview();

    const actions = el("div", "m-actions");
    const spacer = el("span");
    spacer.style.flex = "1";
    const cancel = el("button", "btn ghost", "Cancel");
    cancel.onclick = () => ov.remove();
    actions.append(spacer, cancel);

    // an existing schedule can be removed right here
    if (c.startAt) {
      const remove = el("button", "btn ghost", "Remove");
      remove.onclick = async () => {
        remove.disabled = true;
        try {
          await patch("/board/card/" + c.id, { startAt: 0 });
          ov.remove();
          await loadBoard();
          renderBoard();
          toast(`Schedule removed from card #${c.id}`);
        } catch (err) {
          remove.disabled = false;
          toast(err.message || String(err), true);
        }
      };
      actions.insertBefore(remove, cancel);
    }

    const save = el("button", "btn primary", "Save");
    save.onclick = async () => {
      const ts = input.value ? new Date(input.value).getTime() : 0;
      if (!ts) { input.focus(); return; }
      save.disabled = true;
      try {
        await patch("/board/card/" + c.id, { startAt: ts });
        ov.remove();
        await loadBoard();
        renderBoard();
        toast(`Card #${c.id} scheduled — the agent starts it ${relTime(ts)}`);
      } catch (err) {
        save.disabled = false;
        toast(err.message || String(err), true);
      }
    };
    input.onkeydown = (e) => { if (e.key === "Enter") { e.preventDefault(); save.click(); } };
    actions.appendChild(save);
    modal.appendChild(actions);
    setTimeout(() => input.focus(), 0);
  });
}

/* ---------- start / view a card ---------- */

// Start the agent on a card: create a fresh session, link the card to it
// (and move it to In Progress) BEFORE prompting so the agent already knows
// its card from the first step, then prompt it with the card's task. The
// task title goes in as the plain user message — all card handling guidance
// (how to work the card, moving it to done) is injected server-side into the
// system prompt, keeping the message the user sees clean.
async function startCard(c) {
  try {
    const body = { agent: S.agent, title: c.title };
    if (S.model) body.model = { id: S.model.modelID, providerID: S.model.providerID };
    const sess = await post("/session", body);
    await patch("/board/card/" + c.id, { column: "inprogress", sessionID: sess.id });
    // the card's task goes in as the plain user message, its attachments
    // (if any) ride along as file parts — same shape the composer sends
    const parts = [{ type: "text", text: c.title }];
    if (c.attachments && c.attachments.length) parts.push(...c.attachments);
    await post(`/session/${sess.id}/prompt_async`, {
      agent: S.agent,
      model: S.model ? { providerID: S.model.providerID, modelID: S.model.modelID } : undefined,
      parts,
    });
    await refreshSession(sess.id).catch(() => {});
    // Create a tab for the new session without activating it — like View,
    // but the user stays on the board.
    openTab(sess.id);
    renderTabs();
    await loadBoard();
    renderBoard();
    toast(`Agent started on "${c.title}"`);
  } catch (e) {
    toast(e.message || String(e), true);
  }
}

// Open the session working on a card.
async function viewCard(c) {
  if (!c.sessionID) return;
  await refreshSession(c.sessionID).catch(() => {});
  navigate("/" + c.sessionID);
}

function boardCardsIn(column) {
  return boardCards.filter((c) => c.column === column).sort((a, b) => a.order - b.order);
}

function boardCardById(id) {
  return boardCards.find((c) => c.id === id) || null;
}

// Toggle the busy spinner on kanban cards in place — session.status/idle fire
// per subagent lifecycle, and the full board rebuild this used to trigger was
// the heaviest DOM op in the app (and the reason renderBoard needs its
// focus/contenteditable rescue logic). Cards are keyed by data-id; the
// spinner is the head's first child when present.
function updateBoardSpinners() {
  const board = $("#kanban");
  if (!board) return;
  board.querySelectorAll(".kanban-card[data-id]").forEach((card) => {
    const c = boardCardById(card.dataset.id);
    const want = !!(c && c.column === "inprogress" && c.sessionID && sessionBusy(c.sessionID));
    const head = card.querySelector(".kanban-card-head");
    if (!head) return;
    const has = !!head.querySelector(":scope > span.spinner");
    if (want === has) return;
    if (want) head.insertBefore(el("span", "spinner", ICONS.spinner), head.firstChild);
    else {
      const sp = head.querySelector(":scope > span.spinner");
      if (sp) sp.remove();
    }
  });
}

function renderBoard() {
  const board = $("#kanban");
  if (!board) return;
  // don't rebuild over an active title edit — it would destroy the
  // contenteditable node and lose the unsaved text
  if (board.querySelector('.kanban-card-title[contenteditable="true"]')) return;
  // the orchestrator chat is a board column — keep it across rebuilds
  const chat = $("#board-chat");
  // was the add-card input focused? the rebuild replaces the node, so focus
  // (and the caret) must be restored afterwards
  const prevAdd = board.querySelector(".kanban-add-input");
  const addFocused = !!prevAdd && document.activeElement === prevAdd;
  board.innerHTML = "";
  // collapsed columns live in a vertical rail on the left, stacked below
  // each other; expanded columns fill the rest of the board
  const rail = el("div", "kanban-rail");
  for (const col of BOARD_COLUMNS) {
    if (boardCollapsed[col.id]) rail.appendChild(buildBoardColumn(col));
  }
  if (rail.children.length) board.appendChild(rail);
  for (const col of BOARD_COLUMNS) {
    if (!boardCollapsed[col.id]) board.appendChild(buildBoardColumn(col));
  }
  if (chat) board.appendChild(chat);
  // restore focus into the rebuilt add-card input (the draft text itself is
  // re-applied by buildBoardColumn from kanbanAddDraft)
  if (addFocused) {
    const nextAdd = board.querySelector(".kanban-add-input");
    if (nextAdd) {
      nextAdd.focus();
      const end = nextAdd.value.length;
      nextAdd.setSelectionRange(end, end);
    }
  }
  drawLinkLines(); // repaint the border-to-border lines between linked cards
}

// Shared drop-target logic for expanded columns and collapsed buttons alike:
// insert before the card under the pointer, else at the end. Link drags (the
// ⌖ icon) target cards, not columns, so they are ignored here — the cards
// themselves handle them. `list` is null for collapsed columns (drop = append).
function attachBoardDrop(target, colId, list) {
  target.ondragover = (e) => {
    if (e.dataTransfer.types.includes(LINK_MIME)) return;
    e.preventDefault();
    target.classList.add("drag-over");
  };
  target.ondragleave = (e) => { if (!target.contains(e.relatedTarget)) target.classList.remove("drag-over"); };
  target.ondrop = (e) => {
    e.preventDefault();
    target.classList.remove("drag-over");
    const cid = e.dataTransfer.getData("text/plain");
    if (!cid) return;
    let beforeID = null;
    if (list) {
      for (const n of list.querySelectorAll(".kanban-card:not(.dragging)")) {
        const r = n.getBoundingClientRect();
        if (e.clientY < r.top + r.height / 2) { beforeID = n.dataset.id; break; }
      }
    }
    moveBoardCard(cid, colId, beforeID);
  };
}

// A collapsed column: a vertical button in the left rail. Clicking expands
// the column again; dropping a card on it moves the card there (appended).
function buildCollapsedBoardColumn(col, count) {
  const btn = el("button", "kanban-col-collapsed");
  btn.type = "button";
  btn.dataset.column = col.id;
  btn.title = `Expand ${col.label} — you can also drop cards here`;
  // count first: sideways-lr flows bottom-to-top, so this keeps the pill at
  // the bottom of the button while the title reads upward
  btn.innerHTML = `<span class="kanban-count">${count}</span><span class="kanban-col-title">${esc(col.label)}</span>`;
  btn.onclick = () => setBoardCollapsed(col.id, false);
  attachBoardDrop(btn, col.id, null);
  return btn;
}

function buildBoardColumn(col) {
  const cards = boardCardsIn(col.id);
  if (boardCollapsed[col.id]) return buildCollapsedBoardColumn(col, cards.length);
  const colEl = el("div", "kanban-col");
  colEl.dataset.column = col.id;

  const head = el("div", "kanban-col-head");
  head.innerHTML = `<span class="kanban-col-title">${esc(col.label)}</span><span class="kanban-count">${cards.length}</span>`;
  const toggle = el("button", "kanban-col-toggle", ICONS.chev);
  toggle.type = "button";
  toggle.title = "Collapse column";
  toggle.onclick = (e) => { e.stopPropagation(); setBoardCollapsed(col.id, true); };
  head.appendChild(toggle);
  colEl.appendChild(head);

  const list = el("div", "kanban-cards");
  for (const c of cards) list.appendChild(boardCardNode(c));
  colEl.appendChild(list);

  // add-card input pinned to the bottom of the Open column — cards can only
  // be created in open (work starts by moving a card to In Progress). Enter
  // creates the card, Ctrl+Enter inserts a newline; images/files can be
  // attached or pasted, same as the composer.
  if (col.id === "open") {
    const add = el("div", "kanban-add");
    const input = el("textarea", "kanban-add-input");
    input.rows = 1;
    input.placeholder = "Add card…";
    input.value = kanbanAddDraft; // restore the unsaved draft across rebuilds
    const files = kanbanAddFiles; // shared: attachments survive re-renders too
    const attachRow = el("div", "attach-row");
    const redraw = () => {
      renderAttachChipsInto(attachRow, files, (a) => {
        files.splice(files.indexOf(a), 1);
        redraw();
      });
      autosize(input);
    };
    const addFiles = async (fl) => {
      files.push(...(await filesToAttachments(fl)));
      redraw();
    };
    input.onpaste = (e) => {
      const fl = e.clipboardData && e.clipboardData.files;
      if (!fl || !fl.length) return; // plain text paste: default behaviour
      e.preventDefault();
      addFiles([...fl]);
    };
    input.oninput = () => { kanbanAddDraft = input.value; autosize(input); };
    const attachBtn = el("button", "kanban-add-attach", ICONS.plus);
    attachBtn.title = "Attach files";
    const fileInput = document.createElement("input");
    fileInput.type = "file";
    fileInput.multiple = true;
    fileInput.style.display = "none";
    fileInput.onchange = () => { addFiles(fileInput.files); fileInput.value = ""; };
    attachBtn.onclick = () => fileInput.click();
    const row = el("div", "kanban-add-row");
    row.append(attachBtn, input, fileInput);
    add.append(attachRow, row);
    colEl.appendChild(add);
    const commit = async () => {
      const title = input.value.trim();
      if (!title) return;
      const attachments = [...files];
      try {
        await post("/board/card", { title, column: "open", attachments });
        input.value = "";
        kanbanAddDraft = "";
        files.length = 0;
        redraw();
        await loadBoard();
        renderBoard();
        const next = $(`#kanban .kanban-col[data-column="open"] .kanban-add-input`);
        if (next) next.focus();
      } catch (e) {
        toast(e.message || String(e), true);
      }
    };
    input.onkeydown = (e) => {
      // Enter creates; Ctrl+Enter (and Shift+Enter) inserts a newline
      if (e.key === "Enter" && !e.ctrlKey && !e.metaKey && !e.shiftKey) { e.preventDefault(); commit(); }
    };
    setTimeout(() => autosize(input), 0);
  }

  // drop target: see attachBoardDrop above
  attachBoardDrop(colEl, col.id, list);
  return colEl;
}

// Cards are linked with a custom drag type so a link drag never moves the
// dragged card itself; linkDragSource tracks the drag within the page (the
// payload is unreadable during dragover).
const LINK_MIME = "application/x-board-link";
let linkDragSource = null;

function boardCardNode(c) {
  const card = el("div", "kanban-card");
  card.draggable = true;
  card.dataset.id = c.id;
  const head = el("div", "kanban-card-head");
  // same busy indicator as the session tabs: a spinner while the linked
  // session is actively working on this in-progress card
  if (c.column === "inprogress" && c.sessionID && sessionBusy(c.sessionID)) {
    head.appendChild(el("span", "spinner", ICONS.spinner));
  }
  // short numeric ID so cards are easy to reference (links, chat, commits)
  head.appendChild(el("span", "kanban-card-id", "#" + esc(c.id)));
  const title = el("div", "kanban-card-title", esc(c.title));
  if (c.column === "open") title.title = "Double-click to edit";
  head.appendChild(title);
  // sessions watching this card get pinged when it is marked done
  if (c.watchers && c.watchers.length) {
    const n = c.watchers.length;
    const w = el("span", "kanban-watchers", ICONS.eye + (n > 1 ? n : ""));
    w.title = `Watched by ${n} session${n > 1 ? "s" : ""} — they are notified when the card is marked done`;
    head.appendChild(w);
  }
  card.appendChild(head);

  // scheduled start: clock emoji + expected start time; clicking it reopens
  // the schedule dialog (open cards only — started cards have none left)
  if (c.startAt) {
    const sched = el("div", "kanban-card-schedule", "🕐 <span>" + esc(fmtSchedule(c.startAt)) + "</span>");
    sched.title = `Scheduled start: ${new Date(c.startAt).toLocaleString()} (${relTime(c.startAt)})`;
    if (c.column === "open") {
      sched.classList.add("clickable");
      sched.onclick = (e) => { e.stopPropagation(); openScheduleModal(c); };
    }
    card.appendChild(sched);
  }

  // attachments (usually screenshots) attached at creation — shown as chips,
  // and prompted to the session when the card is started
  if (c.attachments && c.attachments.length) {
    const atts = el("div", "kanban-card-attachments");
    for (const a of c.attachments) {
      const chip = el("div", "attach-chip");
      if (a.mime && a.mime.startsWith("image/")) {
        chip.innerHTML = `<img src="${esc(a.url)}"><span>${esc(a.filename || "image")}</span>`;
      } else {
        chip.innerHTML = ICONS.file + `<span>${esc(a.filename || "file")}</span>`;
      }
      atts.appendChild(chip);
    }
    card.appendChild(atts);
  }

  const actions = el("div", "kanban-card-actions");
  // Only cards waiting in Open offer Start: In Progress cards are already
  // being worked on (View below opens the session working on them), and
  // Done cards are finished — restarting them makes no sense.
  if (c.column === "open") {
    const start = el("button", "kanban-card-btn", ICONS.play + "<span>Start</span>");
    start.title = "Start the agent on this card in a new session";
    start.onclick = async (e) => {
      e.stopPropagation();
      start.disabled = true;
      start.innerHTML = ICONS.spinner + "<span>Starting…</span>";
      await startCard(c);
      renderBoard(); // re-render restores the real buttons
    };
    actions.appendChild(start);
    // schedule a timed start: the server starts the card when the time
    // passes, with no client needing to be connected
    const sched = el("button", "kanban-card-btn", ICONS.clock + "<span>Schedule</span>");
    sched.title = "Set a date and time for this card to start automatically";
    sched.onclick = (e) => { e.stopPropagation(); openScheduleModal(c); };
    actions.appendChild(sched);
  }
  if (c.sessionID) {
    const view = el("button", "kanban-card-btn", ICONS.eye + "<span>View</span>");
    view.title = "Open the session working on this card";
    view.onclick = (e) => { e.stopPropagation(); viewCard(c); };
    actions.appendChild(view);
  }
  // Only Open cards are editable: In Progress and Done cards are locked —
  // no Edit button, and their titles can't be double-click-edited either.
  if (c.column === "open") {
    const edit = el("button", "kanban-card-btn", ICONS.edit + "<span>Edit</span>");
    edit.title = "Edit this card";
    edit.onclick = (e) => { e.stopPropagation(); openEditCardModal(c); };
    actions.appendChild(edit);
  }
  actions.appendChild(linkPill(c));
  card.appendChild(actions);

  const delBtn = el("button", "kanban-card-del", ICONS.close);
  delBtn.title = "Delete card";
  delBtn.onclick = (e) => {
    e.stopPropagation();
    confirmDeleteCard(c);
  };
  card.appendChild(delBtn);

  if (c.column === "open") title.ondblclick = () => {
    title.contentEditable = "true";
    title.focus();
    document.getSelection().selectAllChildren(title);
  };
  const saveTitle = async () => {
    if (title.contentEditable !== "true") return;
    title.contentEditable = "false";
    const v = title.textContent.trim();
    if (!v || v === c.title) { title.textContent = c.title; return; }
    c.title = v;
    try {
      await patch("/board/card/" + c.id, { title: v });
    } catch (err) {
      toast(err.message || String(err), true);
    }
  };
  title.onblur = saveTitle;
  title.onkeydown = (e) => {
    if (e.key === "Enter") { e.preventDefault(); title.blur(); }
    else if (e.key === "Escape") { title.textContent = c.title; title.blur(); }
  };

  card.ondragstart = (e) => {
    e.dataTransfer.setData("text/plain", c.id);
    e.dataTransfer.effectAllowed = "move";
    card.classList.add("dragging");
  };
  card.ondragend = () => card.classList.remove("dragging");

  // link drop target: only cards still waiting in Open can be targeted
  card.ondragover = (e) => {
    if (!e.dataTransfer.types.includes(LINK_MIME)) return;
    if (c.column !== "open" || c.id === linkDragSource) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "link";
    card.classList.add("link-over");
  };
  card.ondragleave = () => card.classList.remove("link-over");
  card.ondrop = async (e) => {
    card.classList.remove("link-over");
    const src = e.dataTransfer.getData(LINK_MIME);
    if (!src || src === c.id || c.column !== "open") return;
    e.preventDefault();
    e.stopPropagation(); // a link drop must not move the card
    try {
      await patch("/board/card/" + src, { link: c.id });
      await loadBoard();
      renderBoard();
      toast(`Card ${src} is now linked to card ${c.id} — it starts when ${src} is done`);
    } catch (err) {
      toast(err.message || String(err), true);
    }
  };
  return card;
}

// The link pill: a small target icon next to the other card buttons. Drag it
// onto a card in Open to link the two — once the source card is done, the
// target starts automatically in a fresh session. While linked, the pill
// reads "Will start Card X"; clicking it removes the link.
function linkPill(c) {
  const pill = el("button", "kanban-card-btn kanban-link");
  pill.draggable = true;
  if (c.link) {
    pill.classList.add("linked");
    pill.innerHTML = ICONS.target + "<span>Will start Card " + esc(c.link) + "</span>";
    pill.title = `When this card is done, card ${c.link} starts automatically. Click to remove the link.`;
    pill.onclick = async (e) => {
      e.stopPropagation();
      try {
        await patch("/board/card/" + c.id, { link: "" });
        await loadBoard();
        renderBoard();
        toast("Link removed");
      } catch (err) {
        toast(err.message || String(err), true);
      }
    };
  } else {
    pill.innerHTML = ICONS.target;
    pill.title = "Drag onto a card in Open to link it — it starts automatically when this card is done";
  }
  pill.ondragstart = (e) => {
    e.stopPropagation(); // drag the link, not the card
    linkDragSource = c.id;
    e.dataTransfer.setData(LINK_MIME, c.id);
    e.dataTransfer.effectAllowed = "link";
    pill.classList.add("dragging-link");
  };
  pill.ondragend = () => {
    linkDragSource = null;
    pill.classList.remove("dragging-link");
  };
  return pill;
}

/* ---------- link lines between linked cards ---------- */

// Linked cards are connected border-to-border with a dashed animated line,
// painted by an SVG overlay above the board. The overlay lives in the board's
// scrollable content space, so lines move with the board's own scroll; column
// scrolls, resizes and re-renders redraw it. The line is dim by default and
// turns green while the source or the target card is hovered.
const SVG_NS = "http://www.w3.org/2000/svg";
let linkLineRO = null; // keeps lines aligned while the board box changes
let linkLineROBoard = null;

// Scroll and resize fire far faster than the lines need repainting; coalesce
// to one draw per frame instead of measuring every card per event.
let linkLineRAF = 0;
function scheduleLinkLines() {
  if (linkLineRAF) return;
  linkLineRAF = requestAnimationFrame(() => {
    linkLineRAF = 0;
    drawLinkLines();
  });
}

function drawLinkLines() {
  const board = $("#kanban");
  if (!board) return;
  if (!linkLineRO) linkLineRO = new ResizeObserver(() => scheduleLinkLines());
  if (linkLineROBoard !== board) {
    if (linkLineROBoard) linkLineRO.unobserve(linkLineROBoard);
    linkLineRO.observe(board);
    linkLineROBoard = board;
  }
  if (!board._linkLinesBound) {
    board._linkLinesBound = true;
    // column scrolls move cards within content space; capture catches the
    // (non-bubbling) scroll events of the column card lists
    board.addEventListener("scroll", () => scheduleLinkLines(), true);
    // hovering the source or the target card lights its line up green
    board.addEventListener("mouseover", (e) => {
      const card = e.target.closest(".kanban-card");
      hotLinkLines(card ? card.dataset.id : null);
    });
    board.addEventListener("mouseleave", () => hotLinkLines(null));
  }
  const sources = boardCards.filter((c) => c.link);
  let svg = board.querySelector("svg.kanban-links");
  if (!sources.length) {
    if (svg) svg.remove();
    return;
  }
  if (!svg) {
    svg = document.createElementNS(SVG_NS, "svg");
    svg.setAttribute("class", "kanban-links");
    board.appendChild(svg);
  }
  // cover the whole scrollable content, so lines reach cards that are
  // scrolled out of view instead of being cut at the viewport edge
  const w = Math.max(board.scrollWidth, board.clientWidth);
  const h = Math.max(board.scrollHeight, board.clientHeight);
  svg.setAttribute("viewBox", `0 0 ${w} ${h}`);
  svg.setAttribute("width", w);
  svg.setAttribute("height", h);
  const brect = board.getBoundingClientRect();
  // card box in board-content coordinates — the SVG overlay's own space
  const box = (node) => {
    const r = node.getBoundingClientRect();
    return {
      left: r.left - brect.left + board.scrollLeft,
      right: r.right - brect.left + board.scrollLeft,
      top: r.top - brect.top + board.scrollTop,
      bottom: r.bottom - brect.top + board.scrollTop,
    };
  };
  svg.replaceChildren();
  for (const c of sources) {
    const sNode = board.querySelector(`.kanban-card[data-id="${CSS.escape(c.id)}"]`);
    const tNode = board.querySelector(`.kanban-card[data-id="${CSS.escape(c.link)}"]`);
    if (!sNode || !tNode) continue; // dangling link (target deleted)
    const s = box(sNode), t = box(tNode);
    const scx = (s.left + s.right) / 2, scy = (s.top + s.bottom) / 2;
    const tcx = (t.left + t.right) / 2, tcy = (t.top + t.bottom) / 2;
    // anchor on the two facing borders: horizontally when the cards sit side
    // by side (different columns), vertically when stacked in one column
    let d;
    if (Math.abs(tcx - scx) >= Math.abs(tcy - scy)) {
      const dir = tcx >= scx ? 1 : -1;
      const sx = dir > 0 ? s.right : s.left, tx = dir > 0 ? t.left : t.right;
      const k = Math.max(38, Math.abs(tx - sx) / 2);
      d = `M ${sx} ${scy} C ${sx + dir * k} ${scy}, ${tx - dir * k} ${tcy}, ${tx} ${tcy}`;
    } else {
      const dir = tcy >= scy ? 1 : -1;
      const sy = dir > 0 ? s.bottom : s.top, ty = dir > 0 ? t.top : t.bottom;
      const k = Math.max(38, Math.abs(ty - sy) / 2);
      d = `M ${scx} ${sy} C ${scx} ${sy + dir * k}, ${tcx} ${ty - dir * k}, ${tcx} ${ty}`;
    }
    const path = document.createElementNS(SVG_NS, "path");
    path.setAttribute("d", d);
    path.setAttribute("class", "kanban-link-line");
    path.dataset.src = c.id;
    path.dataset.dst = c.link;
    svg.appendChild(path);
  }
}

// Lights up the lines touching the hovered card (id), dims all others.
function hotLinkLines(id) {
  const board = $("#kanban");
  if (!board) return;
  for (const p of board.querySelectorAll(".kanban-link-line")) {
    p.classList.toggle("hot", !!id && (p.dataset.src === id || p.dataset.dst === id));
  }
}

// Edit dialog: title and column, pre-filled with the card's current values.
// The title is a multiline textarea (Enter saves, Ctrl+Enter a newline) so
// multiline card titles stay editable.
function openEditCardModal(c) {
  openModal("Edit card", (modal, ov) => {
    const field = el("div", "m-field");
    field.appendChild(el("label", "m-label", "Title"));
    const input = el("textarea", "m-textarea");
    input.rows = 1;
    input.value = c.title;
    field.appendChild(input);
    modal.appendChild(field);

    const colField = el("div", "m-field");
    colField.appendChild(el("label", "m-label", "Column"));
    const sel = el("select", "m-select");
    for (const col of BOARD_COLUMNS) {
      const opt = el("option", "", col.label);
      opt.value = col.id;
      if (col.id === c.column) opt.selected = true;
      sel.appendChild(opt);
    }
    colField.appendChild(sel);
    modal.appendChild(colField);

    const actions = el("div", "m-actions");
    const cancel = el("button", "btn ghost", "Cancel");
    cancel.onclick = () => ov.remove();
    const save = el("button", "btn primary", "Save");
    save.onclick = async () => {
      const title = input.value.trim();
      if (!title) { input.focus(); return; }
      save.disabled = true;
      try {
        const body = { title };
        if (sel.value !== c.column) body.column = sel.value;
        await patch("/board/card/" + c.id, body);
        ov.remove();
        await loadBoard();
        renderBoard();
        toast("Card updated");
      } catch (e) {
        save.disabled = false;
        toast(e.message || String(e), true);
      }
    };
    input.onkeydown = (e) => {
      // Enter saves; Ctrl+Enter (and Shift+Enter) inserts a newline
      if (e.key === "Enter" && !e.ctrlKey && !e.metaKey && !e.shiftKey) { e.preventDefault(); save.click(); }
    };
    input.oninput = () => autosize(input);
    actions.append(cancel, save);
    modal.appendChild(actions);
    setTimeout(() => { autosize(input); input.focus(); input.setSelectionRange(input.value.length, input.value.length); }, 0);
  });
}

// Generic confirmation dialog: a message with Cancel/confirm buttons that
// resolves true only when the confirm button is clicked (backdrop click
// counts as cancel).
function confirmModal(title, message, confirmLabel) {
  return new Promise((resolve) => {
    openModal(title, (modal, ov) => {
      ov.onclick = (e) => { if (e.target === ov) { ov.remove(); resolve(false); } };
      modal.appendChild(el("p", "m-sub", message));
      const actions = el("div", "m-actions");
      const cancel = el("button", "btn ghost", "Cancel");
      cancel.onclick = () => { ov.remove(); resolve(false); };
      const ok = el("button", "btn danger", confirmLabel);
      ok.onclick = () => { ov.remove(); resolve(true); };
      actions.append(cancel, ok);
      modal.appendChild(actions);
    });
  });
}

// Deletion asks for confirmation — a dropped card is gone for good.
async function confirmDeleteCard(c) {
  const ok = await confirmModal(
    "Delete card",
    `Delete card #${esc(c.id)} "${esc(c.title)}"? This cannot be undone.`,
    "Delete"
  );
  if (!ok) return;
  try {
    await del("/board/card/" + c.id);
    await loadBoard();
    renderBoard();
    toast("Card deleted");
  } catch (err) {
    toast(err.message || String(err), true);
  }
}

async function moveBoardCard(id, column, beforeID) {
  const card = boardCards.find((c) => c.id === id);
  if (!card) return;
  const col = boardCardsIn(column).filter((x) => x.id !== id);
  let targetIdx = col.length;
  if (beforeID) {
    const i = col.findIndex((x) => x.id === beforeID);
    if (i >= 0) targetIdx = i;
  }
  if (card.column === column) {
    const curIdx = boardCardsIn(column).findIndex((x) => x.id === id);
    if (curIdx === targetIdx) return; // dropped back where it was
  }
  // Leaving In Progress — to Done, or back to Open — stops the agent working
  // on the card, so ask before pulling the rug out from under it.
  if (card.column === "inprogress" && (column === "done" || column === "open")) {
    const ok = await confirmModal(
      "Stop the agent?",
      `Moving card #${esc(card.id)} "${esc(card.title)}" from In Progress to ${column === "done" ? "Done" : "Open"} will stop the agent. Are you sure you want to stop the current progress?`,
      "Stop & move"
    );
    if (!ok) return;
  }
  // optimistic: reposition locally, then reconcile with the server
  col.splice(targetIdx, 0, card);
  col.forEach((c, i) => { c.column = column; c.order = i; });
  renderBoard();
  try {
    await patch("/board/card/" + id, { column, before: beforeID || "" });
    await loadBoard();
  } catch (e) {
    toast(e.message || String(e), true);
  }
  renderBoard();
}

/* ---------- cron jobs (second fixed tab) ---------- */

// Cron jobs run a prompt on a schedule: every run starts a fresh session
// titled after the job and prompts it with the job's prompt. Managed by the
// backend (internal/cron + a sweep in internal/app), so jobs and their
// state survive browser refreshes — the UI here is a thin list over
// GET/POST/PATCH/DELETE /cron.

let cronJobs = [];

async function loadCron() {
  try {
    cronJobs = (await get("/cron")) || [];
  } catch {
    cronJobs = [];
  }
}

// A job is "running" while any of its recent runs' sessions is busy — the
// hourglass emoji next to the title marks those.
function cronRunning(j) {
  return (j.sessions || []).some((sid) => sessionBusy(sid));
}

// The session the View button targets: the newest run still working, else
// the newest run (the last finished one). Jobs that never ran have no
// target — and no View button.
function cronViewTarget(j) {
  const sids = j.sessions || [];
  for (let i = sids.length - 1; i >= 0; i--) {
    if (sessionBusy(sids[i])) return sids[i];
  }
  return sids[sids.length - 1] || null;
}

async function renderCronView(main) {
  const header = el("header", "chat-header");
  header.appendChild(el("div", "title", "Cron jobs"));
  header.appendChild(el("div", "spacer"));
  header.appendChild(el("span", "board-hint", "Every run starts a fresh session with the prompt · schedules follow the server's timezone"));
  const addBtn = el("button", "btn secondary board-add", ICONS.plus + "<span>New cron job</span>");
  addBtn.title = "Create a new cron job";
  addBtn.onclick = () => openCronModal();
  header.appendChild(addBtn);
  main.appendChild(header);
  const list = el("div", "cron-list");
  list.id = "cron-list";
  main.appendChild(list);
  await loadCron();
  renderCronList();
}

function renderCronList() {
  const list = $("#cron-list");
  if (!list) return;
  list.innerHTML = "";
  if (!cronJobs.length) {
    list.appendChild(el("div", "cron-empty", "No cron jobs yet<br>Create one to run a prompt on a schedule"));
    return;
  }
  for (const j of cronJobs) list.appendChild(cronRow(j));
}

// Toggle the hourglass on cron rows in place — a run starting/ending only
// changes this one glyph, not the row's schedule/meta/prompt content.
function updateCronSpinners() {
  const list = $("#cron-list");
  if (!list || S.view !== "cron") return;
  list.querySelectorAll(".cron-row[data-cron]").forEach((row) => {
    const j = cronJobs.find((x) => x.id === row.dataset.cron);
    const want = !!(j && cronRunning(j));
    const has = !!row.querySelector(".cron-hourglass");
    if (want === has) return;
    const head = row.querySelector(".cron-row-head");
    if (!head) return;
    if (want) {
      const hg = el("span", "cron-hourglass", "⏳");
      hg.title = "A run is in progress";
      head.insertBefore(hg, head.firstChild);
    } else {
      const hg = row.querySelector(".cron-hourglass");
      if (hg) hg.remove();
    }
  });
}

function cronRow(j) {
  const row = el("div", "cron-row" + (j.paused ? " paused" : ""));
  row.dataset.cron = j.id;
  const main = el("div", "cron-row-main");

  const head = el("div", "cron-row-head");
  if (cronRunning(j)) {
    const hg = el("span", "cron-hourglass", "⏳");
    hg.title = "A run is in progress";
    head.appendChild(hg);
  }
  head.appendChild(el("span", "cron-title", esc(j.title || "Untitled")));
  if (j.paused) {
    const p = el("span", "cron-badge", "paused");
    p.title = "Paused — the next run will not fire until resumed";
    head.appendChild(p);
  }
  main.appendChild(head);

  const meta = el("div", "cron-row-meta");
  const spec = el("span", "cron-spec", esc(j.spec));
  spec.title = "Schedule (cron expression, server timezone)";
  meta.appendChild(spec);
  if (j.nextRun && !j.paused) {
    meta.appendChild(el("span", "cron-meta-sep", "·"));
    const n = el("span", "cron-next", "next " + relTime(j.nextRun));
    n.title = "Next run: " + new Date(j.nextRun).toLocaleString();
    meta.appendChild(n);
  }
  if (j.lastRun) {
    meta.appendChild(el("span", "cron-meta-sep", "·"));
    const l = el("span", "cron-last", "last " + relTime(j.lastRun));
    l.title = "Last run: " + new Date(j.lastRun).toLocaleString();
    meta.appendChild(l);
  }
  if (j.runs) {
    meta.appendChild(el("span", "cron-meta-sep", "·"));
    meta.appendChild(el("span", "cron-runs", j.runs + (j.runs === 1 ? " run" : " runs")));
  }
  main.appendChild(meta);

  const prompt = el("div", "cron-row-prompt", esc(j.prompt));
  prompt.title = j.prompt;
  main.appendChild(prompt);
  row.appendChild(main);

  const actions = el("div", "cron-row-actions");
  // Pause holds only the NEXT run: a session already started by an earlier
  // run keeps working. Play re-arms the job without firing it now.
  if (j.paused) {
    const play = el("button", "kanban-card-btn", ICONS.play + "<span>Play</span>");
    play.title = "Resume — arms the job for its next scheduled run (does not run now)";
    play.onclick = async (e) => {
      e.stopPropagation();
      play.disabled = true;
      try {
        const updated = await patch("/cron/" + j.id, { paused: false });
        await loadCron();
        renderCronList();
        toast(`"${j.title || "Untitled"}" resumed — next run ${updated && updated.nextRun ? relTime(updated.nextRun) : "scheduled"}`);
      } catch (err) {
        play.disabled = false;
        toast(err.message || String(err), true);
      }
    };
    actions.appendChild(play);
  } else {
    const pause = el("button", "kanban-card-btn", ICONS.pause + "<span>Pause</span>");
    pause.title = "Pause — the current run (if any) keeps going, only the next run is held";
    pause.onclick = async (e) => {
      e.stopPropagation();
      pause.disabled = true;
      try {
        await patch("/cron/" + j.id, { paused: true });
        await loadCron();
        renderCronList();
        toast(`"${j.title || "Untitled"}" paused`);
      } catch (err) {
        pause.disabled = false;
        toast(err.message || String(err), true);
      }
    };
    actions.appendChild(pause);
  }
  const target = cronViewTarget(j);
  if (target) {
    const view = el("button", "kanban-card-btn", ICONS.eye + "<span>View</span>");
    view.title = "Open the latest active session (or the last finished one)";
    view.onclick = async (e) => {
      e.stopPropagation();
      await refreshSession(target).catch(() => {});
      navigate("/" + target);
    };
    actions.appendChild(view);
  }
  const edit = el("button", "kanban-card-btn", ICONS.edit + "<span>Edit</span>");
  edit.title = "Edit the title, prompt or schedule";
  edit.onclick = (e) => { e.stopPropagation(); openCronModal(j); };
  actions.appendChild(edit);
  const delBtn = el("button", "kanban-card-btn cron-del", ICONS.trash + "<span>Delete</span>");
  delBtn.title = "Delete this cron job";
  delBtn.onclick = async (e) => {
    e.stopPropagation();
    const ok = await confirmModal(
      "Delete cron job",
      `Delete cron job "${esc(j.title || "Untitled")}"? Its past sessions are kept. This cannot be undone.`,
      "Delete"
    );
    if (!ok) return;
    try {
      await del("/cron/" + j.id);
      await loadCron();
      renderCronList();
      toast("Cron job deleted");
    } catch (err) {
      toast(err.message || String(err), true);
    }
  };
  actions.appendChild(delBtn);
  row.appendChild(actions);
  return row;
}

// Create (j undefined) or edit (j given) a cron job. The schedule field
// takes a cron expression, an @macro or "@every <duration>"; presets fill
// it in and a live preview validates it against the server (GET
// /cron/preview) and shows the next fire time.
function openCronModal(j) {
  const editing = !!j;
  openModal(editing ? "Edit cron job" : "New cron job", (modal, ov) => {
    const titleField = el("div", "m-field");
    titleField.appendChild(el("label", "m-label", "Title"));
    const title = el("input");
    title.type = "text";
    title.placeholder = "e.g. Nightly dependency check";
    title.maxLength = 200;
    if (editing) title.value = j.title || "";
    titleField.appendChild(title);
    modal.appendChild(titleField);

    const promptField = el("div", "m-field");
    promptField.appendChild(el("label", "m-label", "Prompt"));
    const prompt = el("textarea", "m-textarea");
    prompt.rows = 3;
    prompt.placeholder = "Sent to a fresh session on every run…";
    if (editing) prompt.value = j.prompt || "";
    promptField.appendChild(prompt);
    modal.appendChild(promptField);

    const specField = el("div", "m-field");
    specField.appendChild(el("label", "m-label", "Schedule"));
    const spec = el("input");
    spec.type = "text";
    spec.spellcheck = false;
    spec.placeholder = "0 9 * * * — cron, @hourly, @every 30m…";
    if (editing) spec.value = j.spec || "";
    specField.appendChild(spec);
    modal.appendChild(specField);

    const presets = el("div", "schedule-presets");
    for (const [label, s] of [
      ["Every hour", "0 * * * *"],
      ["Every 30 min", "*/30 * * * *"],
      ["Daily 09:00", "0 9 * * *"],
      ["Weekdays 09:00", "0 9 * * 1-5"],
      ["Weekly Mon 09:00", "0 9 * * 1"],
    ]) {
      const b = el("button", "kanban-card-btn", "<span>" + label + "</span>");
      b.type = "button";
      b.onclick = () => { spec.value = s; updatePreview(); };
      presets.appendChild(b);
    }
    modal.appendChild(presets);

    const preview = el("div", "schedule-preview");
    modal.appendChild(preview);
    let previewTimer = 0, previewSeq = 0;
    const updatePreview = () => {
      const s = spec.value.trim();
      const my = ++previewSeq;
      if (!s) {
        preview.classList.remove("err");
        preview.textContent = "Pick a schedule";
        return;
      }
      preview.classList.remove("err");
      preview.textContent = "…";
      get("/cron/preview?spec=" + encodeURIComponent(s)).then((r) => {
        if (my !== previewSeq) return;
        preview.textContent = `Next run: ${new Date(r.next).toLocaleString()} (${relTime(r.next)})`;
      }).catch((e) => {
        if (my !== previewSeq) return;
        preview.classList.add("err");
        preview.textContent = e.message || "Invalid schedule";
      });
    };
    spec.oninput = () => { clearTimeout(previewTimer); previewTimer = setTimeout(updatePreview, 250); };

    const actions = el("div", "m-actions");
    const cancel = el("button", "btn ghost", "Cancel");
    cancel.onclick = () => ov.remove();
    const save = el("button", "btn primary", editing ? "Save" : "Create cron job");
    save.onclick = async () => {
      const t = title.value.trim();
      const p = prompt.value.trim();
      const s = spec.value.trim();
      if (!t) { title.focus(); return; }
      if (!p) { prompt.focus(); return; }
      if (!s) { spec.focus(); return; }
      save.disabled = true;
      try {
        if (editing) await patch("/cron/" + j.id, { title: t, prompt: p, spec: s });
        else await post("/cron", { title: t, prompt: p, spec: s });
        ov.remove();
        await loadCron();
        renderCronList();
        toast(editing ? "Cron job updated" : "Cron job created");
      } catch (e) {
        save.disabled = false;
        toast(e.message || String(e), true);
      }
    };
    title.onkeydown = prompt.onkeydown = spec.onkeydown = (e) => {
      if (e.key === "Enter" && e.target !== prompt) { e.preventDefault(); save.click(); }
    };
    actions.append(cancel, save);
    modal.appendChild(actions);
    setTimeout(() => { title.focus(); updatePreview(); }, 0);
  });
}

/* ---------- composer ---------- */

// Composer drafts are per session: every tab keeps its own unsent text (and
// attachments), so switching tabs neither loses nor leaks what was typed.
// The landing view (no session yet) drafts under the "" key.
const composerDrafts = new Map(); // sessionID -> unsent text
const composerFiles = new Map(); // sessionID -> [{type, mime, url, filename}]

function draftKey() {
  return S.current || "";
}

function composerDraft() {
  return composerDrafts.get(draftKey()) || "";
}

function setComposerDraft(text) {
  composerDrafts.set(draftKey(), text);
}

function currentAttachments() {
  let list = composerFiles.get(draftKey());
  if (!list) composerFiles.set(draftKey(), (list = []));
  return list;
}

// Put text back into the chat box (used when reverting a message so the user
// can edit and resend it).
function setComposerText(text) {
  setComposerDraft(text);
  const ta = $("#composer-region textarea");
  if (ta) {
    ta.value = text;
    autosize(ta);
    if (ta.value.startsWith("!")) ta.classList.add("shell-mode");
    else ta.classList.remove("shell-mode");
    renderCmdMenu(ta);
    ta.focus();
    ta.selectionStart = ta.selectionEnd = ta.value.length;
  }
}

// Text of a user message from the local store (non-synthetic text parts).
function userMessageText(store, messageID) {
  const parts = Object.values(store.parts[messageID] || {}).sort((a, b) => (a.id < b.id ? -1 : 1));
  return parts.filter((p) => p.type === "text" && !p.synthetic).map((p) => p.text || "").join("\n");
}

function buildComposerRegion(region) {
  region.innerHTML = "";
  const inner = el("div", "composer-inner");
  region.appendChild(inner);
  renderComposerRegion(region);
}

function renderComposerRegion() {
  const region = $("#composer-region");
  if (!region) return;
  // The rebuild wipes the DOM — carry focus, caret, and an open slash menu
  // across it. Status events, docks, and the mic's periodic dock updates all
  // rebuild; typing must never be interrupted.
  const active = document.activeElement;
  const inRegion = active && active !== document.body && region.contains(active);
  const wasText = !!(inRegion && active.tagName === "TEXTAREA");
  let selS = 0, selE = 0;
  if (wasText) { try { selS = active.selectionStart; selE = active.selectionEnd; } catch {} }
  const activeId = inRegion && active.id ? active.id : "";
  const menuOpen = !!cmdMenu.el;
  region.innerHTML = "";
  buildComposerRegion(region);
  if (!inRegion) return;
  const ta = $("#composer-region textarea");
  if (wasText && ta) {
    ta.focus();
    try { ta.setSelectionRange(selS, selE); } catch {}
    if (menuOpen) renderCmdMenu(ta);
  } else if (activeId) {
    const again = region.querySelector("#" + (window.CSS && CSS.escape ? CSS.escape(activeId) : activeId));
    if (again) again.focus();
  }
}

function renderComposer() {
  const region = $("#composer-region");
  if (!region) return;
  renderComposerRegion();
}

function buildComposerRegion(region) {
  closeCmdMenu();
  const inner = region.firstElementChild || region.appendChild(el("div", "composer-inner"));

  // docks (permission > question > todos)
  const perms = S.permissions.filter((r) => isDockSession(r.sessionID));
  const qs = S.questions.filter((q) => isDockSession(q.sessionID));
  const todos = S.todos[S.current || ""];

  for (const r of perms) inner.appendChild(buildPermissionDock(r));
  for (const q of qs) inner.appendChild(buildQuestionDock(q));
  if (!perms.length && !qs.length && Array.isArray(todos) && todos.length) {
    inner.appendChild(buildTodosDock(todos));
  }
  if (window.IntellexorMic) {
    const micDock = IntellexorMic.dockNode();
    if (micDock) inner.appendChild(micDock);
  }

  const comp = el("div", "composer");
  const ta = el("textarea");
  ta.placeholder = "Ask anything, / for commands, @ for context…";
  ta.rows = 1;
  ta.value = composerDraft();
  ta.oninput = () => {
    setComposerDraft(ta.value);
    autosize(ta);
    if (ta.value.startsWith("!")) ta.classList.add("shell-mode");
    else ta.classList.remove("shell-mode");
    renderCmdMenu(ta);
  };
  ta.onkeydown = (e) => {
    if (cmdMenu.el) {
      if (e.key === "ArrowDown") { e.preventDefault(); moveCmdSel(1); return; }
      if (e.key === "ArrowUp") { e.preventDefault(); moveCmdSel(-1); return; }
      if (e.key === "Enter" || e.key === "Tab") {
        e.preventDefault();
        pickCmd(ta, cmdMenu.items[cmdMenu.sel]);
        return;
      }
      if (e.key === "Escape") { e.preventDefault(); closeCmdMenu(); return; }
    }
    if (e.key === "Tab" && !e.isComposing) {
      // cycle agents: Tab forward, Shift+Tab backward
      e.preventDefault();
      cycleAgent(e.shiftKey ? -1 : 1);
      return;
    }
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      doSend();
    }
    if (e.key === "Escape" && isBusy()) {
      e.preventDefault();
      abort();
    }
  };
  // paste images/files straight into the composer (screenshots, copied files)
  ta.onpaste = (e) => {
    const files = e.clipboardData && e.clipboardData.files;
    if (!files || !files.length) return; // plain text paste: default behaviour
    e.preventDefault();
    onFilesPicked([...files]);
  };
  comp.appendChild(ta);

  const attachRow = el("div", "attach-row");
  attachRow.id = "attach-row";
  renderAttachChips(attachRow);
  ta.after(attachRow);

  const actions = el("div", "c-actions");
  const attachBtn = el("button", "c-btn", ICONS.plus);
  attachBtn.title = "Add files";
  const fileInput = document.createElement("input");
  fileInput.type = "file";
  fileInput.multiple = true;
  fileInput.style.display = "none";
  fileInput.onchange = () => onFilesPicked(fileInput.files);
  attachBtn.onclick = () => fileInput.click();
  actions.appendChild(attachBtn);
  actions.appendChild(fileInput);

  actions.appendChild(buildAgentSelect());
  actions.appendChild(buildModelButton());
  if (window.IntellexorMic) {
    const micBtn = el("button", "c-btn mic-btn" + (IntellexorMic.state() === "recording" ? " recording" : ""), ICONS.mic);
    micBtn.id = "mic-btn";
    micBtn.title = "Dictate (speech to text, runs locally)";
    micBtn.onclick = () => IntellexorMic.toggle();
    actions.appendChild(micBtn);
  }

  const sp = el("span");
  sp.style.flex = "1";
  actions.appendChild(sp);

  const busy = isBusy();
  const send = el("button", "c-btn " + (busy ? "stop" : "send"), busy ? ICONS.stop : ICONS.up);
  send.id = "send-btn";
  send.title = busy ? "Stop" : "Send";
  send.onclick = () => (busy ? abort() : doSend());
  actions.appendChild(send);
  comp.appendChild(actions);

  inner.appendChild(comp);
  const hint = el("div", "hint-row");
  const vcs = S.vcs;
  hint.innerHTML = vcs.branch
    ? `${ICONS.branch} <span style="margin-left:4px">${esc(vcs.branch)}</span>`
    : "No Git";
  inner.appendChild(hint);

  setTimeout(() => { autosize(ta); if (!S.current || isPendingTab(S.current)) ta.focus(); }, 0);
}

function autosize(ta) {
  ta.style.height = "auto";
  ta.style.height = Math.min(240, ta.scrollHeight + 2) + "px";
}

function isBusy() {
  return !!(S.current && S.statuses[S.current]);
}

// Send/stop button follows the busy state without a composer rebuild —
// the rebuild would wipe focus, caret, slash menu and the mic dock.
// The board chat has the same pattern in updateBoardChatChrome.
function updateComposerChrome() {
  const send = $("#send-btn");
  if (!send) return;
  const busy = isBusy();
  send.className = "c-btn " + (busy ? "stop" : "send");
  send.innerHTML = busy ? ICONS.stop : ICONS.up;
  send.title = busy ? "Stop" : "Send";
  send.onclick = () => (busy ? abort() : doSend());
}

async function abort() {
  if (!S.current) return;
  soundSuppressedUntil = Date.now() + 10000; // the user stopped it — no chime
  await post(`/session/${S.current}/abort`).catch(() => {});
  setTimeout(() => {
    if (isBusy()) toast("Abort requested — waiting for the running tool to exit…");
  }, 1500);
}

function isShell() {
  return composerDraft().startsWith("!");
}

async function doSend() {
  const text = composerDraft().trim();
  const isCmd = isShell();
  if (!text && !currentAttachments().length) return;
  setComposerDraft("");
  const parts = [];
  if (text) parts.push({ type: "text", text });
  for (const a of currentAttachments()) parts.push(a);
  composerFiles.delete(draftKey());
  // The composer is no longer rebuilt on session.status (that rebuild wiped
  // focus, caret and the slash menu) — so nothing else syncs the DOM with the
  // now-empty draft. Clear the box and chips in place.
  const box = $("#composer-region textarea");
  if (box) {
    box.value = "";
    autosize(box);
    box.classList.remove("shell-mode");
  }
  const chips = $("#attach-row");
  if (chips) renderAttachChips(chips);
  // sending is explicit — the user message (arriving via SSE) and everything
  // after it must be visible, even if the user was scrolled up
  S.scrollStick = true;

  try {
    let sid = await ensureSessionId();
    await loadMessages(sid).catch(() => {});
    const slash = !isCmd && text.startsWith("/") ? cmdQuery(text) : null;
    const known = slash && (S.commands || []).some((c) => c.name === slash.name);
    if (isCmd) {
      await post(`/session/${sid}/shell`, { agent: S.agent, command: text.slice(1) });
    } else if (known) {
      await post(`/session/${sid}/command`, { command: slash.name, arguments: slash.args, agent: S.agent });
    } else {
      await post(`/session/${sid}/prompt_async`, {
        agent: S.agent,
        model: S.model ? { providerID: S.model.providerID, modelID: S.model.modelID } : undefined,
        parts,
      });
    }
    await refreshSession(sid);
    renderSidebar();
    const ta = $("#composer-region textarea");
    if (ta) { ta.focus(); ta.selectionStart = ta.selectionEnd = 0; }
  } catch (e) {
    toast(e.message || String(e), true);
  }
}

async function refreshSession(sid) {
  try {
    const sess = await get("/session/" + sid);
    const idx = S.sessions.findIndex((s) => s.id === sid);
    if (idx >= 0) S.sessions[idx] = sess;
    else S.sessions.unshift(sess);
    renderSidebar();
  } catch {}
}

// Read picked files into composer-style attachment parts ({type, mime, url,
// filename} with a data URL) — the shape prompt_async expects, and the shape
// stored on board cards so Start can pass them to the session prompt.
async function filesToAttachments(files) {
  const out = [];
  for (const f of files) {
    const dataUrl = await new Promise((res) => {
      const rd = new FileReader();
      rd.onload = () => res(rd.result);
      rd.readAsDataURL(f);
    });
    out.push({ type: "file", mime: f.type || "text/plain", url: dataUrl, filename: f.name });
  }
  return out;
}

async function onFilesPicked(files) {
  const atts = await filesToAttachments(files);
  currentAttachments().push(...atts);
  renderComposerRegion();
  const ta = $("#composer-region textarea");
  if (ta) ta.focus();
}

// Attachment chips for a plain array (the composer's list lives in
// composerFiles and goes through renderAttachChips below; the add-card
// modal and the board's inline input keep local lists). onRemove is called
// with the removed attachment so the owner can update its list.
function renderAttachChipsInto(row, list, onRemove) {
  row.innerHTML = "";
  for (const a of list) {
    const chip = el("div", "attach-chip");
    if (a.mime && a.mime.startsWith("image/")) {
      chip.innerHTML = `<img src="${esc(a.url)}"><span>${esc(a.filename)}</span>`;
    } else {
      chip.innerHTML = ICONS.file + `<span>${esc(a.filename)}</span>`;
    }
    const x = el("button", "", "✕");
    x.onclick = () => onRemove && onRemove(a);
    chip.appendChild(x);
    row.appendChild(chip);
  }
}

function renderAttachChips(row) {
  renderAttachChipsInto(row, currentAttachments(), (a) => {
    composerFiles.set(draftKey(), currentAttachments().filter((t) => t !== a));
    renderComposerRegion();
  });
}

/* ---------- agent / model selectors ---------- */

const AGENT_LIST = ["build", "plan", "orchestrator", "general", "explore"];

function setAgent(a) {
  S.agent = a;
  wsSet("agent", a);
  renderComposerRegion();
}

function cycleAgent(dir) {
  let i = AGENT_LIST.indexOf(S.agent);
  if (i < 0) i = 0;
  setAgent(AGENT_LIST[(i + dir + AGENT_LIST.length) % AGENT_LIST.length]);
}

function buildAgentSelect() {
  const btn = el("button", "c-select");
  const colors = { build: "#034cff", plan: "#a753ae", orchestrator: "#0d9f6f", general: "#2090f5", explore: "#fcb239" };
  btn.title = "Switch agent (Tab in composer)";
  btn.innerHTML =
    `<span class="agent-dot" style="background:${colors[S.agent] || "#999"}"></span>
     <span class="name">${esc(cap(S.agent))}</span>${ICONS.chev}`;
  btn.onclick = (e) => {
    e.stopPropagation();
    const pop = el("div", "popover");
    for (const a of AGENT_LIST) {
      const item = el("button", "p-item" + (S.agent === a ? " on" : ""));
      item.innerHTML = `<span class="agent-dot" style="background:${colors[a]}"></span><span>${cap(a)}</span>`;
      item.onclick = () => {
        closePopovers();
        setAgent(a);
      };
      pop.appendChild(item);
    }
    openPopover(btn, pop);
  };
  return btn;
}

function modelForSession(sess) {
  const mp = (sess && sess.model && sess.model.providerID) || (S.model && S.model.providerID);
  const mid = (sess && sess.model && sess.model.id) || (S.model && S.model.modelID);
  if (!mp || !mid) return null;
  const p = S.providers.all.find((x) => x.id === mp);
  return p && p.models[mid];
}

function buildModelButton() {
  const btn = el("button", "c-select");
  const cur = S.model && S.model.modelID;
  btn.innerHTML = `<span class="name">${esc(cur || "Select model")}</span>${ICONS.chev}`;
  btn.onclick = (e) => {
    e.stopPropagation();
    openModelPicker(btn);
  };
  return btn;
}

function openModelPicker(anchor) {
  const pop = el("div", "popover");
  const search = el("input");
  search.placeholder = "Search models…";
  pop.appendChild(search);
  const list = el("div");
  pop.appendChild(list);
  const render = () => {
    list.innerHTML = "";
    const q = search.value.toLowerCase();
    for (const p of S.providers.all) {
      const models = Object.values(p.models || {}).filter(
        (m) => !q || m.id.toLowerCase().includes(q) || (m.name || "").toLowerCase().includes(q)
      );
      if (!models.length) continue;
      list.appendChild(el("div", "p-group", esc(p.name || p.id)));
      models
        .sort((a, b) => (a.release_date || "") < (b.release_date || "") ? 1 : -1)
        .forEach((m) => {
          const item = el("button", "p-item" + (S.model && S.model.providerID === p.id && S.model.modelID === m.id ? " on" : ""));
          item.innerHTML = `<span>${esc(m.name || m.id)}</span><span class="pm">${esc(p.id)}</span>`;
          item.onclick = () => {
            S.model = { providerID: p.id, modelID: m.id };
            wsSet("model", JSON.stringify(S.model));
            closePopovers();
            renderComposerRegion();
          };
          list.appendChild(item);
        });
    }
    if (!list.children.length) {
      list.appendChild(el("div", "empty-sessions", "No providers connected. Open settings to add an API key."));
    }
  };
  render();
  openPopover(anchor, pop);
  setTimeout(() => search.focus(), 0);
}

/* ---------- docks ---------- */

function buildPermissionDock(r) {
  const dock = el("div", "dock");
  const head = el("div", "d-head");
  head.innerHTML = `<span style="color:#fcd53a">${ICONS.warn}</span><span class="d-title">Permission required</span>`;
  dock.appendChild(head);
  const body = el("div", "d-body");
  const desc = permissionDesc(r.permission);
  body.appendChild(el("div", "", esc(desc)));
  for (const pat of r.patterns || []) {
    body.appendChild(Object.assign(el("code"), { textContent: pat }));
    body.appendChild(document.createElement("br"));
  }
  dock.appendChild(body);
  const actions = el("div", "d-actions");
  const deny = el("button", "btn ghost", "Deny");
  deny.onclick = () => replyPermission(r.id, "reject");
  const always = el("button", "btn secondary", "Allow always");
  always.onclick = () => replyPermission(r.id, "always");
  const once = el("button", "btn primary", "Allow once");
  once.onclick = () => replyPermission(r.id, "once");
  actions.append(deny, always, once);
  dock.appendChild(actions);
  return dock;
}

function permissionDesc(perm) {
  switch (perm) {
    case "bash": return "Run shell commands";
    case "edit": return "Modify files, including edits, writes, and patches";
    case "read": return "Read files";
    case "task": return "Spawn a subagent";
    case "webfetch": return "Fetch a URL";
    case "doom_loop": return "Repeated identical tool calls detected";
    default: return perm;
  }
}

async function replyPermission(id, reply) {
  try {
    await post(`/permission/${id}/reply`, { reply });
  } catch (e) {
    toast(e.message, true);
  }
}

function buildQuestionDock(q) {
  const dock = el("div", "dock");
  // Answers, current index, and the input draft live on the request object:
  // renderComposerRegion() rebuilds this dock from scratch on every bus
  // event, and closure-local state would silently reset (wiping typed
  // answers) each time.
  if (!Array.isArray(q._answers) || q._answers.length !== q.questions.length) {
    q._answers = q.questions.map(() => []);
  }
  const answers = q._answers;
  if (typeof q._qi !== "number" || q._qi < 0 || q._qi >= q.questions.length) q._qi = 0;
  let qi = q._qi;
  let draft = typeof q._draft === "string" ? q._draft : "";
  let inputEl = null;

  const submit = () => {
    post(`/question/${q.id}/reply`, { answers }).catch((e) => toast(e.message, true));
  };

  function render() {
    q._qi = qi;
    const wasFocused = !!(inputEl && document.activeElement === inputEl);
    dock.innerHTML = "";
    const cur = q.questions[qi];
    const head = el("div", "d-head");
    head.innerHTML = `<span class="d-title">${esc(cur.header || "Question")}</span>
      <span class="q-prog" style="margin-left:auto">${qi + 1} of ${q.questions.length}</span>`;
    dock.appendChild(head);
    dock.appendChild(el("div", "d-body", esc(cur.question)));
    const opts = el("div", "q-opts");
    for (const o of cur.options || []) {
      const b = el("button", "q-opt" + (answers[qi].includes(o.label) ? " sel" : ""));
      b.innerHTML = `<div class="ol">${esc(o.label)}</div><div class="od">${esc(o.description || "")}</div>`;
      b.onclick = () => {
        if (cur.multiple) {
          answers[qi] = answers[qi].includes(o.label)
            ? answers[qi].filter((x) => x !== o.label)
            : [...answers[qi], o.label];
        } else {
          answers[qi] = [o.label];
        }
        render();
      };
      opts.appendChild(b);
    }
    dock.appendChild(opts);
    const input = el("input", "q-input");
    inputEl = input;
    input.placeholder = "Type your answer…";
    input.value = draft;
    input.oninput = () => { draft = input.value; q._draft = draft; };
    // Enter commits a custom answer; on the last question it sends the reply
    // right away instead of forcing a second click on Submit.
    const commitCustom = (sendNow) => {
      const v = input.value.trim();
      if (!v) return;
      answers[qi] = [v];
      draft = ""; q._draft = "";
      if (!sendNow && qi < q.questions.length - 1) qi++;
      if (sendNow) submit();
      else render();
    };
    input.onkeydown = (e) => {
      if (e.key !== "Enter") return;
      e.preventDefault();
      commitCustom(qi === q.questions.length - 1);
    };
    dock.appendChild(input);
    if (wasFocused) input.focus();
    const actions = el("div", "d-actions");
    const dismiss = el("button", "btn ghost", "Dismiss");
    dismiss.onclick = async () => {
      await post(`/question/${q.id}/reject`).catch(() => {});
    };
    actions.appendChild(dismiss);
    if (qi > 0) {
      const back = el("button", "btn secondary", "Back");
      back.onclick = () => { qi--; render(); };
      actions.appendChild(back);
    }
    if (qi < q.questions.length - 1) {
      const next = el("button", "btn primary", "Next");
      next.onclick = () => { qi++; render(); };
      actions.appendChild(next);
    } else {
      const submitBtn = el("button", "btn primary", "Submit");
      submitBtn.onclick = () => {
        // an uncommitted draft still counts if nothing was selected
        if (!answers[qi].length && draft.trim()) answers[qi] = [draft.trim()];
        submit();
      };
      actions.appendChild(submitBtn);
    }
    dock.appendChild(actions);
  }
  render();
  return dock;
}

function buildTodosDock(todos) {
  const box = el("div", "todos");
  const done = todos.filter((t) => t.status === "completed").length;
  const head = el("div", "t-head");
  head.innerHTML = `<span>Todos</span>`;
  const count = el("span", "t-count", `${done} of ${todos.length} todos completed`);
  // once the agent is idle (no tab progress), the list can be cleared
  if (S.current && !sessionBusy(S.current)) {
    const clear = el("button", "t-clear", ICONS.trash);
    clear.title = "Clear todos";
    clear.onclick = async () => {
      await del(`/session/${S.current}/todo`).catch((e) => toast(e.message, true));
    };
    count.appendChild(clear);
  }
  head.appendChild(count);
  box.appendChild(head);
  for (const t of todos) {
    const item = el("div", "t-item" + (t.status === "completed" ? " done" : t.status === "in_progress" ? " current" : ""));
    const mark = t.status === "completed" ? "✓" : "";
    item.innerHTML = `<span class="tbox">${mark}</span><span>${esc(t.content)}</span>`;
    box.appendChild(item);
  }
  return box;
}

/* ---------- slash commands ---------- */

async function loadCommands() {
  try {
    S.commands = await get("/command");
  } catch {
    S.commands = [];
  }
  return S.commands;
}

let cmdMenu = { el: null, items: [], sel: 0 };

// UI-level commands the web app handles itself (the rest come from GET /command)
const UI_COMMANDS = [
  { name: "new", description: "create a new session", ui: true },
  { name: "models", description: "switch model", ui: true },
  { name: "undo", description: "revert the last message", ui: true },
  { name: "redo", description: "restore reverted messages", ui: true },
];

function allCommands() {
  const known = new Set((S.commands || []).map((c) => c.name));
  return [...(S.commands || []), ...UI_COMMANDS.filter((c) => !known.has(c.name))];
}

function cmdQuery(text) {
  // "/name args..." -> { name, args } (only when the whole input is a command)
  const m = /^\/([^\s]*)(?:\s([\s\S]*))?$/.exec(text);
  return m ? { name: m[1], args: m[2] || "" } : null;
}

function closeCmdMenu() {
  if (cmdMenu.el) { cmdMenu.el.remove(); cmdMenu.el = null; }
  cmdMenu.items = [];
  cmdMenu.sel = 0;
}

function positionCmdMenu(ta) {
  const pop = cmdMenu.el;
  if (!pop) return;
  const comp = ta.closest(".composer") || ta;
  const r = comp.getBoundingClientRect();
  pop.style.visibility = "hidden";
  const pw = pop.offsetWidth, ph = pop.offsetHeight;
  let left = Math.max(8, Math.min(window.innerWidth - pw - 8, r.left));
  let top = r.top - ph - 6;
  if (top < 8) top = r.bottom + 6;
  pop.style.left = left + "px";
  pop.style.top = top + "px";
  pop.style.visibility = "";
}

function renderCmdMenu(ta) {
  const q = cmdQuery(ta.value);
  if (!q) { closeCmdMenu(); return; }
  const needle = q.name.toLowerCase();
  const items = allCommands().filter((c) => (c.name || "").toLowerCase().includes(needle));
  if (!items.length) { closeCmdMenu(); return; }

  if (!cmdMenu.el) {
    cmdMenu.el = el("div", "popover cmd-menu");
    document.body.appendChild(cmdMenu.el);
    const onDown = (e) => {
      if (cmdMenu.el && !cmdMenu.el.contains(e.target) && e.target !== ta) closeCmdMenu();
    };
    document.addEventListener("pointerdown", onDown);
    cmdMenu.el._onDown = onDown;
  }
  const pop = cmdMenu.el;
  cmdMenu.items = items;
  if (cmdMenu.sel >= items.length) cmdMenu.sel = 0;
  pop.innerHTML = "";
  pop.appendChild(el("div", "p-group", "Commands"));
  items.forEach((c, i) => {
    const item = el("button", "p-item cmd-item" + (i === cmdMenu.sel ? " on" : ""));
    item.innerHTML = `<span class="cmd-name">/${esc(c.name)}</span>` +
      (c.description ? `<span class="cmd-desc">${esc(c.description)}</span>` : "");
    item.onmouseenter = () => { cmdMenu.sel = i; paintCmdSel(); };
    item.onclick = () => pickCmd(ta, c);
    pop.appendChild(item);
  });
  positionCmdMenu(ta);
}

function paintCmdSel() {
  if (!cmdMenu.el) return;
  [...cmdMenu.el.querySelectorAll(".cmd-item")].forEach((n, i) =>
    n.classList.toggle("on", i === cmdMenu.sel));
}

function moveCmdSel(d) {
  if (!cmdMenu.items.length) return;
  cmdMenu.sel = (cmdMenu.sel + d + cmdMenu.items.length) % cmdMenu.items.length;
  paintCmdSel();
  const on = cmdMenu.el.querySelector(".cmd-item.on");
  if (on) on.scrollIntoView({ block: "nearest" });
}

function pickCmd(ta, c) {
  closeCmdMenu();
  if (c.ui) {
    setComposerDraft("");
    ta.value = "";
    autosize(ta);
    runUICommand(c.name);
    return;
  }
  const takesArgs = /\$ARGUMENTS|\$1\b/.test(c.template || "");
  if (takesArgs) {
    setComposerDraft("/" + c.name + " ");
    ta.value = composerDraft();
    ta.focus();
    ta.selectionStart = ta.selectionEnd = ta.value.length;
    autosize(ta);
  } else {
    setComposerDraft("");
    ta.value = "";
    autosize(ta);
    runCommand(c.name, "");
  }
}

async function runCommand(name, args) {
  try {
    const sid = await ensureSessionId();
    await post(`/session/${sid}/command`, { command: name, arguments: args, agent: S.agent });
    await refreshSession(sid);
    renderSidebar();
    const ta = $("#composer-region textarea");
    if (ta) ta.focus();
  } catch (e) {
    toast(e.message || String(e), true);
  }
}

async function runUICommand(name) {
  if (name === "new") { newSessionTab(); return; }
  if (name === "models") {
    const anchor = $("#composer-region .c-select") || $("#composer-region");
    if (anchor) openModelPicker(anchor);
    return;
  }
  if (name === "undo" || name === "redo") {
    if (!S.current || isPendingTab(S.current)) { toast("No session to " + name, true); return; }
    try {
      if (name === "undo") {
        const store = msgStore(S.current);
        let lastUser = null;
        for (const mid of store.order) {
          const info = store.info[mid];
          if (info && info.role === "user") lastUser = mid;
        }
        if (!lastUser) { toast("Nothing to undo", true); return; }
        const text = userMessageText(store, lastUser);
        await post(`/session/${S.current}/revert`, { messageID: lastUser });
        if (text) setComposerText(text);
      } else {
        await post(`/session/${S.current}/unrevert`, {});
      }
      await refreshSession(S.current);
      renderSidebar();
    } catch (e) {
      toast(e.message || String(e), true);
    }
  }
}

/* ---------- popovers ---------- */

let popoverEl = null;
let popoverCleanup = null;
function openPopover(anchor, content) {
  closePopovers();
  const pop = content.classList.contains("popover") ? content : (() => {
    const p = el("div", "popover");
    p.appendChild(content);
    return p;
  })();
  document.body.appendChild(pop);
  const r = anchor.getBoundingClientRect();
  const pw = pop.offsetWidth, ph = pop.offsetHeight;
  let left = Math.max(8, Math.min(window.innerWidth - pw - 8, r.left));
  let top = r.top - ph - 6;
  if (top < 8) top = r.bottom + 6;
  pop.style.left = left + "px";
  pop.style.top = top + "px";
  popoverEl = pop;
  setTimeout(() => {
    document.addEventListener("pointerdown", outsideClose);
  }, 0);
}
function outsideClose(e) {
  if (popoverEl && !popoverEl.contains(e.target)) closePopovers();
}
function closePopovers() {
  if (popoverEl) { popoverEl.remove(); popoverEl = null; }
  if (popoverCleanup) { popoverCleanup(); popoverCleanup = null; }
  document.removeEventListener("pointerdown", outsideClose);
}

/* ---------- context menu (right-click) ---------- */

function openContextMenu(x, y, items) {
  closePopovers();
  const menu = el("div", "popover ctx-menu");
  for (const it of items) {
    const b = el("button", "p-item", (it.icon ? it.icon : "") + "<span>" + esc(it.label) + "</span>");
    b.onclick = () => { closePopovers(); it.action(); };
    menu.appendChild(b);
  }
  document.body.appendChild(menu);
  const pw = menu.offsetWidth, ph = menu.offsetHeight;
  menu.style.left = Math.max(8, Math.min(window.innerWidth - pw - 8, x)) + "px";
  menu.style.top = Math.max(8, Math.min(window.innerHeight - ph - 8, y)) + "px";
  popoverEl = menu;
  const onKey = (e) => { if (e.key === "Escape") closePopovers(); };
  popoverCleanup = () => document.removeEventListener("keydown", onKey);
  setTimeout(() => {
    document.addEventListener("pointerdown", outsideClose);
    document.addEventListener("keydown", onKey);
  }, 0);
}

function openRenamePopover(x, y, sess) {
  closePopovers();
  const pop = el("div", "popover rename-pop");
  const input = document.createElement("input");
  input.type = "text";
  input.value = sess.title || "";
  input.placeholder = "Session name";
  input.maxLength = 200;
  pop.appendChild(input);
  document.body.appendChild(pop);
  const pw = pop.offsetWidth, ph = pop.offsetHeight;
  pop.style.left = Math.max(8, Math.min(window.innerWidth - pw - 8, x)) + "px";
  pop.style.top = Math.max(8, Math.min(window.innerHeight - ph - 8, y)) + "px";
  popoverEl = pop;
  let done = false;
  const finish = (save) => {
    if (done) return;
    done = true;
    closePopovers();
    if (!save) return;
    const v = input.value.trim();
    if (!v || v === sess.title) return;
    patch("/session/" + sess.id, { title: v })
      .then(() => {
        const cur = S.sessions.find((s) => s.id === sess.id);
        if (cur) cur.title = v;
        renderSidebar();
        if (sess.id === S.current) renderHeader();
      })
      .catch((err) => toast("Rename failed: " + (err && err.message ? err.message : err), true));
  };
  const onOutside = (e) => { if (popoverEl && !popoverEl.contains(e.target)) finish(true); };
  popoverCleanup = () => document.removeEventListener("pointerdown", onOutside);
  input.onkeydown = (e) => {
    if (e.key === "Enter") { e.preventDefault(); finish(true); }
    else if (e.key === "Escape") finish(false);
  };
  input.onblur = () => finish(true);
  setTimeout(() => document.addEventListener("pointerdown", onOutside), 0);
  input.focus();
  input.select();
}

/* ---------- settings dialog ---------- */

// Two-pane dialog: a side nav (General / Providers / Agents) plus a page
// area. The Models page is reached from a provider row, not from the nav.
// Provider/agent data comes from the config-file API (GET /config/files,
// GET /config/entries) and is written back into a user-chosen config file.
const SETTINGS_PAGES = [
  { id: "general", label: "General" },
  { id: "providers", label: "Providers" },
  { id: "agents", label: "Agents" },
];

// Tool groups offered in the new-agent form; the keys map to opencode's
// agent.tools config map (true = the agent has access to the tool).
const SETTINGS_TOOL_GROUPS = [
  { label: "Build tools", tools: ["bash", "read", "edit", "write", "glob", "grep"] },
  { label: "Kanban tools", tools: ["board"] },
  { label: "Web fetch", tools: ["webfetch"] },
  { label: "Subagents", tools: ["task"] },
  { label: "Todo", tools: ["todowrite"] },
];

function openSettings(page) {
  const ov = el("div", "overlay");
  const modal = el("div", "modal settings-modal");
  ov.onclick = (e) => { if (e.target === ov) ov.remove(); };

  const side = el("div", "set-side");
  side.appendChild(el("div", "set-title", "Settings"));
  const content = el("div", "set-content");
  modal.append(side, content);

  const show = (id, arg) => {
    for (const b of side.querySelectorAll(".set-nav-item")) {
      b.classList.toggle("on", b.dataset.page === id);
    }
    content.innerHTML = "";
    if (id === "providers") renderSettingsProviders(content, show);
    else if (id === "agents") renderSettingsAgents(content, show);
    else if (id === "models") renderSettingsModels(content, show, arg);
    else renderSettingsGeneral(content);
  };

  for (const p of SETTINGS_PAGES) {
    const b = el("button", "set-nav-item", p.label);
    b.dataset.page = p.id;
    b.onclick = () => show(p.id);
    side.appendChild(b);
  }

  ov.appendChild(modal);
  $("#modal-root").appendChild(ov);
  show(typeof page === "string" ? page : "general");
}

function renderSettingsGeneral(content) {
  content.appendChild(el("h3", "set-h", "Appearance"));
  const row = el("div", "m-row");
  row.appendChild(el("div", "", '<div class="m-label">Color scheme</div><div class="m-sub">System · Light · Dark</div>'));
  const seg = el("div", "seg");
  for (const opt of ["system", "light", "dark"]) {
    const b = el("button", S.scheme === opt ? "on" : "", cap(opt));
    b.onclick = () => {
      S.scheme = opt;
      localStorage.setItem("intellexor.scheme", opt);
      applyScheme();
      for (const x of seg.querySelectorAll("button")) x.classList.toggle("on", x === b);
    };
    seg.appendChild(b);
  }
  row.appendChild(seg);
  content.appendChild(row);

  const bgRow = el("div", "m-row");
  bgRow.appendChild(el("div", "", '<div class="m-label">Animated background</div><div class="m-sub">Aurora canvas behind the app</div>'));
  const bgSeg = el("div", "seg");
  for (const opt of [true, false]) {
    const label = opt ? "On" : "Off";
    const b = el("button", S.bg === opt ? "on" : "", label);
    b.onclick = () => {
      S.bg = opt;
      localStorage.setItem("intellexor.bg", opt ? "1" : "0");
      applyBackground();
      for (const x of bgSeg.querySelectorAll("button")) x.classList.toggle("on", x === b);
    };
    bgSeg.appendChild(b);
  }
  bgRow.appendChild(bgSeg);
  content.appendChild(bgRow);

  content.appendChild(el("h3", "set-h", "About"));
  const about = el("div", "m-row");
  about.appendChild(el("div", "m-label", "Version"));
  about.appendChild(el("div", "m-sub", esc(S.version || "dev")));
  content.appendChild(about);
}

async function fetchConfigFiles() {
  const res = await get("/config/files");
  return res.files || [];
}

async function fetchConfigEntries() {
  return await get("/config/entries");
}

// Populates a <select> with the config files the server accepts writes to,
// defaulting to the closest existing project file (else the global one).
function fillConfigFileSelect(sel) {
  return fetchConfigFiles().then((files) => {
    sel.innerHTML = "";
    for (const f of files) {
      const o = el("option");
      o.value = f.path;
      o.textContent = (f.scope === "global" ? "Global — " : "Project — ") + f.path + (f.exists ? "" : "  (new file)");
      sel.appendChild(o);
    }
    const def = files.find((f) => f.scope === "project" && f.exists) || files[0];
    if (def) sel.value = def.path;
  });
}

function setField(label, input, sub) {
  const f = el("div", "m-field");
  if (label) f.appendChild(el("label", "m-label", label));
  f.appendChild(input);
  if (sub) f.appendChild(el("div", "m-sub", sub));
  return f;
}

// opencode carries the provider type in the "npm" SDK package field; for now
// only the OpenAI-compatible SDK is offered in the UI.
const OPENAI_COMPATIBLE_NPM = "@ai-sdk/openai-compatible";

function providerTypeLabel(npm) {
  if (!npm || npm === OPENAI_COMPATIBLE_NPM) return "openai-compatible";
  return npm;
}

function renderSettingsProviders(content, show) {
  const head = el("div", "set-page-head");
  head.appendChild(el("h3", "set-h", "Providers"));
  const addBtn = el("button", "btn primary", "Add provider");
  head.appendChild(addBtn);
  content.appendChild(head);

  const list = el("div", "set-list", '<div class="set-empty">Loading…</div>');
  content.appendChild(list);

  const load = async () => {
    let entries;
    try {
      entries = (await fetchConfigEntries()).providers || {};
    } catch (e) {
      list.innerHTML = "";
      list.appendChild(el("div", "set-empty", "Failed to load config: " + esc(e.message || String(e))));
      return;
    }
    list.innerHTML = "";
    const ids = Object.keys(entries).sort();
    if (!ids.length) {
      list.appendChild(el("div", "set-empty", "No providers configured in config files yet."));
      return;
    }
    for (const id of ids) {
      const e = entries[id];
      const cfg = e.config || {};
      const item = el("div", "set-item");
      const top = el("div", "si-head");
      top.appendChild(el("div", "si-name", esc(cfg.name || id)));
      const nModels = Object.keys(cfg.models || {}).length;
      if (nModels) top.appendChild(el("span", "si-badge", nModels + " model" + (nModels === 1 ? "" : "s")));
      top.appendChild(el("span", "si-badge", esc(providerTypeLabel(cfg.npm))));
      if (cfg.api && cfg.api !== "openai") top.appendChild(el("span", "si-badge", esc(cfg.api)));
      top.appendChild(el("span", "si-spacer"));
      const cfgBtn = el("button", "btn secondary si-btn", "Configure models");
      cfgBtn.onclick = () => show("models", id);
      const editBtn = el("button", "btn secondary si-btn", "Edit");
      editBtn.onclick = () => openProviderForm(load, { id, entry: cfg, file: e.file });
      const rm = el("button", "icon-btn", ICONS.trash);
      rm.title = "Remove provider";
      rm.onclick = async () => {
        if (!(await confirmModal("Remove provider", `Remove "${id}" from ${e.file}?`, "Remove"))) return;
        try {
          await del(`/config/provider/${encodeURIComponent(id)}?file=${encodeURIComponent(e.file)}`);
          toast("Provider removed");
          refreshProviders();
          load();
        } catch (err) {
          toast(err.message || String(err), true);
        }
      };
      top.append(cfgBtn, editBtn, rm);
      item.appendChild(top);
      const file = el("div", "si-file", esc(e.file));
      file.title = e.file;
      item.appendChild(file);
      list.appendChild(item);
    }
  };
  load();
  addBtn.onclick = () => openProviderForm(load);
}

// Add/edit provider. `existing` is {id, entry, file} when editing; the entry
// is the raw config object, so models and opencode-only fields survive the
// round-trip (the PUT replaces the whole entry).
function openProviderForm(onSaved, existing) {
  const editing = !!(existing && existing.id);
  openModal(editing ? "Edit provider" : "Add provider", (modal, ov) => {
    const idIn = el("input");
    idIn.type = "text";
    idIn.placeholder = "Provider ID, e.g. vllm or my-api";
    idIn.value = editing ? existing.id : "";
    if (editing) idIn.disabled = true;
    const nameIn = el("input");
    nameIn.type = "text";
    nameIn.placeholder = "Display name (optional)";
    nameIn.value = editing ? existing.entry.name || "" : "";
    // Provider type: only openai-compatible is supported for now. The value
    // is the opencode npm package so the config stays schema-compatible.
    const typeSel = el("select", "m-select");
    const typeOpt = el("option");
    typeOpt.value = OPENAI_COMPATIBLE_NPM;
    typeOpt.textContent = "openai-compatible";
    typeSel.appendChild(typeOpt);
    typeSel.value = OPENAI_COMPATIBLE_NPM;
    const keyIn = el("input");
    keyIn.type = "text";
    keyIn.placeholder = "API key (optional for local endpoints)";
    keyIn.value = editing ? (existing.entry.options || {}).apiKey || "" : "";
    const urlIn = el("input");
    urlIn.type = "text";
    urlIn.placeholder = "Endpoint URL, e.g. http://localhost:8080/v1";
    urlIn.value = editing ? (existing.entry.options || {}).baseURL || "" : "";
    const fileSel = el("select", "m-select");
    if (editing) {
      // edits go back to the file the provider is defined in
      fileSel.disabled = true;
      const o = el("option");
      o.value = existing.file;
      o.textContent = existing.file;
      fileSel.appendChild(o);
    } else {
      fillConfigFileSelect(fileSel).catch(() => {});
    }

    const err = el("div", "set-form-err");
    const save = el("button", "btn primary", editing ? "Save changes" : "Save provider");
    const cancel = el("button", "btn ghost", "Cancel");
    cancel.onclick = () => ov.remove();
    const commit = async () => {
      const id = editing ? existing.id : idIn.value.trim();
      if (!id) { err.textContent = "Provider ID is required."; idIn.focus(); return; }
      if (!fileSel.value) { err.textContent = "No config file available."; return; }
      const url = urlIn.value.trim();
      if (!url) { err.textContent = "Endpoint URL is required for openai-compatible providers."; urlIn.focus(); return; }
      // start from the stored entry so models and unknown fields survive
      const entry = editing ? JSON.parse(JSON.stringify(existing.entry || {})) : {};
      entry.npm = typeSel.value;
      const name = nameIn.value.trim();
      if (name) entry.name = name; else delete entry.name;
      const options = { ...(entry.options || {}) };
      options.baseURL = url;
      const key = keyIn.value.trim();
      if (key) options.apiKey = key; else delete options.apiKey;
      entry.options = options;
      delete entry.api; // protocol is implied by the provider type
      save.disabled = true;
      try {
        await api("PUT", "/config/provider/" + encodeURIComponent(id), { file: fileSel.value, entry });
        ov.remove();
        toast("Provider " + id + " saved");
        refreshProviders();
        if (onSaved) onSaved();
      } catch (e) {
        save.disabled = false;
        err.textContent = e.message || String(e);
      }
    };
    save.onclick = commit;
    for (const inp of [idIn, nameIn, keyIn, urlIn]) {
      inp.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); commit(); } });
    }
    modal.append(
      setField("ID", idIn, 'The key under "provider" in the config file.'),
      setField("Provider type", typeSel, "Only openai-compatible is supported for now."),
      setField("Name", nameIn),
      setField("Endpoint URL", urlIn, "Stored as options.baseURL."),
      setField("API key", keyIn, "Stored in the config file (options.apiKey)."),
      setField("Config file", fileSel, editing ? undefined : "Which config file to save this provider into."),
      err
    );
    const actions = el("div", "m-actions");
    actions.append(cancel, save);
    modal.appendChild(actions);
    setTimeout(() => (editing ? nameIn : idIn).focus(), 0);
  });
}

// Models page: reached per provider via "Configure models". Lists the
// models configured for the provider (straight from the config files) and
// allows adding, editing and removing them.
function renderSettingsModels(content, show, providerID) {
  const back = el("button", "btn ghost si-back", "← Providers");
  back.onclick = () => show("providers");
  content.appendChild(back);

  const head = el("div", "set-page-head");
  head.appendChild(el("h3", "set-h", "Models" + (providerID ? " — " + esc(cap(providerID)) : "")));
  const addBtn = el("button", "btn primary", "Add model");
  head.appendChild(addBtn);
  content.appendChild(head);

  const list = el("div", "set-list", '<div class="set-empty">Loading…</div>');
  content.appendChild(list);

  let curFile = null; // config file the provider lives in; set by load()

  const load = async () => {
    let data;
    try {
      data = await fetchConfigEntries();
    } catch (e) {
      list.innerHTML = "";
      list.appendChild(el("div", "set-empty", "Failed to load config: " + esc(e.message || String(e))));
      return;
    }
    const e = (data.providers || {})[providerID];
    list.innerHTML = "";
    if (!e) {
      curFile = null;
      addBtn.disabled = true;
      list.appendChild(el("div", "set-empty", "Provider not found — it may have been removed."));
      return;
    }
    curFile = e.file;
    addBtn.disabled = false;
    const models = (e.config || {}).models || {};
    const ids = Object.keys(models).sort();
    if (!ids.length) {
      list.appendChild(el("div", "set-empty", "No models configured for this provider yet."));
      return;
    }
    for (const mid of ids) {
      const m = models[mid] || {};
      const item = el("div", "set-item");
      const top = el("div", "si-head");
      top.appendChild(el("div", "si-name", esc(m.name || mid)));
      top.appendChild(el("span", "si-badge", esc(mid)));
      const caps = [];
      if (m.tool_call) caps.push("tools");
      if (m.reasoning) caps.push("reasoning");
      if (m.attachment) caps.push("attachments");
      if (m.temperature === false) caps.push("no temperature");
      for (const c of caps) top.appendChild(el("span", "si-badge", c));
      top.appendChild(el("span", "si-spacer"));
      const editBtn = el("button", "btn secondary si-btn", "Edit");
      editBtn.onclick = () => openModelForm(providerID, curFile, load, { id: mid, entry: m });
      const rm = el("button", "icon-btn", ICONS.trash);
      rm.title = "Remove model";
      rm.onclick = async () => {
        if (!(await confirmModal("Remove model", `Remove "${mid}" from ${providerID} (${e.file})?`, "Remove"))) return;
        try {
          await del(`/config/provider/${encodeURIComponent(providerID)}/model/${encodeURIComponent(mid)}?file=${encodeURIComponent(e.file)}`);
          toast("Model removed");
          refreshProviders();
          load();
        } catch (err) {
          toast(err.message || String(err), true);
        }
      };
      top.append(editBtn, rm);
      item.appendChild(top);
      const lim = m.limit || {};
      const bits = [];
      if (lim.context) bits.push("context " + lim.context);
      if (lim.output) bits.push("output " + lim.output);
      if (bits.length) item.appendChild(el("div", "si-sub", esc(bits.join(" · "))));
      const fileEl = el("div", "si-file", esc(e.file));
      fileEl.title = e.file;
      item.appendChild(fileEl);
      list.appendChild(item);
    }
  };
  load();
  addBtn.onclick = () => {
    if (!curFile) { toast("Provider not found", true); return; }
    openModelForm(providerID, curFile, load, null);
  };
}

// Add/edit a single model inside a provider entry. `existing` is
// {id, entry} when editing. The entry shape follows opencode's model schema
// (name, tool_call, reasoning, attachment, temperature, limit); unknown
// fields (cost, modalities, …) are preserved on edit.
function openModelForm(providerID, file, onSaved, existing) {
  const editing = !!(existing && existing.id);
  openModal(editing ? "Edit model" : "Add model", (modal, ov) => {
    const idIn = el("input");
    idIn.type = "text";
    idIn.placeholder = "Model ID, e.g. llama-3.3-70b-instruct";
    idIn.value = editing ? existing.id : "";
    if (editing) idIn.disabled = true;
    const nameIn = el("input");
    nameIn.type = "text";
    nameIn.placeholder = "Display name (optional)";
    nameIn.value = editing ? existing.entry.name || "" : "";

    const mkCheck = (label, key, def) => {
      const c = el("input");
      c.type = "checkbox";
      c.checked = editing ? !!existing.entry[key] : def;
      const wrap = el("label", "m-check");
      wrap.appendChild(c);
      wrap.appendChild(el("span", "", label));
      return { input: c, key, wrap };
    };
    const checks = [
      mkCheck("Tool calling", "tool_call", true),
      mkCheck("Reasoning", "reasoning", false),
      mkCheck("Image attachments", "attachment", false),
      mkCheck("Supports temperature", "temperature", true),
    ];
    const checkRow = el("div", "m-row m-checks");
    for (const c of checks) checkRow.appendChild(c.wrap);

    const numIn = (placeholder, val) => {
      const n = el("input");
      n.type = "number";
      n.min = "0";
      n.placeholder = placeholder;
      if (val) n.value = val;
      return n;
    };
    const ctxIn = numIn("e.g. 131072", editing ? (existing.entry.limit || {}).context : null);
    const outIn = numIn("e.g. 8192", editing ? (existing.entry.limit || {}).output : null);

    const err = el("div", "set-form-err");
    const save = el("button", "btn primary", editing ? "Save changes" : "Add model");
    const cancel = el("button", "btn ghost", "Cancel");
    cancel.onclick = () => ov.remove();
    const commit = async () => {
      const mid = editing ? existing.id : idIn.value.trim();
      if (!mid) { err.textContent = "Model ID is required."; idIn.focus(); return; }
      if (!file) { err.textContent = "No config file available."; return; }
      // start from the stored entry so cost/modalities/… survive
      const entry = editing ? JSON.parse(JSON.stringify(existing.entry || {})) : {};
      entry.id = mid;
      const name = nameIn.value.trim();
      if (name) entry.name = name; else delete entry.name;
      for (const c of checks) entry[c.key] = c.input.checked;
      const limit = { ...(entry.limit || {}) };
      const ctx = parseInt(ctxIn.value, 10);
      const out = parseInt(outIn.value, 10);
      if (ctx > 0) limit.context = ctx; else delete limit.context;
      if (out > 0) limit.output = out; else delete limit.output;
      if (Object.keys(limit).length) entry.limit = limit; else delete entry.limit;
      save.disabled = true;
      try {
        await api("PUT", `/config/provider/${encodeURIComponent(providerID)}/model/${encodeURIComponent(mid)}`, { file, entry });
        ov.remove();
        toast("Model " + mid + " saved");
        refreshProviders();
        if (onSaved) onSaved();
      } catch (e) {
        save.disabled = false;
        err.textContent = e.message || String(e);
      }
    };
    save.onclick = commit;
    for (const inp of [idIn, nameIn, ctxIn, outIn]) {
      inp.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); commit(); } });
    }
    modal.append(
      setField("Model ID", idIn, 'The key under "models" in the provider entry.'),
      setField("Name", nameIn),
      checkRow,
      el("div", "m-row", '<div class="m-label">Limits (tokens)</div>'),
      setField("Context limit", ctxIn),
      setField("Output limit", outIn),
      err
    );
    const actions = el("div", "m-actions");
    actions.append(cancel, save);
    modal.appendChild(actions);
    setTimeout(() => (editing ? nameIn : idIn).focus(), 0);
  });
}

function renderSettingsAgents(content, show) {
  const head = el("div", "set-page-head");
  head.appendChild(el("h3", "set-h", "Agents"));
  const addBtn = el("button", "btn primary", "Add agent");
  head.appendChild(addBtn);
  content.appendChild(head);

  const list = el("div", "set-list", '<div class="set-empty">Loading…</div>');
  content.appendChild(list);

  const load = async () => {
    let data;
    try {
      data = await fetchConfigEntries();
    } catch (e) {
      list.innerHTML = "";
      list.appendChild(el("div", "set-empty", "Failed to load config: " + esc(e.message || String(e))));
      return;
    }
    list.innerHTML = "";
    const defModel = data.default_model || "";
    for (const b of data.builtin || []) {
      const item = el("div", "set-item");
      const top = el("div", "si-head");
      const dot = el("span", "agent-dot");
      dot.style.background = b.color || "#999";
      top.append(dot, el("div", "si-name", esc(b.name)), el("span", "si-badge", esc(b.mode || "agent")));
      top.appendChild(el("span", "si-spacer"));
      top.appendChild(el("span", "si-model", "model: " + esc(defModel || "auto")));
      item.appendChild(top);
      if (b.description) item.appendChild(el("div", "si-desc", esc(b.description)));
      item.appendChild(el("div", "si-file", "built-in"));
      list.appendChild(item);
    }
    const agents = data.agents || {};
    for (const id of Object.keys(agents).sort()) {
      const e = agents[id];
      const cfg = e.config || {};
      const item = el("div", "set-item");
      const top = el("div", "si-head");
      top.appendChild(el("div", "si-name", esc(id)));
      if (cfg.mode) top.appendChild(el("span", "si-badge", esc(cfg.mode)));
      top.appendChild(el("span", "si-spacer"));
      top.appendChild(el("span", "si-model", "model: " + esc(cfg.model || defModel || "auto")));
      const rm = el("button", "icon-btn", ICONS.trash);
      rm.title = "Remove agent";
      rm.onclick = async () => {
        if (!(await confirmModal("Remove agent", `Remove "${id}" from ${e.file}?`, "Remove"))) return;
        try {
          await del(`/config/agent/${encodeURIComponent(id)}?file=${encodeURIComponent(e.file)}`);
          toast("Agent removed");
          load();
        } catch (err) {
          toast(err.message || String(err), true);
        }
      };
      top.appendChild(rm);
      item.appendChild(top);
      if (cfg.description) item.appendChild(el("div", "si-desc", esc(cfg.description)));
      const tools = cfg.tools && typeof cfg.tools === "object" ? Object.keys(cfg.tools).filter((t) => cfg.tools[t]) : null;
      item.appendChild(el("div", "si-sub", "tools: " + (tools && tools.length ? esc(tools.join(", ")) : "all")));
      const file = el("div", "si-file", esc(e.file));
      file.title = e.file;
      item.appendChild(file);
      list.appendChild(item);
    }
  };
  load();
  addBtn.onclick = () => openAgentForm(load);
}

function openAgentForm(onSaved) {
  openModal("Add agent", (modal, ov) => {
    const nameIn = el("input");
    nameIn.type = "text";
    nameIn.placeholder = "Agent name, e.g. reviewer";
    const descIn = el("input");
    descIn.type = "text";
    descIn.placeholder = "What the agent is for (optional)";
    const modelIn = el("input");
    modelIn.type = "text";
    modelIn.placeholder = "provider/model (optional — uses the default model)";
    const promptTa = el("textarea", "m-textarea");
    promptTa.rows = 4;
    promptTa.placeholder = "System prompt for the agent…";
    const modeSel = el("select", "m-select");
    for (const [v, label] of [["subagent", "Subagent (spawned via task)"], ["primary", "Primary (selectable in the composer)"], ["all", "All"]]) {
      const o = el("option");
      o.value = v;
      o.textContent = label;
      modeSel.appendChild(o);
    }
    const checks = [];
    const toolsBox = el("div", "set-checks");
    for (const g of SETTINGS_TOOL_GROUPS) {
      const lab = el("label", "set-check");
      const cb = el("input");
      cb.type = "checkbox";
      cb.checked = true;
      lab.append(cb, el("span", "", g.label));
      toolsBox.appendChild(lab);
      checks.push({ group: g, cb });
    }
    const fileSel = el("select", "m-select");
    fillConfigFileSelect(fileSel).catch(() => {});

    const err = el("div", "set-form-err");
    const save = el("button", "btn primary", "Save agent");
    const cancel = el("button", "btn ghost", "Cancel");
    cancel.onclick = () => ov.remove();
    const commit = async () => {
      const name = nameIn.value.trim();
      if (!name) { err.textContent = "Agent name is required."; nameIn.focus(); return; }
      if (!fileSel.value) { err.textContent = "No config file available."; return; }
      const entry = { mode: modeSel.value, tools: {} };
      const desc = descIn.value.trim();
      if (desc) entry.description = desc;
      const model = modelIn.value.trim();
      if (model) entry.model = model;
      const prompt = promptTa.value.trim();
      if (prompt) entry.prompt = prompt;
      for (const { group, cb } of checks) {
        for (const t of group.tools) entry.tools[t] = cb.checked;
      }
      save.disabled = true;
      try {
        await api("PUT", "/config/agent/" + encodeURIComponent(name), { file: fileSel.value, entry });
        ov.remove();
        toast("Agent " + name + " saved");
        if (onSaved) onSaved();
      } catch (e) {
        save.disabled = false;
        err.textContent = e.message || String(e);
      }
    };
    save.onclick = commit;
    nameIn.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); commit(); } });
    modal.append(
      setField("Name", nameIn),
      setField("Description", descIn),
      setField("Model", modelIn),
      setField("Mode", modeSel),
      setField("System prompt", promptTa),
      setField("Tools", toolsBox),
      setField("Config file", fileSel, "Which config file to save this agent into."),
      err
    );
    const actions = el("div", "m-actions");
    actions.append(cancel, save);
    modal.appendChild(actions);
    setTimeout(() => nameIn.focus(), 0);
  });
}

async function refreshProviders() {
  try {
    S.providers = await get("/provider");
    if (!S.model) {
      const first = S.providers.all[0];
      if (first) {
        const defId = S.providers.default[first.id];
        const mid = defId && first.models[defId] ? defId : Object.keys(first.models)[0];
        if (mid) {
          S.model = { providerID: first.id, modelID: mid };
        }
      }
    }
  } catch {}
}

/* ---------- routing / boot ---------- */

function sessionInfo(id) {
  return S.sessions.find((s) => s.id === id) || S.subagents.get(id) || null;
}

async function route() {
  const h = location.hash.replace(/^#/, "");
  const id = h.replace(/^\//, "");
  if (id === "board") {
    S.view = "board";
    S.current = null;
  } else if (id === "cron") {
    S.view = "cron";
    S.current = null;
  } else {
    S.view = "sessions";
    S.current = id || null;
  }
  // a subagent session opened from a task part is not in the session list —
  // fetch its info so the read-only view can resolve parentID/title
  if (S.current && !isPendingTab(S.current) &&
      !S.sessions.find((s) => s.id === S.current) && !S.subagents.has(S.current)) {
    try {
      const info = await get("/session/" + S.current);
      if (info && info.id) S.subagents.set(info.id, info);
    } catch (e) {
      // session was deleted externally (e.g. while the page was closed) —
      // don't strand the user on a dead route, fall back to the root view
      if (e && e.payload && e.payload.name === "SessionNotFoundError") {
        navigate("");
        return;
      }
    }
  }
  if (S.current) openTab(S.current); // one tab per opened session, like opencode web
  renderSidebar();
  renderMain();
  updateTab();
  if (S.current && !isPendingTab(S.current)) {
    loadSession(S.current);
  }
}

async function loadSession(sid) {
  try {
    await loadMessages(sid);
    const todos = await get(`/session/${sid}/todo`).catch(() => null);
    if (todos) S.todos[sid] = todos;
    renderTimeline();
    buildHeader(S.sessions.find((s) => s.id === sid));
    S.unseen.delete(sid);
    renderSidebar();
  } catch (e) {
    // session vanished between the list load and this fetch (deleted
    // externally) — go back to the root view instead of a dead composer
    if (e && e.payload && e.payload.name === "SessionNotFoundError") {
      S.tabs = S.tabs.filter((id) => id !== sid);
      saveTabs();
      S.sessions = S.sessions.filter((s) => s.id !== sid);
      S.msgs.delete(sid);
      delete S.statuses[sid];
      renderSidebar();
      if (sid === S.current) navigate("");
      return;
    }
    toast(e.message, true);
  }
}

async function boot() {
  applyScheme();
  applyBackground();
  matchMedia("(prefers-color-scheme: dark)").addEventListener("change", applyScheme);

  try {
    const health = await get("/global/health");
    if (!health.healthy) throw new Error("unhealthy");
    S.version = health.version || "";
  } catch (e) {
    document.body.innerHTML = '<div style="padding:40px;font-family:monospace">Cannot reach Intellexor server. Is it running?</div>';
    return;
  }

  const pathInfo = await get("/path").catch(() => ({}));
  S.dir = pathInfo.directory || "";

  try {
    S.vcs = await get("/vcs");
  } catch {}

  try {
    S.sessions = await get("/session") || [];
    S.sessions = S.sessions.filter((s) => !s.parentID);
    sortSessions();
  } catch (e) {
    toast(e.message, true);
  }
  loadTabs();

  // no deep link: restore this workspace's last open chat (same rule as
  // switching workspaces — a reload lands where you were)
  const restore = restorableSession();
  if (!location.hash && restore) {
    history.replaceState(null, "", location.pathname + location.search + "#/" + restore.id);
  }

  try {
    await loadAlarms();
  } catch {}

  try {
    S.statuses = await get("/session/status") || {};
  } catch {}
  try {
    S.permissions = await get("/permission") || [];
  } catch {}
  try {
    S.questions = await get("/question") || [];
  } catch {}

  await refreshProviders();
  loadCommands();

  if (window.IntellexorMic) {
    IntellexorMic.init({
      onUpdate: () => renderComposerRegion(),
      onText: (text) => {
        const cur = composerDraft();
        setComposerDraft((cur ? cur.trimEnd() + " " : "") + text);
        const ta = $("#composer-region textarea");
        if (ta) {
          ta.value = composerDraft();
          autosize(ta);
          renderCmdMenu(ta);
          ta.focus();
          ta.selectionStart = ta.selectionEnd = ta.value.length;
        }
      },
      toast,
    });
  }

  $("#btn-new-session").onclick = () => newSessionTab();
  $("#rail-new").onclick = () => openWorkspacePicker();
  $("#rail-settings").onclick = () => openSettings();
  $("#rail-sidebar").onclick = () => setSidebar(!S.sidebarOpen);
  $("#rail-board-panel").onclick = () => setBoardPanel(!S.boardPanelOpen);
  $("#board-panel-expand").onclick = () => navigate("/board");
  $("#board-panel-list").onscroll = hideBoardPanelTip; // a scrolled-away row must not keep its tooltip
  initSidebar();
  initBoardPanel();
  if (S.boardPanelOpen) loadBoard(); // seed the panel with cards on boot
  window.addEventListener("resize", () => { applySidebar(); applyBoardPanel(); }); // keep drawer/backdrop correct across the breakpoint
  const name = (S.dir || "Project").split("/").filter(Boolean).pop() || "P";
  $("#rail-avatar").textContent = name[0].toUpperCase();
  $("#sidebar-title").textContent = name;
  $("#rail-project").title = S.dir || "Project";
  TabProgress.setTitle("Intellexor — " + name);

  // the current workspace has been open since load — anything that finished
  // in it from here on is visible in the UI, so clear its dot state
  markWorkspaceVisited(S.dir || DIR);
  loadWorkspaces();
  initDesktopEvents();
  connectGlobalSSE();

  window.addEventListener("hashchange", route);
  connectSSE();
  route();
}

boot();
