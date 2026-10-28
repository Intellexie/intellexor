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
    decode: null, // {done, total} — chunked decode progress
  };

  let worker = null, client = null, loaded = false, loadPromise = null;
  let mediaStream = null, audioCtx = null, sourceNode = null, procNode = null, workletNode = null, sinkNode = null;
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
        if (msg.type === "decode-progress") {
          S.decode = { done: msg.done || 0, total: msg.total || 0 };
          updThrottled();
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

  // Idempotent: concurrent callers share one in-flight load.
  function ensureModel() {
    if (loaded) return Promise.resolve(true);
    if (!loadPromise) {
      S.state = "loading"; S.error = ""; S.progress = { loaded: 0, total: 0 }; upd();
      loadPromise = (async () => {
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
          loadPromise = null; // allow a retry on the next attempt
          upd();
          hooks.toast("Speech model init failed: " + S.error, true);
          return false;
        }
      })();
    }
    return loadPromise;
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
    // A suspended context silently captures nothing — e.g. when the page
    // was restored from a back-forward cache or the webview throttles us.
    if (audioCtx.state === "suspended") { try { await audioCtx.resume(); } catch {} }
    sourceNode = audioCtx.createMediaStreamSource(mediaStream);
    // Reset the buffer before anything can flow into it, and mark
    // recording BEFORE the graph is wired: the chunk callback drops
    // samples captured outside the recording window.
    chunks = []; totalSamples = 0;
    S.recordingSince = Date.now();
    S.state = "recording";
    const onChunk = (samples) => {
      if (S.state !== "recording") return;
      const s = downsample(samples, audioCtx.sampleRate);
      chunks.push(new Float32Array(s));
      totalSamples += s.length;
    };
    // Every capture path drains through this silent sink into the
    // destination: engines only pull nodes reachable from the destination,
    // and a zero-output capture node is never processed on some of them
    // (WebKitGTK/Safari). Gain 0 keeps it inaudible — no echo.
    sinkNode = audioCtx.createGain();
    sinkNode.gain.value = 0;
    sinkNode.connect(audioCtx.destination);

    // Preferred capture: AudioWorklet (audio render thread — no dropped
    // or repeated buffers). Fallback: deprecated ScriptProcessor (main
    // thread, glitches under load) for browsers without worklet support.
    let usedWorklet = false;
    try {
      await audioCtx.audioWorklet.addModule("/mic-worklet.js");
      workletNode = new AudioWorkletNode(audioCtx, "intellexor-capture");
      workletNode.port.onmessage = (e) => onChunk(e.data);
      sourceNode.connect(workletNode);
      workletNode.connect(sinkNode);
      usedWorklet = true;
    } catch {}
    if (!usedWorklet) {
      procNode = audioCtx.createScriptProcessor(4096, 1, 1);
      procNode.onaudioprocess = (e) => onChunk(e.inputBuffer.getChannelData(0));
      sourceNode.connect(procNode);
      procNode.connect(sinkNode); // required to pull ScriptProcessor — via mute, not speakers
    }

    upd();

    // recording-timer display only — no decoding while recording
    tickTimer = setInterval(updThrottled, 1000);
  }

  function stopRecording() {
    clearInterval(tickTimer); tickTimer = null;
    try { workletNode && workletNode.disconnect(); } catch {}
    try { workletNode && (workletNode.port.onmessage = null); } catch {}
    workletNode = null;
    try { procNode && (procNode.onaudioprocess = null); } catch {}
    try { procNode && procNode.disconnect(); } catch {}
    try { sourceNode && sourceNode.disconnect(); } catch {}
    try { sinkNode && sinkNode.disconnect(); } catch {}
    if (mediaStream) for (const t of mediaStream.getTracks()) t.stop();

    S.state = "decoding";
    S.decode = null;
    upd();
    const finish = () => {
      try { audioCtx && audioCtx.close(); } catch {}
      audioCtx = null; procNode = null; sourceNode = null; mediaStream = null; sinkNode = null;
      chunks = []; totalSamples = 0;
      S.state = "ready";
      S.decode = null;
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
    if (S.state === "decoding") return;
    if (!window.caches) {
      hooks.toast("Speech-to-text needs a secure context (https:// or localhost)", true);
      return;
    }
    if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
      hooks.toast("Microphone unavailable — requires a secure context (https:// or localhost)", true);
      return;
    }
    // Load on first use only — the ~1.2 GB download must never start
    // unless the user actually asked for speech-to-text.
    if (S.state === "loading" || !loaded) {
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
      const prog = S.decode && S.decode.total > 1 ? ` — segment ${S.decode.done}/${S.decode.total}` : "";
      head.innerHTML = `<span class="d-title">Transcribing${prog}…</span>` +
        (S.device ? `<span class="mic-device">${dev}</span>` : "");
      d.appendChild(head);
    }
    return d;
  }

  function cancelDownload() {
    // transformers.js downloads can't be aborted cleanly — kill the worker
    // and start over on the next toggle.
    if (worker) { try { worker.terminate(); } catch {} }
    worker = null; client = null; loaded = false; loadPromise = null;
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
