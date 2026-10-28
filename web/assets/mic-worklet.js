/*
 * AudioWorklet capture for Intellexor's speech-to-text (loaded by mic.js).
 * ScriptProcessorNode is deprecated and runs on the main thread — under
 * load it drops or REPEATS audio buffers, which garbles the transcription
 * (missing sentences, duplicated text). This processor runs on the audio
 * render thread.
 *
 * Two deliberate details:
 * - Input quanta (128 frames ≈ 8 ms) are accumulated into ~128 ms blocks
 *   before posting — 8 messages/sec instead of 125 keeps the main thread
 *   (which re-renders the composer while recording) out of the hot path.
 * - The node keeps one output which mic.js routes through a zero-gain
 *   node into the destination. Engines only pull nodes reachable from the
 *   destination; a zero-output capture node is silently never processed
 *   on some of them (WebKitGTK/Safari). The output carries silence, so
 *   nothing is audible (no echo).
 *
 * The AudioContext is created at 16 kHz by mic.js, so chunks arrive
 * already resampled; they are copied because the input buffer is reused
 * by the audio thread.
 */
const BLOCK = 2048; // frames per posted chunk (~128 ms at 16 kHz)

class IntellexorCapture extends AudioWorkletProcessor {
  constructor() {
    super();
    this.buf = new Float32Array(BLOCK);
    this.fill = 0;
  }
  process(inputs) {
    const input = inputs[0];
    const ch = input && input[0];
    if (!ch || ch.length === 0) return true;
    let i = 0;
    while (i < ch.length) {
      const n = Math.min(ch.length - i, this.buf.length - this.fill);
      this.buf.set(ch.subarray(i, i + n), this.fill);
      this.fill += n;
      i += n;
      if (this.fill === this.buf.length) {
        this.port.postMessage(this.buf.slice(0));
        this.fill = 0;
      }
    }
    return true;
  }
}

registerProcessor("intellexor-capture", IntellexorCapture);
