/*
 * Intellexor STT worker: transformers.js (onnxruntime-web) behind the task
 * protocol mic.js speaks. Runs NVIDIA Parakeet (onnx-community/
 * parakeet-ctc-0.6b-ONNX, CTC — fp16 on GPU, q8 on CPU). The model choice is
 * a hard requirement: Parakeet, not Whisper. WebGPU when available, CPU wasm
 * otherwise. Everything runs client-side — audio never leaves the browser.
 *
 * The engine (transformers.min.js + onnxruntime wasm, ~42 MB) ships embedded
 * with the app and is served from /stt/. The model (~1.2 GB) is downloaded
 * once from the Hugging Face hub on first use and cached by transformers.js
 * in Cache Storage ("transformers-cache"); later sessions load from cache.
 *
 * Protocol (mirrors mic.js TaskClient): request/response messages carry
 * {id, dir:"receiver", type, payload} worker-bound and {id, dir:"sender",
 * data|error} back. Out-of-band: {type:"intellexor-ready"} on startup,
 * {type:"progress", loaded, total} during model download, {type:"fallback"}
 * when WebGPU init failed and we dropped to CPU.
 */
import { pipeline, env } from "./stt/transformers.min.js";

// Serve the vendored onnxruntime-web binaries from the app itself — no CDN,
// works fully offline once the model is cached.
env.allowLocalModels = false;
env.backends.onnx.wasm.wasmPaths = "/stt/";

// Hard requirement: Parakeet (FastConformer CTC). Do not swap for Whisper.
const MODEL = "onnx-community/parakeet-ctc-0.6b-ONNX";

let transcriber = null;
let progress = new Map(); // file -> {loaded, total}

const post = (id, data) => self.postMessage({ id, dir: "sender", data });
const fail = (id, e) => self.postMessage({ id, dir: "sender", error: String((e && e.message) || e) });

self.postMessage({ type: "intellexor-ready" });

function onProgress(p) {
  if (p.status !== "progress" || !p.file) return;
  const cur = progress.get(p.file) || { loaded: 0, total: 0 };
  cur.loaded = p.loaded || 0;
  cur.total = p.total || cur.total;
  progress.set(p.file, cur);
  let loaded = 0, total = 0;
  for (const v of progress.values()) { loaded += v.loaded; total += Math.max(v.total, v.loaded); }
  self.postMessage({ type: "progress", loaded, total: Math.max(total, 1) });
}

async function buildPipeline(device) {
  progress = new Map();
  return await pipeline("automatic-speech-recognition", MODEL, {
    device,
    // Parakeet is a single CTC model (no encoder/decoder split): fp16 on
    // WebGPU, q8 on CPU wasm.
    dtype: device === "webgpu" ? "fp16" : "q8",
    progress_callback: onProgress,
  });
}

self.onmessage = async (e) => {
  const { id, type, payload } = e.data || {};
  if (id === undefined) return;
  try {
    if (type === "load") {
      if (transcriber) { post(id, { device: payload.device }); return; }
      let device = payload.device || "webgpu";
      try {
        transcriber = await buildPipeline(device);
      } catch (err) {
        // WebGPU can fail on old drivers or missing shader-f16 — drop to CPU.
        if (device === "wasm") throw err;
        self.postMessage({ type: "fallback", error: String((err && err.message) || err) });
        transcriber = await buildPipeline("wasm");
        device = "wasm";
      }
      post(id, { device });
    } else if (type === "transcribe") {
      if (!transcriber) throw new Error("engine not loaded");
      // Parakeet is CTC: no language/task prompt tokens, and it handles
      // long recordings natively (FastConformer subsampling) — no chunking.
      const out = await transcriber(payload.samples);
      post(id, { text: (out && out.text ? out.text : "").trim() });
    } else {
      throw new Error("unknown task: " + type);
    }
  } catch (err) {
    fail(id, err);
  }
};
