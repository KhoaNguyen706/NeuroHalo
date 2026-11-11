"use client";

// The raw signal, as it arrives: one scrolling trace per watched sensor,
// stacked like a paper EEG. With a single sensor it also draws that
// sensor's frequency spectrum (how strong each frequency from 1 to 45 Hz is
// over the last 2 seconds). Everything is drawn on a canvas from a ref every
// animation frame, so the 10 messages a second never re-render React.

import { useEffect, useRef, type RefObject } from "react";
import type { LiveFrame } from "@/lib/api";
import type { RawBuffer } from "@/hooks/useLiveSession";
import { usePalette } from "@/lib/theme";
import styles from "./RawTraces.module.css";

const SECONDS_SHOWN = 5;
const MIN_ROW_MICROVOLTS = 50; // half a row's height is at least this many microvolts
const MAX_ROW_MICROVOLTS = 1000;
const SCALE_STEP = 50; // the scale grows in steps of this many microvolts
const TOP = 16; // room above the traces for the scale text
const SPECTRUM_SECONDS = 2; // the window the spectrum is computed over
const SPECTRUM_MAX_HZ = 45;
const SPECTRUM_EVERY_MS = 250; // the spectrum is recomputed this often, not every animation frame
const QUALITY_OK = 1;
const LABEL_W = 44; // room for the sensor names on the left
// Where the rhythm bands start, for the spectrum's axis.
const BAND_EDGES: [number, string][] = [[1, "δ"], [4, "θ"], [8, "α"], [13, "β"], [30, "γ"]];

// spectrum returns the amplitude at each whole frequency from 1 to
// SPECTRUM_MAX_HZ for the newest `n` samples of a ring buffer: mean removed,
// Hann window, then one sum per frequency. 45 sums over 320 samples is
// small enough to do directly, with no FFT.
function spectrum(ring: Float32Array, written: number, n: number, rate: number): Float32Array {
  const out = new Float32Array(SPECTRUM_MAX_HZ + 1);
  if (written < n) return out;
  const cap = ring.length;
  const x = new Float32Array(n);
  let mean = 0;
  for (let i = 0; i < n; i++) {
    x[i] = ring[(written - n + i) % cap];
    mean += x[i];
  }
  mean /= n;
  for (let i = 0; i < n; i++) x[i] = (x[i] - mean) * (0.5 - 0.5 * Math.cos((2 * Math.PI * i) / (n - 1)));
  for (let f = 1; f <= SPECTRUM_MAX_HZ; f++) {
    let re = 0;
    let im = 0;
    const w = (2 * Math.PI * f) / rate;
    for (let i = 0; i < n; i++) {
      re += x[i] * Math.cos(w * i);
      im -= x[i] * Math.sin(w * i);
    }
    out[f] = (Math.hypot(re, im) * 4) / n; // 4/n: back to microvolts after the Hann window
  }
  return out;
}

// RawTraces draws the panel. It re-renders only when its props change (the
// set of sensors), never for new data.
// Used by: the session page.
export default function RawTraces({ rawRef, frameRef, names, rate }: {
  rawRef: RefObject<RawBuffer>; // the newest raw samples, filled by useLiveSession
  frameRef: RefObject<LiveFrame | null>; // newest frame: a sensor that is not OK is drawn red
  names: string[]; // channel names, in the session's channel order
  rate: number; // samples per second
}) {
  const canvas = useRef<HTMLCanvasElement>(null);
  // The theme's colors, read by the drawing loop through a ref, so a theme change needs no restart.
  const palette = usePalette();
  const paletteRef = useRef(palette);
  paletteRef.current = palette;

  useEffect(() => {
    const el = canvas.current;
    if (!el) return;
    const ctx = el.getContext("2d")!;
    let raf = 0;
    let bars: Float32Array = new Float32Array(SPECTRUM_MAX_HZ + 1);
    let barsAt = 0;

    const draw = (now: number) => {
      raf = requestAnimationFrame(draw);
      const dpr = window.devicePixelRatio || 1;
      const w = el.clientWidth;
      const h = el.clientHeight;
      if (el.width !== Math.round(w * dpr) || el.height !== Math.round(h * dpr)) {
        el.width = Math.round(w * dpr);
        el.height = Math.round(h * dpr);
      }
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      ctx.clearRect(0, 0, w, h);
      ctx.font = '12px "IBM Plex Sans", system-ui, sans-serif';
      ctx.textBaseline = "middle";

      const { accent: AMBER, red: RED, grid: GRID, text: TEXT } = paletteRef.current;
      const buf = rawRef.current;
      const rows = buf.channels.length;
      if (rows === 0) return;
      const single = rows === 1;
      const tracesW = single ? Math.floor(w * 0.62) : w; // the spectrum takes the rest
      const plotW = tracesW - LABEL_W - 8;
      const rowH = (h - TOP) / rows;
      const n = Math.round(SECONDS_SHOWN * rate);
      const quality = frameRef.current?.quality;
      const have = Math.min(n, buf.written);

      // Each trace's own average, and one scale for all rows: just large
      // enough for the biggest swing of any sensor that is OK, so a noisy
      // sensor cannot flatten the others. A bad sensor is clipped to its row.
      const means = buf.data.map((ring) => {
        let sum = 0;
        for (let i = 0; i < have; i++) sum += ring[(buf.written - have + i) % ring.length];
        return have ? sum / have : 0;
      });
      let swing = 0;
      buf.channels.forEach((channel, r) => {
        if (quality != null && quality[channel] !== QUALITY_OK) return;
        const ring = buf.data[r];
        for (let i = 0; i < have; i++) {
          const v = Math.abs(ring[(buf.written - have + i) % ring.length] - means[r]);
          if (v > swing) swing = v;
        }
      });
      const scale = Math.min(MAX_ROW_MICROVOLTS, Math.max(MIN_ROW_MICROVOLTS, Math.ceil(swing / SCALE_STEP) * SCALE_STEP));

      buf.channels.forEach((channel, r) => {
        const mid = TOP + rowH * (r + 0.5);
        ctx.fillStyle = TEXT;
        ctx.textAlign = "left";
        ctx.fillText(names[channel] ?? String(channel), 0, mid);
        ctx.strokeStyle = GRID;
        ctx.lineWidth = 1;
        ctx.beginPath();
        ctx.moveTo(LABEL_W, mid);
        ctx.lineTo(LABEL_W + plotW, mid);
        ctx.stroke();

        const ring = buf.data[r];
        if (have < 2) return;
        // Remove the trace's own average, so a constant offset does not push it off its row.
        const mean = means[r];
        const bad = quality != null && quality[channel] !== QUALITY_OK;
        ctx.strokeStyle = bad ? RED : AMBER;
        ctx.beginPath();
        const half = rowH / 2;
        for (let i = 0; i < have; i++) {
          const v = ring[(buf.written - have + i) % ring.length] - mean;
          // The newest sample sits at the right edge; a trace is clipped to its own row.
          const x = LABEL_W + ((n - have + i) / (n - 1)) * plotW;
          const y = mid - Math.max(-1, Math.min(1, v / scale)) * half * 0.9;
          if (i === 0) ctx.moveTo(x, y);
          else ctx.lineTo(x, y);
        }
        ctx.stroke();
      });

      // The scale, once, above the traces: how many microvolts half a row is.
      ctx.fillStyle = TEXT;
      ctx.textAlign = "left";
      ctx.fillText(`Raw signal · ±${scale} µV per row · last ${SECONDS_SHOWN} s`, LABEL_W, 7);

      if (!single) return;
      // The spectrum of the one watched sensor.
      if (now - barsAt > SPECTRUM_EVERY_MS) {
        bars = spectrum(buf.data[0], buf.written, Math.round(SPECTRUM_SECONDS * rate), rate);
        barsAt = now;
      }
      const left = tracesW + 24;
      const sw = w - left - 4;
      const top = TOP + 4;
      const sh = h - top - 20;
      let peak = 1;
      for (let f = 1; f <= SPECTRUM_MAX_HZ; f++) if (bars[f] > peak) peak = bars[f];
      const barW = sw / SPECTRUM_MAX_HZ;
      ctx.fillStyle = AMBER;
      for (let f = 1; f <= SPECTRUM_MAX_HZ; f++) {
        const bh = (bars[f] / peak) * sh;
        ctx.fillRect(left + (f - 1) * barW, top + sh - bh, Math.max(1, barW - 1), bh);
      }
      ctx.strokeStyle = GRID;
      ctx.beginPath();
      ctx.moveTo(left, top + sh + 0.5);
      ctx.lineTo(left + sw, top + sh + 0.5);
      ctx.stroke();
      ctx.fillStyle = TEXT;
      ctx.textAlign = "left";
      for (const [hz, symbol] of BAND_EDGES) ctx.fillText(`${symbol} ${hz}`, left + (hz - 1) * barW, top + sh + 11);
      ctx.fillText(`Spectrum, 1–${SPECTRUM_MAX_HZ} Hz · peak ${peak.toFixed(0)} µV`, left, 7);
    };
    raf = requestAnimationFrame(draw);
    return () => cancelAnimationFrame(raf);
  }, [rawRef, frameRef, names, rate]);

  return <canvas ref={canvas} className={styles.traces} aria-label="Raw signal" role="img" />;
}
