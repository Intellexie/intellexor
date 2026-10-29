"use strict";

/* ============================== animated background ============================== */

// IntellexorBG paints a slow "aurora" — drifting color fields and rising
// sparkles — onto a fixed canvas behind the app (#bg in index.html). The
// canvas lives at the root level (position:fixed, inset:0) and the module
// starts itself as soon as this script loads, so the background is visible
// on every view from the first paint. renderMain() never starts or stops
// it; it only toggles body.bg-live on the landing (new session) view for
// full strength and body.bg-dim everywhere else so the panels stay readable
// (see the matching section in style.css).
//
// The render loop pauses while the tab is hidden, stops itself if the canvas
// ever leaves the DOM, re-reads its palette when the color scheme changes,
// and renders a single static frame when the user prefers reduced motion.
//
// NOTE: assigned to window explicitly — a top-level `const` only creates a
// global lexical binding, not a window property, and app.js gates on
// window.IntellexorBG.
window.IntellexorBG = (() => {
  const canvas = document.getElementById("bg-canvas");
  if (!canvas) return { setActive() {} };
  const ctx = canvas.getContext("2d");
  const reduced = matchMedia("(prefers-reduced-motion: reduce)");

  // Blob layout: base position as a fraction of the viewport, radius as a
  // fraction of the viewport diagonal, drift amplitude (ax/ay), drift
  // frequency (fx/fy) and phase (px/py) per axis, palette slot (c) and
  // relative strength (k). Everything is relative so the scene survives any
  // window size.
  const BLOBS = [
    { x: 0.16, y: 0.30, r: 0.46, c: 0, ax: 0.10, ay: 0.09, fx: 0.32, fy: 0.41, px: 0.0, py: 1.7, k: 1.00 },
    { x: 0.84, y: 0.20, r: 0.38, c: 1, ax: 0.09, ay: 0.12, fx: 0.44, fy: 0.30, px: 2.1, py: 0.4, k: 0.90 },
    { x: 0.55, y: 0.94, r: 0.52, c: 2, ax: 0.12, ay: 0.08, fx: 0.26, fy: 0.37, px: 4.2, py: 2.9, k: 0.85 },
    { x: 0.06, y: 0.90, r: 0.34, c: 1, ax: 0.08, ay: 0.10, fx: 0.38, fy: 0.27, px: 1.3, py: 5.1, k: 0.70 },
  ];

  // Tiny particles drifting upward with a sway and a twinkle; positions are
  // viewport fractions so they wrap around forever.
  const SPARKS = Array.from({ length: 42 }, () => ({
    x: Math.random(), y: Math.random(),
    v: 0.010 + Math.random() * 0.022, // viewport heights per second
    sf: 0.3 + Math.random() * 0.9,    // sway frequency
    sp: Math.random() * Math.PI * 2,  // sway phase
    tf: 0.4 + Math.random() * 1.2,    // twinkle frequency
    tp: Math.random() * Math.PI * 2,  // twinkle phase
    r: 0.7 + Math.random() * 1.3,     // radius in css px
  }));

  const STATIC_T = 11.3; // time slice used for the reduced-motion still

  let raf = 0;        // pending animation frame (0 = none)
  let active = false; // background currently on screen
  let W = 0, H = 0;   // canvas size in device pixels
  let dpr = 1;
  let pal = null;     // { rgb: [[r,g,b]...], alpha, glow }

  // The aurora drifts slowly — 30fps is visually indistinguishable from the
  // display's refresh rate but halves the full-screen gradient fill cost
  // (the loop used to redraw the whole viewport at 60fps+ forever, which
  // showed up as ~60% of a core on idle). Frames between ticks are skipped
  // without touching the canvas.
  const FRAME_MIN_MS = 1000 / 30;
  let lastDraw = 0;

  function readPalette() {
    const cs = getComputedStyle(document.documentElement);
    const triplet = (name) =>
      cs.getPropertyValue(name).trim().split(",").map((s) => parseInt(s, 10) || 0);
    pal = {
      rgb: [triplet("--aurora-1"), triplet("--aurora-2"), triplet("--aurora-3")],
      alpha: parseFloat(cs.getPropertyValue("--aurora-alpha")) || 0.14,
      glow: (document.documentElement.dataset.colorScheme || "dark") === "dark",
    };
  }

  function resize() {
    dpr = Math.min(window.devicePixelRatio || 1, 2);
    W = Math.max(1, Math.round(window.innerWidth * dpr));
    H = Math.max(1, Math.round(window.innerHeight * dpr));
    canvas.width = W; // assigning also clears the canvas
    canvas.height = H;
  }

  function blobAt(b, t) {
    const diag = Math.hypot(W, H);
    return {
      x: (b.x + Math.sin(t * b.fx + b.px) * b.ax) * W,
      y: (b.y + Math.cos(t * b.fy + b.py) * b.ay) * H,
      r: Math.max(1, b.r * diag * (1 + 0.07 * Math.sin(t * 0.21 + b.px))),
    };
  }

  function draw(t, dt) {
    ctx.clearRect(0, 0, W, H);
    // color fields — additive in dark mode so overlaps glow
    ctx.globalCompositeOperation = pal.glow ? "lighter" : "source-over";
    for (const b of BLOBS) {
      const { x, y, r } = blobAt(b, t);
      const [cr, cg, cb] = pal.rgb[b.c];
      const a = pal.alpha * b.k * (0.85 + 0.15 * Math.sin(t * 0.35 + b.py));
      const g = ctx.createRadialGradient(x, y, 0, x, y, r);
      g.addColorStop(0, `rgba(${cr},${cg},${cb},${a.toFixed(4)})`);
      g.addColorStop(1, `rgba(${cr},${cg},${cb},0)`);
      ctx.fillStyle = g;
      ctx.fillRect(x - r, y - r, r * 2, r * 2);
    }
    // rising sparkles
    const [sr, sg, sb] = pal.rgb[0];
    const baseA = pal.glow ? 0.55 : 0.35;
    for (const p of SPARKS) {
      if (dt > 0) {
        p.y -= p.v * dt;
        if (p.y < -0.02) { p.y = 1.02; p.x = Math.random(); }
      }
      const x = (p.x + Math.sin(t * p.sf + p.sp) * 0.006) * W;
      const y = p.y * H;
      const a = baseA * (0.5 + 0.5 * Math.sin(t * p.tf + p.tp));
      ctx.fillStyle = `rgba(${sr},${sg},${sb},${a.toFixed(3)})`;
      ctx.beginPath();
      ctx.arc(x, y, p.r * dpr, 0, Math.PI * 2);
      ctx.fill();
    }
    ctx.globalCompositeOperation = "source-over";
  }

  function frame(now) {
    raf = 0;
    if (!active || !canvas.isConnected) return; // stops the loop
    if (now - lastDraw >= FRAME_MIN_MS) {
      const dt = Math.min(Math.max((now - lastDraw) / 1000, 0), 0.05);
      lastDraw = now;
      draw(now / 1000, dt);
    }
    raf = requestAnimationFrame(frame);
  }

  function still() { draw(STATIC_T, 0); }

  function start() {
    readPalette();
    resize();
    if (reduced.matches) { still(); return; }
    lastDraw = performance.now();
    if (!raf) raf = requestAnimationFrame(frame);
  }

  function stop() {
    if (raf) cancelAnimationFrame(raf);
    raf = 0;
    // keep the last frame painted; body.bg-dim fades #bg via CSS
  }

  function setActive(on) {
    on = !!on;
    if (on === active) {
      // Re-assert while already on: if the canvas left the DOM and was
      // re-attached, the loop is dead — revive it without resetting state.
      if (on && !raf && canvas.isConnected && !reduced.matches) {
        lastDraw = performance.now();
        raf = requestAnimationFrame(frame);
      }
      return;
    }
    active = on;
    if (on) start(); else stop();
  }

  window.addEventListener("resize", () => {
    if (!active) return;
    resize();
    if (reduced.matches) still();
  });

  document.addEventListener("visibilitychange", () => {
    if (!active || reduced.matches) return;
    if (document.hidden) {
      if (raf) cancelAnimationFrame(raf);
      raf = 0;
    } else if (!raf) {
      lastDraw = performance.now();
      raf = requestAnimationFrame(frame);
    }
  });

  // follow light/dark switches
  new MutationObserver(() => {
    if (!active) return;
    readPalette();
    if (reduced.matches) still();
  }).observe(document.documentElement, { attributes: true, attributeFilter: ["data-color-scheme"] });

  readPalette();
  resize();

  // Root-level activation: the background belongs to the document, not to
  // any single view — start it here so it is visible everywhere, even before
  // app.js boots (and if app.js never gets that far, the loop still stops
  // cleanly on its own once the canvas is gone from the DOM). Honors the
  // user's "Animated background" setting (Settings → General); app.js's
  // applyBackground() flips it live via setActive().
  if (localStorage.getItem("intellexor.bg") !== "0") setActive(true);

  return { setActive };
})();
