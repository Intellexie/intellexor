"use strict";

/* ============================== api ============================== */

const DIR = localStorage.getItem("intellexor.dir") || "";

async function api(method, path, body) {
  const url = new URL(path, location.origin);
  if (DIR) url.searchParams.set("directory", DIR);
  const opts = { method, headers: {} };
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
  model: JSON.parse(localStorage.getItem("intellexor.model") || "null"), // {providerID, modelID}
  agent: localStorage.getItem("intellexor.agent") || "build",
  scheme: localStorage.getItem("intellexor.scheme") || "system",
  unseen: new Set(),
  errored: new Set(),
  scrollStick: true,
  streaming: new Map(), // partID -> messageID (parts with live text deltas)
  toolOpen: new Map(), // tool partID -> user's explicit open/closed choice (survives timeline re-renders)
  tabs: [], // open session tabs, in order (persisted)
  sidebarOpen: true, // resolved from localStorage/viewport by initSidebar()
  commands: null, // cached GET /command result (slash-command menu)
  subagents: new Map(), // sessionID -> info for subagent sessions (fetched on demand, not in the sidebar)
  es: null,
};

function applyScheme() {
  let s = S.scheme;
  if (s === "system") s = matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  document.documentElement.dataset.colorScheme = s;
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

function inlineMd(s) {
  s = esc(s);
  s = s.replace(/`([^`]+)`/g, '<code class="inline">$1</code>');
  s = s.replace(/\*\*([^*]+)\*\*/g, "<b>$1</b>");
  s = s.replace(/(^|[\s(])\*([^*\n]+)\*(?=[\s).,!?:;]|$)/g, "$1<i>$2</i>");
  s = s.replace(/\[([^\]]+)\]\((https?:[^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');
  s = s.replace(/(^|[\s(])(https?:\/\/[^\s<)]+)/g, '$1<a href="$2" target="_blank" rel="noopener">$2</a>');
  return s;
}

function renderMarkdown(src) {
  if (!src) return "";
  const lines = src.split("\n");
  let out = [];
  let inCode = false, codeLang = "", codeBuf = [];
  let listType = null, para = [];
  const flushPara = () => {
    if (para.length) { out.push("<p>" + inlineMd(para.join(" ")) + "</p>"); para = []; }
  };
  const flushList = () => {
    if (listType) { out.push(listType === "ul" ? "</ul>" : "</ol>"); listType = null; }
  };
  for (let raw of lines) {
    const line = raw;
    if (/^```/.test(line.trim())) {
      if (!inCode) {
        flushPara(); flushList();
        inCode = true; codeLang = line.trim().slice(3); codeBuf = [];
      } else {
        out.push(
          `<pre><button class="copy-code" onclick="copyCode(this)">${"copy"}</button><code data-lang="${esc(codeLang)}">${esc(codeBuf.join("\n"))}</code></pre>`
        );
        inCode = false;
      }
      continue;
    }
    if (inCode) { codeBuf.push(raw); continue; }
    const h = line.match(/^(#{1,6})\s+(.*)$/);
    if (h) { flushPara(); flushList(); out.push(`<h${h[1].length}>${inlineMd(h[2])}</h${h[1].length}>`); continue; }
    const ul = line.match(/^\s*[-*+]\s+(.*)$/);
    const ol = line.match(/^\s*\d+[.)]\s+(.*)$/);
    if (ul || ol) {
      flushPara();
      const t = ul ? "ul" : "ol";
      if (listType !== t) { flushList(); out.push(t === "ul" ? "<ul>" : "<ol>"); listType = t; }
      out.push("<li>" + inlineMd((ul || ol)[1]) + "</li>");
      continue;
    }
    if (/^>\s?/.test(line)) {
      flushPara(); flushList();
      out.push("<blockquote>" + inlineMd(line.replace(/^>\s?/, "")) + "</blockquote>");
      continue;
    }
    if (/^(-{3,}|\*{3,}|_{3,})$/.test(line.trim())) {
      flushPara(); flushList(); out.push("<hr>"); continue;
    }
    if (line.trim() === "") { flushPara(); flushList(); continue; }
    para.push(line.trim());
  }
  if (inCode) out.push(`<pre><code>${esc(codeBuf.join("\n"))}</code></pre>`);
  flushPara(); flushList();
  return out.join("\n");
}

window.copyCode = (btn) => {
  const code = btn.parentElement.querySelector("code");
  navigator.clipboard.writeText(code.textContent);
  btn.textContent = "copied";
};

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
      if (idx >= 0) S.sessions[idx] = info; else S.sessions.unshift(info);
      sortSessions();
      renderSidebar();
      if (info.id === S.current) {
        renderHeader();
        if (wasReverted !== !!info.revert) {
          // Reverting puts the reverted message back in the chat box; read it
          // from the local store before the reload hides it.
          if (info.revert && info.revert.messageID) {
            const text = userMessageText(msgStore(info.id), info.revert.messageID);
            if (text) setComposerText(text);
          }
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
        updateTab();
      }).catch(() => {});
      break;
    case "session.status": {
      if (p.status && p.status.type === "idle") {
        // record whether the session was really working before the entry is
        // wiped — session.idle needs it to decide on the completion chime
        if (sessionBusy(p.sessionID)) idleAfterBusy.add(p.sessionID);
        delete S.statuses[p.sessionID];
      }
      else {
        S.statuses[p.sessionID] = p.status;
        S.errored.delete(p.sessionID); // fresh activity clears a stale error flag
      }
      renderSidebar();
      renderComposer();
      // also re-render when a subagent shown in the current timeline changed
      // state — its task row carries a live progress indicator
      if (p.sessionID === S.current || taskChildOfCurrent(p.sessionID)) renderTimeline();
      renderBoard(); // in-progress cards show a spinner while their session is busy
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
      renderSidebar();
      renderComposer();
      if (p.sessionID === S.current || taskChildOfCurrent(p.sessionID)) renderTimeline();
      renderBoard(); // clear the spinner on the card this session was working on
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
      renderSidebar();
      renderBoard();
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
      if (info.role === "assistant" && info.error && p.sessionID === S.current) {
        renderTimeline();
      }
      if (info.role === "user" && p.sessionID !== S.current) {
        S.unseen.add(p.sessionID);
        renderSidebar();
      }
      break;
    }
    case "message.part.updated": {
      const store = msgStore(p.sessionID);
      const part = p.part;
      if (!store.parts[part.messageID]) store.parts[part.messageID] = {};
      store.parts[part.messageID][part.id] = part;
      // text/reasoning part finalized — stop delta-streaming into it
      if ((part.type === "text" || part.type === "reasoning") && part.time && part.time.end) {
        S.streaming.delete(part.id);
      }
      if (p.sessionID === S.current) renderTimeline();
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
      }
      if (p.field === "text" || p.field === "reasoning") {
        part.text = (part.text || "") + (p.delta || "");
        if (p.sessionID === S.current) {
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
      if (p.sessionID === S.current) renderTimeline();
      break;
    }
    case "permission.asked":
      S.permissions = S.permissions.filter((r) => r.id !== p.id);
      S.permissions.push(p);
      renderSidebar();
      if (isDockSession(p.sessionID)) renderComposerRegion();
      break;
    case "permission.replied":
      S.permissions = S.permissions.filter((r) => r.id !== p.requestID);
      renderSidebar();
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
      // another client (or tab) changed the board
      if (S.view === "board") loadBoard().then(renderBoard);
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

// True when sessionID is a subagent referenced by a task tool part in the
// current session's timeline — its row shows the open link and, while the
// subagent is still working, a progress indicator.
function taskChildOfCurrent(sessionID) {
  if (!S.current || !sessionID || sessionID === S.current) return false;
  const store = S.msgs.get(S.current);
  if (!store) return false;
  for (const mid of store.order) {
    const parts = store.parts[mid];
    if (!parts) continue;
    for (const pid of Object.keys(parts)) {
      const part = parts[pid];
      if (part && part.type === "tool" && part.tool === "task" &&
          part.state && part.state.metadata && part.state.metadata.sessionId === sessionID) return true;
    }
  }
  return false;
}

function msgStore(sid) {
  if (!S.msgs.has(sid)) {
    S.msgs.set(sid, { order: [], info: {}, parts: {} });
  }
  return S.msgs.get(sid);
}

let deltaRender = 0;
function scheduleDeltaRender() {
  if (deltaRender) return;
  deltaRender = requestAnimationFrame(() => {
    deltaRender = 0;
    renderStreamingText();
  });
}

// Update only the parts with live deltas; falls back to a full timeline
// render when a streaming part has no DOM node yet (bubble not created).
function renderStreamingText() {
  const inner = $("#timeline-inner");
  if (!inner || !S.current) return;
  const store = msgStore(S.current);
  let needFull = false;
  for (const [pid, mid] of S.streaming) {
    const part = store.parts[mid] && store.parts[mid][pid];
    if (!part) { needFull = true; continue; }
    const node = inner.querySelector(`[data-part="${cssId(pid)}"]`);
    if (!node) { needFull = true; continue; }
    node.innerHTML = renderMarkdown(part.text || "");
  }
  if (needFull) {
    renderTimeline();
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

const TABS_KEY = "intellexor.tabs";

function loadTabs() {
  try { S.tabs = JSON.parse(localStorage.getItem(TABS_KEY) || "[]"); } catch { S.tabs = []; }
  if (!Array.isArray(S.tabs)) S.tabs = [];
  // drop tabs whose sessions no longer exist (deleted/archived) or are subagents
  S.tabs = S.tabs.filter((id) => S.sessions.some((s) => s.id === id && !s.parentID));
}

function saveTabs() {
  try { localStorage.setItem(TABS_KEY, JSON.stringify(S.tabs)); } catch {}
}

function openTab(sid) {
  const sess = S.sessions.find((s) => s.id === sid);
  if (!sess || sess.parentID) return;
  if (!S.tabs.includes(sid)) {
    S.tabs.push(sid);
    saveTabs();
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
    if (!s) continue;
    const tab = el("div", "tab" + (sid === S.current ? " active" : ""));
    tab.title = (s.title || "New session") + " — click to switch, right-click to rename, middle-click to close";
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
    tab.appendChild(el("span", "tab-title", esc(s.title || "New session")));
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
    tab.oncontextmenu = (e) => {
      e.preventDefault();
      openContextMenu(e.clientX, e.clientY, [
        { label: "Rename", icon: ICONS.edit, action: () => openRenamePopover(e.clientX, e.clientY, s) },
      ]);
    };
    bar.appendChild(tab);
  }
  const plus = el("button", "tab-new", ICONS.plus);
  plus.title = "New session";
  plus.onclick = () => navigate("");
  bar.appendChild(plus);
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

function setSidebar(open) {
  S.sidebarOpen = open;
  try { localStorage.setItem("intellexor.sidebar", open ? "1" : "0"); } catch {}
  applySidebar();
}

function initSidebar() {
  let saved = null;
  try { saved = localStorage.getItem("intellexor.sidebar"); } catch {}
  // default: open on desktop, closed (drawer) on small screens
  S.sidebarOpen = saved != null ? saved === "1" : !isNarrow();
  applySidebar();
}

function renderSidebar() {
  const list = $("#session-list");
  list.innerHTML = "";
  if (!S.sessions.length) {
    list.appendChild(el("div", "empty-sessions", "Nothing here yet<br>Create a session to get started"));
    return;
  }
  let lastGroup = "";
  for (const s of S.sessions) {
    if (s.parentID) continue;
    const g = groupDay(s.time.updated);
    if (g !== lastGroup) {
      lastGroup = g;
      list.appendChild(el("div", "session-group", esc(g)));
    }
    const row = el("div", "session-row" + (s.id === S.current ? " active" : ""));
    row.onclick = () => navigate("/" + s.id);
    row.oncontextmenu = (e) => {
      e.preventDefault();
      openContextMenu(e.clientX, e.clientY, [
        { label: "Rename", icon: ICONS.edit, action: () => openRenamePopover(e.clientX, e.clientY, s) },
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
    const arch = el("button", "archive", ICONS.archive);
    arch.title = "Archive";
    arch.onclick = (e) => {
      e.stopPropagation();
      del("/session/" + s.id).then(() => toast("Session archived"));
    };
    row.appendChild(arch);
    list.appendChild(row);
  }
  renderTabs();
}

function navigate(path) {
  if (isNarrow() && S.sidebarOpen) setSidebar(false); // dismiss the drawer after picking a session
  location.hash = path === "" ? "" : "#" + path;
}

// Only the last MESSAGE_WINDOW messages are fetched and rendered. Long
// sessions stay smooth (the timeline is fully rebuilt on every render) and
// a page refresh transfers a bounded payload. Older messages are simply not
// shown; SSE events for them are ignored so they can't land out of order.
const MESSAGE_WINDOW = 100;

async function loadMessages(sid) {
  const msgs = await get(`/session/${sid}/message?limit=${MESSAGE_WINDOW}`);
  const store = msgStore(sid);
  store.order = [];
  store.info = {};
  store.parts = {};
  for (const m of msgs) {
    store.order.push(m.info.id);
    store.info[m.info.id] = m.info;
    store.parts[m.info.id] = {};
    for (const part of m.parts || []) {
      store.parts[m.info.id][part.id] = part;
    }
  }
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
  main.appendChild(buildTabBar());
  renderTabs();
  // the animated background is global: full strength on the landing (new
  // session) view, dimmed behind the panels everywhere else (see style.css)
  const landing = S.view === "sessions" && !S.current;
  document.body.classList.toggle("bg-live", landing);
  document.body.classList.toggle("bg-dim", !landing);
  if (window.IntellexorBG) IntellexorBG.setActive(true);
  if (S.view === "board") {
    renderBoardView(main);
    return;
  }
  if (!S.current) {
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
  }
  header.appendChild(el("div", "spacer"));
  header.appendChild(buildContextCircle(sess));
  return header;
}

// rebuild the header of the current session in place
function renderHeader() {
  const sess = S.sessions.find((s) => s.id === S.current);
  if (!sess || !$("#chat-header")) return;
  buildHeader(sess);
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
  // thinking indicator
  const st = S.statuses[S.current];
  if (st && (st.type === "busy" || st.type === "retry")) {
    const think = el("div", "assistant-block");
    if (st.type === "retry") {
      think.appendChild(el("div", "retry", `retrying in ${Math.max(0, Math.round(((st.next || 0) - Date.now()) / 1000))}s — attempt #${st.attempt}`));
    } else if (!lastBlockHasText(store)) {
      think.appendChild(el("div", "shimmer", "Thinking"));
    }
    inner.appendChild(think);
  }
  // turn files summary
  if (turnFiles.length) {
    inner.appendChild(renderTurnFiles(turnFiles));
  }
  if (wasStick) tl.scrollTop = tl.scrollHeight;
  else updateJumpButton(tl); // content grew while scrolled up — no scroll event fires, refresh visibility
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

function renderUserMessage(info, parts) {
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
    if (!S.current) return;
    try {
      await post(`/session/${S.current}/revert`, { messageID: info.id });
      await loadMessages(S.current);
      await refreshSession(S.current);
      renderTimeline();
      if (text) setComposerText(text);
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

function renderAssistantMessage(info, parts) {
  const block = el("div", "assistant-block");
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
  fork.onclick = async () => {
    if (!S.current) return;
    try {
      const dup = await post(`/session/${S.current}/fork`, { messageID: info.id });
      await refreshSession(dup.id);
      navigate("/" + dup.id);
      toast("Session forked — you are now in the copy");
    } catch (e) {
      toast(e.message || String(e), true);
    }
  };
  meta.appendChild(fork);
  if (hasVisible) block.appendChild(meta);
  return block;
}

function durationText(a, b) {
  const s = Math.max(0, Math.round((b - a) / 1000));
  if (s < 60) return s + "s";
  return Math.floor(s / 60) + "m " + (s % 60) + "s";
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
    case "reasoning":
      if (!(p.text || "").trim()) return null; {
        const r = el("div", "reasoning");
        r.appendChild(el("div", "rlabel", "Thought"));
        const node = el("div", "assistant-text", renderMarkdown(p.text));
        node.dataset.part = p.id;
        r.appendChild(node);
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

  if (running && !sub) {
    titleRow.classList.add("shimmer");
  }
  const body = el("div", "tool-body");
  titleRow.appendChild(el("span", "tname", esc(title) + (running && !taskLink ? "…" : "")));
  if (sub) titleRow.appendChild(el("span", "tsub", esc(sub)));
  if (taskLink) {
    titleRow.classList.add("task-link");
    // while the subagent session is still ongoing, a spinner at the beginning
    // of the line tells the user it is still working; the row itself stays
    // clickable and opens the session (read-only)
    const childRunning = sessionBusy(taskLink);
    titleRow.title = childRunning
      ? "Subagent still working — click to open its session (read-only)"
      : "Open subagent session (read-only)";
    if (childRunning) {
      titleRow.prepend(el("span", "spinner", ICONS.spinner));
    }
    titleRow.appendChild(el("span", "tsub ro-open", "open ↗"));
    titleRow.appendChild(el("span", "spacer"));
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

let boardCards = [];

async function renderBoardView(main) {
  const header = el("header", "chat-header");
  header.appendChild(el("div", "title", "Board"));
  header.appendChild(el("div", "spacer"));
  header.appendChild(el("span", "board-hint", "Drag cards between columns · double-click a title to edit"));
  const addBtn = el("button", "btn secondary board-add", ICONS.plus + "<span>Add card</span>");
  addBtn.title = "Create a new card";
  addBtn.onclick = () => openAddCardModal();
  header.appendChild(addBtn);
  main.appendChild(header);
  const board = el("div", "kanban");
  board.id = "kanban";
  main.appendChild(board);
  await loadBoard();
  renderBoard();
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

function openAddCardModal() {
  openModal("Add card", (modal, ov) => {
    const field = el("div", "m-field");
    field.appendChild(el("label", "m-label", "Title"));
    const input = el("input");
    input.type = "text";
    input.placeholder = "Describe the task for the agent…";
    input.maxLength = 500;
    field.appendChild(input);
    modal.appendChild(field);

    const colField = el("div", "m-field");
    colField.appendChild(el("label", "m-label", "Column"));
    const sel = el("select", "m-select");
    for (const col of BOARD_COLUMNS) {
      const opt = el("option", "", col.label);
      opt.value = col.id;
      if (col.id === "open") opt.selected = true;
      sel.appendChild(opt);
    }
    colField.appendChild(sel);
    modal.appendChild(colField);

    const actions = el("div", "m-actions");
    const cancel = el("button", "btn ghost", "Cancel");
    cancel.onclick = () => ov.remove();
    const create = el("button", "btn primary", "Create card");
    create.onclick = async () => {
      const title = input.value.trim();
      if (!title) { input.focus(); return; }
      create.disabled = true;
      try {
        await post("/board/card", { title, column: sel.value });
        ov.remove();
        await loadBoard();
        renderBoard();
        toast("Card added");
      } catch (e) {
        create.disabled = false;
        toast(e.message || String(e), true);
      }
    };
    input.onkeydown = (e) => { if (e.key === "Enter") { e.preventDefault(); create.click(); } };
    actions.append(cancel, create);
    modal.appendChild(actions);
    setTimeout(() => input.focus(), 0);
  });
}

/* ---------- start / view a card ---------- */

// Start the agent on a card: create a fresh session, link the card to it
// (and move it to In Progress) BEFORE prompting so the agent already knows
// its card from the first step, then prompt it with the card's task.
async function startCard(c) {
  try {
    const body = { agent: S.agent, title: c.title };
    if (S.model) body.model = { id: S.model.modelID, providerID: S.model.providerID };
    const sess = await post("/session", body);
    await patch("/board/card/" + c.id, { column: "inprogress", sessionID: sess.id });
    const prompt =
      `A task was created on the project kanban board:\n\n` +
      `"${c.title}"\n\n` +
      `The task's card ID is ${c.id} (the card is linked to this session). ` +
      `When you finish working on it, move the card to the done column with the board tool.\n\n` +
      `Start working on this task now. Explore the codebase as needed, implement it end to end, and verify your work (build and tests where applicable). Keep going until the task is complete.`;
    await post(`/session/${sess.id}/prompt_async`, {
      agent: S.agent,
      model: S.model ? { providerID: S.model.providerID, modelID: S.model.modelID } : undefined,
      parts: [{ type: "text", text: prompt }],
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

async function loadBoard() {
  try {
    boardCards = (await get("/board")) || [];
  } catch {
    boardCards = [];
  }
}

function boardCardsIn(column) {
  return boardCards.filter((c) => c.column === column).sort((a, b) => a.order - b.order);
}

function renderBoard() {
  const board = $("#kanban");
  if (!board) return;
  // don't rebuild over an active title edit — it would destroy the
  // contenteditable node and lose the unsaved text
  if (board.querySelector('.kanban-card-title[contenteditable="true"]')) return;
  board.innerHTML = "";
  for (const col of BOARD_COLUMNS) {
    board.appendChild(buildBoardColumn(col));
  }
}

function buildBoardColumn(col) {
  const colEl = el("div", "kanban-col");
  colEl.dataset.column = col.id;
  const cards = boardCardsIn(col.id);

  const head = el("div", "kanban-col-head");
  head.innerHTML = `<span class="kanban-col-title">${esc(col.label)}</span><span class="kanban-count">${cards.length}</span>`;
  colEl.appendChild(head);

  const list = el("div", "kanban-cards");
  for (const c of cards) list.appendChild(boardCardNode(c));
  colEl.appendChild(list);

  // add-card input pinned to the bottom of the column
  const add = el("div", "kanban-add");
  const input = el("input", "kanban-add-input");
  input.placeholder = "Add card…";
  input.maxLength = 500;
  const commit = async () => {
    const title = input.value.trim();
    if (!title) return;
    input.value = "";
    try {
      await post("/board/card", { title, column: col.id });
      await loadBoard();
      renderBoard();
      const next = $(`#kanban .kanban-col[data-column="${col.id}"] .kanban-add-input`);
      if (next) next.focus();
    } catch (e) {
      toast(e.message || String(e), true);
    }
  };
  input.onkeydown = (e) => {
    if (e.key === "Enter") { e.preventDefault(); commit(); }
  };
  add.appendChild(input);
  colEl.appendChild(add);

  // drop target: insert before the card under the pointer, else at the end
  colEl.ondragover = (e) => { e.preventDefault(); colEl.classList.add("drag-over"); };
  colEl.ondragleave = (e) => { if (!colEl.contains(e.relatedTarget)) colEl.classList.remove("drag-over"); };
  colEl.ondrop = (e) => {
    e.preventDefault();
    colEl.classList.remove("drag-over");
    const cid = e.dataTransfer.getData("text/plain");
    if (!cid) return;
    let beforeID = null;
    for (const n of list.querySelectorAll(".kanban-card:not(.dragging)")) {
      const r = n.getBoundingClientRect();
      if (e.clientY < r.top + r.height / 2) { beforeID = n.dataset.id; break; }
    }
    moveBoardCard(cid, col.id, beforeID);
  };
  return colEl;
}

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
  const title = el("div", "kanban-card-title", esc(c.title));
  title.title = "Double-click to edit";
  head.appendChild(title);
  card.appendChild(head);

  const actions = el("div", "kanban-card-actions");
  // Cards in "In Progress" are already started, so they must not offer a
  // Start button (View below opens the session working on them).
  if (c.column !== "inprogress") {
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
  }
  if (c.sessionID) {
    const view = el("button", "kanban-card-btn", ICONS.eye + "<span>View</span>");
    view.title = "Open the session working on this card";
    view.onclick = (e) => { e.stopPropagation(); viewCard(c); };
    actions.appendChild(view);
  }
  if (actions.children.length) card.appendChild(actions);

  const delBtn = el("button", "kanban-card-del", ICONS.close);
  delBtn.title = "Delete card";
  delBtn.onclick = async (e) => {
    e.stopPropagation();
    try {
      await del("/board/card/" + c.id);
      await loadBoard();
      renderBoard();
    } catch (err) {
      toast(err.message || String(err), true);
    }
  };
  card.appendChild(delBtn);

  title.ondblclick = () => {
    title.contentEditable = "true";
    title.focus();
    document.getSelection().selectAllChildren(title);
  };
  const saveTitle = async () => {
    if (title.contentEditable !== "true") return;
    title.contentEditable = "false";
    const v = title.textContent.trim();
    if (!v || v === c.title) { title.textContent = esc(c.title); return; }
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
  return card;
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

  setTimeout(() => { autosize(ta); if (!S.current) ta.focus(); }, 0);
}

function autosize(ta) {
  ta.style.height = "auto";
  ta.style.height = Math.min(240, ta.scrollHeight + 2) + "px";
}

function isBusy() {
  return !!(S.current && S.statuses[S.current]);
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

  try {
    let sid = S.current;
    if (!sid) {
      const body = { agent: S.agent };
      if (S.model) body.model = { id: S.model.modelID, providerID: S.model.providerID };
      const sess = await post("/session", body);
      sid = sess.id;
      navigate("/" + sid);
    }
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

async function onFilesPicked(files) {
  for (const f of files) {
    const dataUrl = await new Promise((res) => {
      const rd = new FileReader();
      rd.onload = () => res(rd.result);
      rd.readAsDataURL(f);
    });
    currentAttachments().push({ type: "file", mime: f.type || "text/plain", url: dataUrl, filename: f.name });
  }
  renderComposerRegion();
  const ta = $("#composer-region textarea");
  if (ta) ta.focus();
}

function renderAttachChips(row) {
  for (const a of currentAttachments()) {
    const chip = el("div", "attach-chip");
    if (a.mime && a.mime.startsWith("image/")) {
      chip.innerHTML = `<img src="${esc(a.url)}"><span>${esc(a.filename)}</span>`;
    } else {
      chip.innerHTML = ICONS.file + `<span>${esc(a.filename)}</span>`;
    }
    const x = el("button", "", "✕");
    x.onclick = () => {
      composerFiles.set(draftKey(), currentAttachments().filter((t) => t !== a));
      renderComposerRegion();
    };
    chip.appendChild(x);
    row.appendChild(chip);
  }
}

/* ---------- agent / model selectors ---------- */

function buildAgentSelect() {
  const btn = el("button", "c-select");
  const colors = { build: "#034cff", plan: "#a753ae", general: "#2090f5", explore: "#fcb239" };
  btn.innerHTML =
    `<span class="agent-dot" style="background:${colors[S.agent] || "#999"}"></span>
     <span class="name">${esc(cap(S.agent))}</span>${ICONS.chev}`;
  btn.onclick = (e) => {
    e.stopPropagation();
    const pop = el("div", "popover");
    for (const a of ["build", "plan", "general", "explore"]) {
      const item = el("button", "p-item" + (S.agent === a ? " on" : ""));
      item.innerHTML = `<span class="agent-dot" style="background:${colors[a]}"></span><span>${cap(a)}</span>`;
      item.onclick = () => {
        S.agent = a;
        localStorage.setItem("intellexor.agent", a);
        closePopovers();
        renderComposerRegion();
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
            localStorage.setItem("intellexor.model", JSON.stringify(S.model));
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
  let qi = 0;
  const answers = q.questions.map(() => []);

  function render() {
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
    input.placeholder = "Type your answer…";
    input.onkeydown = (e) => {
      if (e.key === "Enter" && input.value.trim()) {
        answers[qi] = [input.value.trim()];
        render();
      }
    };
    dock.appendChild(input);
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
      const submit = el("button", "btn primary", "Submit");
      submit.onclick = async () => {
        await post(`/question/${q.id}/reply`, { answers }).catch((e) => toast(e.message, true));
      };
      actions.appendChild(submit);
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
    let sid = S.current;
    if (!sid) {
      const body = { agent: S.agent };
      if (S.model) body.model = { id: S.model.modelID, providerID: S.model.providerID };
      const sess = await post("/session", body);
      sid = sess.id;
      navigate("/" + sid);
    }
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
  if (name === "new") { navigate(""); return; }
  if (name === "models") {
    const anchor = $("#composer-region .c-select") || $("#composer-region");
    if (anchor) openModelPicker(anchor);
    return;
  }
  if (name === "undo" || name === "redo") {
    if (!S.current) { toast("No session to " + name, true); return; }
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

/* ---------- settings modal ---------- */

function openSettings() {
  const ov = el("div", "overlay");
  const modal = el("div", "modal");
  ov.onclick = (e) => { if (e.target === ov) ov.remove(); };

  modal.appendChild(el("h2", "", "Settings" + (S.version ? ` <span class="m-sub">v${esc(S.version)}</span>` : "")));

  // color scheme
  const row1 = el("div", "m-row");
  row1.appendChild(el("div", "", '<div class="m-label">Color scheme</div><div class="m-sub">System · Light · Dark</div>'));
  const seg = el("div", "seg");
  for (const opt of ["system", "light", "dark"]) {
    const b = el("button", S.scheme === opt ? "on" : "", cap(opt));
    b.onclick = () => {
      S.scheme = opt;
      localStorage.setItem("intellexor.scheme", opt);
      applyScheme();
      openSettings();
      ov.remove();
    };
    seg.appendChild(b);
  }
  row1.appendChild(seg);
  modal.appendChild(row1);

  modal.appendChild(el("h2", "", "Providers"));
  const provBox = el("div");
  const ids = [...new Set(["anthropic", "openai", "google", "openrouter", "deepseek", "xai", "groq", "mistral", "ollama"])];
  for (const id of ids) {
    const row = el("div", "m-row");
    row.appendChild(el("div", "", `<div class="m-label">${esc(cap(id))}</div>`));
    const inp = el("input");
    inp.type = "text";
    inp.placeholder = "API key";
    const conn = S.providers.connected.includes(id);
    const btn = el("button", "btn secondary", "Save");
    btn.style.marginLeft = "8px";
    btn.onclick = async () => {
      const v = inp.value.trim();
      if (!v) {
        await del("/auth/" + id).catch(() => {});
        toast("Removed key for " + id);
      } else {
        await api("PUT", "/auth/" + id, { type: "api", key: v });
        toast("Saved key for " + id);
      }
      inp.value = "";
      await refreshProviders();
      ov.remove();
    };
    inp.addEventListener("keydown", (e) => { if (e.key === "Enter") btn.click(); });
    const wrap = el("div", "", "");
    wrap.style.display = "flex";
    wrap.append(inp, btn);
    if (conn) {
      const ok = el("span", "", "✓ ");
      ok.style.color = "var(--ok-fg)";
      row.appendChild(ok);
    }
    row.appendChild(wrap);
    provBox.appendChild(row);
  }
  modal.appendChild(provBox);

  const close = el("div", "m-row");
  const cb = el("button", "btn primary", "Done");
  cb.onclick = () => ov.remove();
  close.appendChild(cb);
  modal.appendChild(close);

  ov.appendChild(modal);
  $("#modal-root").appendChild(ov);
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
  } else {
    S.view = "sessions";
    S.current = id || null;
  }
  // a subagent session opened from a task part is not in the session list —
  // fetch its info so the read-only view can resolve parentID/title
  if (S.current && !S.sessions.find((s) => s.id === S.current) && !S.subagents.has(S.current)) {
    await get("/session/" + S.current)
      .then((info) => { if (info && info.id) S.subagents.set(info.id, info); })
      .catch(() => {});
  }
  if (S.current) openTab(S.current); // one tab per opened session, like opencode web
  renderSidebar();
  renderMain();
  updateTab();
  if (S.current) {
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
    toast(e.message, true);
  }
}

async function boot() {
  applyScheme();
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

  $("#btn-new-session").onclick = () => navigate("");
  $("#rail-new").onclick = () => navigate("");
  $("#rail-settings").onclick = openSettings;
  $("#rail-sidebar").onclick = () => setSidebar(!S.sidebarOpen);
  initSidebar();
  window.addEventListener("resize", applySidebar); // keep drawer/backdrop correct across the breakpoint
  const name = (S.dir || "Project").split("/").filter(Boolean).pop() || "P";
  $("#rail-avatar").textContent = name[0].toUpperCase();
  $("#sidebar-title").textContent = name;
  TabProgress.setTitle("Intellexor — " + name);

  window.addEventListener("hashchange", route);
  connectSSE();
  route();
}

boot();
