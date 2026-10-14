"use strict";

/*
 * Browser speech-to-text for Intellexor: sherpa-onnx (WASM, in a dedicated
 * Worker) + NVIDIA Parakeet TDT 0.6B v3 (int8, multilingual).
 * Everything runs client-side — audio never leaves the browser.
 *
 * The engine (wasm + glue, ~15 MB) ships embedded with the app: the worker
 * (sherpa-worker.js) imports sherpa-engine.js + sherpa-asr.js and receives
 * sherpa.wasm via postMessage. The model (~640 MB) is downloaded once on
 * first use — with a live progress bar — and cached in Cache Storage
 * ("intellexor-stt-v1"); later sessions load instantly from cache.
 */

(function () {
  const CACHE_NAME = "intellexor-stt-v1";
  const HF =
    "https://huggingface.co/csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8/resolve/main";
  const MODEL_FILES = [
    { key: "encoder", url: HF + "/encoder.int8.onnx", label: "parakeet encoder (int8)", bytes: 652184281 },
    { key: "decoder", url: HF + "/decoder.int8.onnx", label: "parakeet decoder (int8)", bytes: 11845275 },
    { key: "joiner", url: HF + "/joiner.int8.onnx", label: "parakeet joiner (int8)", bytes: 6355277 },
    { key: "tokens", url: HF + "/tokens.txt", label: "tokens", bytes: 93939 },
  ];
  const TOTAL_BYTES = MODEL_FILES.reduce((a, f) => a + f.bytes, 0);
  const TARGET_RATE = 16000;
  const LIVE_WINDOW_SEC = 12; // partial re-decodes only look at the recent tail (wasm RTFx ~1.7)

  const S = {
    state: "idle", // idle | loading | ready | recording | decoding
    progress: { loaded: 0, total: TOTAL_BYTES },
    partial: "",
    recordingSince: 0,
    error: "",
  };

  let worker = null, client = null, recognizerId = null;
  let mediaStream = null, audioCtx = null, sourceNode = null, procNode = null;
  let chunks = [], totalSamples = 0, decBusy = false, tickTimer = null;
  let abortCtl = null;
  let progress = {}; // file.key -> {loaded, total}
  const hooks = { onUpdate: () => {}, onText: () => {}, toast: () => {} };

  const el = (tag, cls, html) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (html !== undefined) e.innerHTML = html;
    return e;
  };
  const esc = (s) =>
    String(s == null ? "" : s).replace(/&/g, "&amp;").replace(/&lt;/g, "&lt;").replace(/>/g, "&gt;");
  const fmtMB = (n) => (n / 1048576).toFixed(1) + " MB";
  const upd = () => hooks.onUpdate();

  // the composer rebuilds on every update — never do it more than ~5x/s
  let lastUpd = 0;
  const updThrottled = () => {
    const now = Date.now();
    if (now - lastUpd > 200) { lastUpd = now; upd(); }
  };

  function sumProgress() {
    let loaded = 0, total = 0;
    for (const f of MODEL_FILES) {
      const p = progress[f.key];
      if (p) { loaded += p.loaded; total += Math.max(p.total, p.loaded); }
      else total += f.bytes;
    }
    return { loaded, total: Math.max(total, 1) };
  }

  async function openCache() { return caches.open(CACHE_NAME); }
  async function cachedResponse(url) { return (await openCache()).match(url); }

  // Stream the response into the cache while counting bytes: flat memory
  // usage (a 650 MB buffer here crashed tabs), live progress, and entries
  // only appear once the file is complete. A watchdog aborts stalled
  // downloads so the button can never get stuck silently.
  async function fetchWithProgress(file, ctl) {
    const res = await fetch(file.url, { signal: ctl.signal });
    if (!res.ok) throw new Error(file.label + ": HTTP " + res.status);
    const total = +(res.headers.get("content-length") || file.bytes || 0);
    let loaded = 0;
    let lastByteAt = Date.now();
    const watchdog = setInterval(() => {
      if (Date.now() - lastByteAt > 30000) {
        try { ctl.abort(new Error("download stalled (no data for 30s)")); } catch {}
      }
    }, 5000);
    const counter = new TransformStream({
      transform(chunk, ctrl) {
        loaded += chunk.byteLength;
        lastByteAt = Date.now();
        progress[file.key] = { loaded, total };
        S.progress = sumProgress();
        updThrottled();
        ctrl.enqueue(chunk);
      },
    });
    try {
      const counted = res.body.pipeThrough(counter, { signal: ctl.signal });
      await (await openCache()).put(
        file.url,
        new Response(counted, { headers: { "x-intellexor-size": String(total || loaded) } })
      );
    } catch (e) {
      await (await openCache()).delete(file.url).catch(() => {});
      throw e;
    } finally {
      clearInterval(watchdog);
    }
    progress[file.key] = { loaded, total: Math.max(total, loaded) };
    S.progress = sumProgress();
    upd();
  }

  // ---- worker plumbing (mirrors the worker's TaskScheduler protocol) ----

  class TaskClient {
    constructor(w) {
      this.tasks = new Map();
      this.counter = 0;
      w.addEventListener("message", (e) => {
        const { id, dir, data, error } = e.data || {};
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
    const [wRes, wasmRes] = await Promise.all([
      fetch("/sherpa-worker.js"),
      fetch("/sherpa.wasm"),
    ]);
    if (!wRes.ok) throw new Error("sherpa-worker.js: HTTP " + wRes.status);
    if (!wasmRes.ok) throw new Error("sherpa.wasm: HTTP " + wasmRes.status);
    const url = URL.createObjectURL(new Blob([await wRes.text()], { type: "text/javascript" }));
    worker = new Worker(url);
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
    const wasm = await wasmRes.arrayBuffer();
    await client.post("init", { wasm }, [wasm]);
  }

  async function ensureModel() {
    if (recognizerId) return true;
    S.state = "loading"; S.error = ""; S.progress = sumProgress(); upd();
    abortCtl = new AbortController();
    try {
      // pre-download the model with visible progress (the worker's own
      // fetch would be silent); the worker then hits the same cache
      for (const f of MODEL_FILES) {
        const hit = await cachedResponse(f.url);
        if (hit) {
          // validate: an interrupted session can leave incomplete entries
          const blob = await hit.blob();
          const expected = +(hit.headers.get("x-intellexor-size") || f.bytes);
          if (blob.size > 0 && blob.size === expected) {
            progress[f.key] = { loaded: blob.size, total: Math.max(blob.size, f.bytes) };
            S.progress = sumProgress(); upd();
            continue;
          }
          await (await openCache()).delete(f.url).catch(() => {});
        }
        await fetchWithProgress(f, abortCtl);
      }
      await ensureWorker();
      S.partial = "";
      upd();
      // hand the model bytes to the worker (MEMFS paths) and build the recognizer
      const names = { encoder: "/enc.onnx", decoder: "/dec.onnx", joiner: "/join.onnx", tokens: "/tokens.txt" };
      const files = {}, transfers = [];
      for (const f of MODEL_FILES) {
        const hit = await cachedResponse(f.url);
        if (!hit) throw new Error(f.label + ": not in cache");
        const buf = await hit.arrayBuffer();
        files[names[f.key]] = buf;
        transfers.push(buf);
      }
      recognizerId = await client.post("offline.create", {
        files,
        config: {
          modelConfig: {
            modelType: "nemo-transducer",
            transducer: {
              encoder: names.encoder,
              decoder: names.decoder,
              joiner: names.joiner,
            },
            tokens: names.tokens,
            numThreads: 1,
            debug: 0,
          },
        },
      }, transfers);
      S.state = "ready";
      upd();
      return true;
    } catch (e) {
      if (e && (e.name === "AbortError" || /abort/i.test(String(e && e.message)))) {
        S.state = "idle";
        upd();
        return false;
      }
      S.error = (e && e.message) || String(e);
      S.state = "idle";
      upd();
      hooks.toast("Speech model init failed: " + S.error, true);
      return false;
    } finally {
      abortCtl = null;
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
    const streamId = await client.post("offline.createStream", { recognizerId });
    try {
      await client.post("stream.acceptWaveform",
        { streamId, sampleRate: TARGET_RATE, samples }, [samples.buffer]);
      await client.post("offline.decode", { recognizerId, streamId });
      const result = await client.post("offline.getResult", { recognizerId, streamId });
      return (result && result.text ? result.text : "").trim();
    } finally {
      client.post("offline.streamFree", { streamId }).catch(() => {});
    }
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

    chunks = []; totalSamples = 0; decBusy = false;
    S.partial = "";
    S.recordingSince = Date.now();
    S.state = "recording";
    upd();

    tickTimer = setInterval(async () => {
      upd(); // keeps the recording timer fresh
      if (decBusy || !audioCtx || totalSamples < audioCtx.sampleRate * 0.7) return;
      decBusy = true;
      try {
        const window_ = Math.min(totalSamples, LIVE_WINDOW_SEC * TARGET_RATE);
        const all = mergeChunks(chunks, totalSamples);
        const tail = totalSamples > window_ ? all.subarray(totalSamples - window_) : all;
        const text = await decodeSamples(new Float32Array(tail));
        if (text && text !== S.partial) { S.partial = text; upd(); }
      } catch (e) {
        hooks.toast("Transcription: " + (e.message || e), true);
      } finally {
        decBusy = false;
      }
    }, 2000);
  }

  function stopRecording() {
    clearInterval(tickTimer); tickTimer = null;
    try { procNode && procNode.disconnect(); } catch {}
    try { sourceNode && sourceNode.disconnect(); } catch {}
    if (mediaStream) for (const t of mediaStream.getTracks()) t.stop();

    S.state = "decoding";
    upd();
    const finish = (text) => {
      try { audioCtx && audioCtx.close(); } catch {}
      audioCtx = null; procNode = null; sourceNode = null; mediaStream = null;
      chunks = []; totalSamples = 0; decBusy = false;
      S.state = "ready"; S.partial = "";
      upd();
      if (text) hooks.onText(text);
    };
    if (totalSamples > 0) {
      const all = mergeChunks(chunks, totalSamples);
      decodeSamples(all).then(finish).catch((e) => {
        hooks.toast("Transcription failed: " + (e.message || e), true);
        finish("");
      });
    } else {
      finish("");
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
    if (!recognizerId) {
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
      progress = {};
      hooks.toast("Cached speech model deleted (reload to re-download)");
    } catch (e) {
      hooks.toast("Cache clear failed: " + (e.message || e), true);
    }
  }

  function dockNode() {
    if (S.state === "idle" || S.state === "ready") return null;
    const d = el("div", "dock mic-dock");
    if (S.state === "loading") {
      const { loaded, total } = S.progress;
      const pct = Math.min(100, (loaded / total) * 100);
      const head = el("div", "d-head");
      head.innerHTML =
        `<span class="d-title">Downloading speech model <span class="mic-model">parakeet-v3 · int8 · 640 MB</span></span>` +
        `<span class="mic-dock-pct">${pct.toFixed(0)}%</span>`;
      d.appendChild(head);
      const bar = el("div", "mic-progress");
      bar.innerHTML = `<div class="mic-progress-fill" style="width:${pct.toFixed(1)}%"></div>`;
      d.appendChild(bar);
      d.appendChild(
        el("div", "mic-dock-sub",
          `${fmtMB(loaded)} of ${fmtMB(total)} — downloaded once, cached in your browser`)
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
      if (S.partial) d.appendChild(el("div", "mic-partial", esc(S.partial)));
    } else if (S.state === "decoding") {
      const head = el("div", "d-head");
      head.innerHTML = `<span class="d-title">Transcribing…</span>`;
      d.appendChild(head);
      if (S.partial) d.appendChild(el("div", "mic-partial", esc(S.partial)));
    }
    return d;
  }

  function cancelDownload() {
    if (abortCtl) abortCtl.abort();
  }

  window.IntellexorMic = {
    init(h) { Object.assign(hooks, h); },
    toggle,
    state: () => S.state,
    dockNode,
    clearCache,
  };
})();
