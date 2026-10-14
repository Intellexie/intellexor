// Intellexor STT worker: wraps the sherpa-onnx wasm engine (full build with the
// offline API) behind the task protocol mic.js speaks. The engine + model run
// entirely in this worker; audio and model bytes are transferred, never fetched.
importScripts(`${location.origin}/sherpa-engine.js`, `${location.origin}/sherpa-asr.js`);

let mod = null;
const recognizers = new Map();
const streams = new Map();
let seq = 0;

const post = (id, data) => self.postMessage({ id, dir: "sender", data });
const fail = (id, e) => self.postMessage({ id, dir: "sender", error: String((e && e.message) || e) });

self.postMessage({ type: "intellexor-ready" });

self.onmessage = async (e) => {
  const { id, type, payload } = e.data || {};
  if (id === undefined) return;
  try {
    let data;
    if (type === "init") {
      if (mod) throw new Error("engine already initialized");
      mod = await Module({ wasmBinary: payload.wasm, print: () => {}, printErr: () => {} });
      data = true;
    } else if (type === "offline.create") {
      for (const [name, buf] of Object.entries(payload.files)) {
        mod.FS.writeFile(name, new Uint8Array(buf));
      }
      const rec = new OfflineRecognizer(payload.config, mod);
      const rid = "r" + ++seq;
      recognizers.set(rid, rec);
      data = rid;
    } else if (type === "offline.createStream") {
      const st = recognizers.get(payload.recognizerId).createStream();
      const sid = "s" + ++seq;
      streams.set(sid, st);
      data = sid;
    } else if (type === "stream.acceptWaveform") {
      streams.get(payload.streamId).acceptWaveform(payload.sampleRate, payload.samples);
      data = true;
    } else if (type === "offline.decode") {
      recognizers.get(payload.recognizerId).decode(streams.get(payload.streamId));
      data = true;
    } else if (type === "offline.getResult") {
      const res = recognizers.get(payload.recognizerId).getResult(streams.get(payload.streamId));
      data = { text: res.text || "" };
    } else if (type === "offline.streamFree") {
      const st = streams.get(payload.streamId);
      if (st) { st.free(); streams.delete(payload.streamId); }
      data = true;
    } else {
      throw new Error("unknown task: " + type);
    }
    post(id, data);
  } catch (err) {
    fail(id, err);
  }
};
