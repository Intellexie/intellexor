"use strict";

/*
 * Browser speech-to-text for Intellexor: transformers.js (onnxruntime-web)
 * in a dedicated module Worker + NVIDIA Parakeet (parakeet-ctc-0.6b, CTC —
 * fp16 on GPU, q8 on CPU). The model is a hard requirement: Parakeet, not
 * Whisper. Everything runs client-side — audio never leaves the browser.
 *
 * Pure record → transcribe: click the mic to record, click again to stop;
 * the whole recording is decoded once and appended to the composer. No
 * real-time / partial transcription.
 *
 * The engine (transformers.min.js + onnxruntime wasm, ~42 MB) ships embedded
 * with the app and is served from /stt/. Inference runs on WebGPU when the
 * browser exposes it, with an automatic fallback to CPU wasm. The model
 * (~1.2 GB) is downloaded once from the Hugging Face hub on first use —
 * with a live progress bar — and cached by transformers.js in Cache Storage
 * ("transformers-cache"); later sessions load instantly from cache.
 */

(function () {
  const CACHE_NAME = "transformers-cache"; // transformers.js browser cache
  const TARGET_RATE = 16000;

  const S = {
    state: "idle", // idle | loading | ready | recording | decoding
    progress: { loaded: 0, total: 0 },
    device: "", // "webgpu" | "wasm" — reported by the worker after load
    recordingSince: 0,
    error: "",
  };

  let worker = null, client = null, loaded = false;
  let mediaStream = null, audioCtx = null, sourceNode = null, procNode = null;
  let chunks = [], totalSamples = 0, tickTimer = null;
  const hooks = { onUpdate: () => {}, onText: () => {}, toast: () => {} };

  const el = (tag, cls, html) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (html !== undefined) e.innerHTML = html;
    return e;
  };
  const fmtMB = (n) => (n / 1048576).toFixed(1) + " MB";
  const upd = () => hooks.onUpdate();

  // the composer rebuilds on every update — never do it more than ~5x/s
  let lastUpd = 0;
  const updThrottled = () => {
    const now = Date.now();
    if (now - lastUpd > 200) { lastUpd = now; upd(); }
  };

  // ---- worker plumbing (mirrors the worker's task protocol) ----

  class TaskClient {
    constructor(w) {
      this.tasks = new Map();
      this.counter = 0;
      w.addEventListener("message", (e) => {
        const msg = e.data || {};
        if (msg.type === "progress") {
          S.progress = { loaded: msg.loaded || 0, total: msg.total || 0 };
          updThrottled();
          return;
        }
        if (msg.type === "fallback") {
          hooks.toast("WebGPU unavailable — speech-to-text falling back to CPU", true);
          return;
        }
        const { id, dir, data, error } = msg;
        if (id === undefined || dir !== "sender") return;
        const t = this.tasks.get(id);
        if (!t) return;
        this.tasks.delete(id);
        error ? t.reject(new Error(error)) : t.resolve(data);
      });
    }
    post(type, payload, transfer) {
      return new Promise((resolve, reject) => {
        const id = ++this.counter;
        this.tasks.set(id, { resolve, reject });
        worker.postMessage({ id, type, dir: "receiver", payload }, transfer || []);
      });
    }
  }

  async function ensureWorker() {
    if (worker) return;
    // A module worker (needed for the ESM imports) — served straight from
    // the app so relative imports resolve against the origin.
    worker = new Worker("/stt-worker.js", { type: "module" });
    client = new TaskClient(worker);
    await new Promise((resolve, reject) => {
      const to = setTimeout(() => {
        worker.removeEventListener("message", onMsg);
        reject(new Error("engine init timeout"));
      }, 60000);
      const onMsg = (e) => {
        if (e.data && e.data.type === "intellexor-ready") {
          clearTimeout(to);
          worker.removeEventListener("message", onMsg);
          resolve();
        }
      };
      worker.addEventListener("message", onMsg);
    });
  }

  async function ensureModel() {
    if (loaded) return true;
    S.state = "loading"; S.error = ""; S.progress = { loaded: 0, total: 0 }; upd();
    try {
      await ensureWorker();
      const res = await client.post("load", {
        device: navigator.gpu ? "webgpu" : "wasm",
      });
      S.device = (res && res.device) || "";
      loaded = true;
      S.state = "ready";
      upd();
      return true;
    } catch (e) {
      S.error = (e && e.message) || String(e);
      S.state = "idle";
      upd();
      hooks.toast("Speech model init failed: " + S.error, true);
      return false;
    }
  }

  function downsample(buffer, fromRate) {
    if (fromRate === TARGET_RATE) return buffer;
    const ratio = fromRate / TARGET_RATE;
    const newLen = Math.round(buffer.length / ratio);
    const out = new Float32Array(newLen);
    let oi = 0, bi = 0;
    while (oi < newLen) {
      const next = Math.round((oi + 1) * ratio);
      let acc = 0, n = 0;
      for (let i = bi; i < next && i < buffer.length; i++) { acc += buffer[i]; n++; }
      out[oi++] = acc / Math.max(n, 1);
      bi = next;
    }
    return out;
  }

  function mergeChunks(list, total) {
    const out = new Float32Array(total);
    let off = 0;
    for (const c of list) { out.set(c, off); off += c.length; }
    return out;
  }

  async function decodeSamples(samples) {
    const res = await client.post("transcribe", { samples }, [samples.buffer]);
    return (res && res.text ? res.text : "").trim();
  }

  async function startRecording() {
    mediaStream = await navigator.mediaDevices.getUserMedia({
      audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true },
    });
    audioCtx = new AudioContext({ sampleRate: TARGET_RATE });
    sourceNode = audioCtx.createMediaStreamSource(mediaStream);
    procNode = audioCtx.createScriptProcessor(4096, 1, 1);
    procNode.onaudioprocess = (e) => {
      if (S.state !== "recording") return;
      const samples = downsample(e.inputBuffer.getChannelData(0), audioCtx.sampleRate);
      chunks.push(new Float32Array(samples));
      totalSamples += samples.length;
    };
    sourceNode.connect(procNode);
    procNode.connect(audioCtx.destination);

    chunks = []; totalSamples = 0;
    S.recordingSince = Date.now();
    S.state = "recording";
    upd();

    // recording-timer display only — no decoding while recording
    tickTimer = setInterval(updThrottled, 1000);
  }

  function stopRecording() {
    clearInterval(tickTimer); tickTimer = null;
    try { procNode && (procNode.onaudioprocess = null); } catch {}
    try { procNode && procNode.disconnect(); } catch {}
    try { sourceNode && sourceNode.disconnect(); } catch {}
    if (mediaStream) for (const t of mediaStream.getTracks()) t.stop();

    S.state = "decoding";
    upd();
    const finish = () => {
      try { audioCtx && audioCtx.close(); } catch {}
      audioCtx = null; procNode = null; sourceNode = null; mediaStream = null;
      chunks = []; totalSamples = 0;
      S.state = "ready";
      upd();
    };
    if (totalSamples > 0) {
      const all = mergeChunks(chunks, totalSamples);
      chunks = [];
      decodeSamples(all)
        .then((text) => { finish(); if (text) hooks.onText(text); })
        .catch((e) => {
          hooks.toast("Transcription failed: " + (e.message || e), true);
          finish();
        });
    } else {
      finish();
    }
  }

  async function toggle() {
    if (S.state === "recording") { stopRecording(); return; }
    if (S.state === "loading" || S.state === "decoding") return;
    if (!window.caches) {
      hooks.toast("Speech-to-text needs a secure context (https:// or localhost)", true);
      return;
    }
    if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
      hooks.toast("Microphone unavailable — requires a secure context (https:// or localhost)", true);
      return;
    }
    if (!loaded) {
      const ok = await ensureModel();
      if (!ok) return;
    }
    try {
      await startRecording();
    } catch (e) {
      S.state = "ready";
      upd();
      hooks.toast("Microphone: " + (e.message || e), true);
    }
  }

  async function clearCache() {
    try {
      await caches.delete(CACHE_NAME);
      S.progress = { loaded: 0, total: 0 };
      hooks.toast("Cached speech model deleted (reload to re-download)");
    } catch (e) {
      hooks.toast("Cache clear failed: " + (e.message || e), true);
    }
  }

  function dockNode() {
    if (S.state === "idle" || S.state === "ready") return null;
    const d = el("div", "dock mic-dock");
    if (S.state === "loading") {
      const { loaded: done, total } = S.progress;
      const pct = total > 0 ? Math.min(100, (done / total) * 100) : 0;
      const head = el("div", "d-head");
      head.innerHTML =
        `<span class="d-title">Downloading speech model <span class="mic-model">parakeet-ctc-0.6b · ${S.device || "webgpu"} · ~1.2 GB</span></span>` +
        `<span class="mic-dock-pct">${pct.toFixed(0)}%</span>`;
      d.appendChild(head);
      const bar = el("div", "mic-progress");
      bar.innerHTML = `<div class="mic-progress-fill" style="width:${pct.toFixed(1)}%"></div>`;
      d.appendChild(bar);
      d.appendChild(
        el("div", "mic-dock-sub",
          total > 0
            ? `${fmtMB(done)} of ${fmtMB(total)} — downloaded once, cached in your browser`
            : "preparing download — cached in your browser after the first run")
      );
      const actions = el("div", "d-actions");
      const cancel = el("button", "btn ghost", "Cancel");
      cancel.onclick = cancelDownload;
      actions.appendChild(cancel);
      d.appendChild(actions);
    } else if (S.state === "recording") {
      const secs = Math.max(0, Math.round((Date.now() - S.recordingSince) / 1000));
      const head = el("div", "d-head");
      head.innerHTML =
        `<span class="rec-dot"></span><span class="d-title">Recording — ${secs}s · click the mic to stop</span>`;
      d.appendChild(head);
    } else if (S.state === "decoding") {
      const head = el("div", "d-head");
      const dev = S.device === "webgpu" ? "On GPU" : "On CPU";
      head.innerHTML = `<span class="d-title">Transcribing…</span>` +
        (S.device ? `<span class="mic-device">${dev}</span>` : "");
      d.appendChild(head);
    }
    return d;
  }

  function cancelDownload() {
    // transformers.js downloads can't be aborted cleanly — kill the worker
    // and start over on the next toggle.
    if (worker) { try { worker.terminate(); } catch {} }
    worker = null; client = null; loaded = false;
    S.state = "idle";
    upd();
  }

  window.IntellexorMic = {
    init(h) { Object.assign(hooks, h); },
    toggle,
    state: () => S.state,
    dockNode,
    clearCache,
  };
})();
