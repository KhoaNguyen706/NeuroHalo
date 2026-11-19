"use client";

// The averaged response chart: one line per marker label, over time from
// before the marker (left of the vertical line at 0) to after it. It draws
// either how band strength changes compared with the time before the marker
// (the averaged response), or the voltage in µV (the ERP). It sits under the
// band strength chart on the session page and uses that chart's styles.

import { useEffect, useRef, useState, type ReactNode } from "react";
import { change } from "@/lib/format";
import styles from "./Timeline.module.css";

// One label's line: the value at each time (a change, where 0 is "as before
// the marker" and -0.5 half as strong, or µV; NaN where unknown), how many
// trials are in it and, when some were left out, of how many. An ERP line
// also has error bars (the SME around each bin's mean voltage) and a note
// after its count.
// Built by: the session page. Used by: ResponseChart.
export type ResponseLine = {
  label: string;
  trials: number;
  of?: number;
  values: number[];
  shade: string;
  bars?: { t: number; lo: number; hi: number }[];
  note?: string;
};

const HEIGHT = 150;
const M = { top: 10, right: 80, bottom: 22, left: 48 }; // margins around the plot, in pixels; labels go on the right
const NOTE_RIGHT = 210; // the right margin when the labels carry a note
const BAR_SPREAD = 4; // pixels between two lines' error bars at the same time
const LABEL_GAP = 13; // least distance between two line labels, in pixels

// ResponseChart draws the lines over the times (seconds after the marker), a
// grid at the top, zero and bottom of the scale, a vertical line at the
// marker, and each line's label with its trial count at the right.
// Used by: the session page.
export default function ResponseChart({ title, times, lines, unit = "change", controls, empty }: {
  title: string; // what the chart shows, e.g. "Alpha · C3 · change after the marker"
  times: number[]; // per point: seconds after the marker (negative before it)
  lines: ResponseLine[];
  unit?: "change" | "uV"; // a ratio minus 1, or microvolts
  controls?: ReactNode; // shown at the right of the title
  empty?: string; // shown over the plot while no line has a value
}) {
  const box = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(600);

  useEffect(() => {
    const el = box.current;
    if (!el) return;
    const ro = new ResizeObserver(([e]) => setWidth(Math.max(240, e.contentRect.width)));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  // A change: the scale reaches the largest one, rounded up to the next 25%,
  // and never more than 200% so one wild line cannot flatten the others. Below
  // zero it stops at -100%: a rhythm cannot lose more than all of its strength.
  // Microvolts: the same distance above and below 0, rounded up to the next 5 µV.
  let peak = 0;
  for (const l of lines) for (const v of l.values) if (Number.isFinite(v)) peak = Math.max(peak, Math.abs(v));
  const uv = unit === "uV";
  const yMax = uv ? Math.max(5, Math.ceil(peak / 5 - 1e-9) * 5) : Math.min(2, Math.max(0.25, Math.ceil(peak * 4 - 1e-9) / 4));
  const yMin = uv ? -yMax : -Math.min(1, yMax);
  const t0 = times.length ? times[0] : -1;
  const t1 = times.length ? times[times.length - 1] : 1;
  const right = lines.some((l) => l.note) ? NOTE_RIGHT : M.right;
  const plotW = width - M.left - right;
  const plotH = HEIGHT - M.top - M.bottom;
  const x = (t: number) => M.left + ((t - t0) / (t1 - t0)) * plotW;
  const y = (v: number) => M.top + (1 - (Math.min(Math.max(v, yMin), yMax) - yMin) / (yMax - yMin)) * plotH;

  // pathOf joins a line's known points; a gap (NaN) starts a new stroke.
  const pathOf = (values: number[]) => {
    let d = "";
    let pen = false;
    values.forEach((v, i) => {
      if (!Number.isFinite(v)) return void (pen = false);
      d += `${pen ? "L" : "M"}${x(times[i]).toFixed(1)},${y(v).toFixed(1)}`;
      pen = true;
    });
    return d;
  };

  // Where each line's label goes: beside its last known point (at 0 for a
  // line with no point, so its count of 0 trials still shows), pushed down
  // where needed so labels do not overlap, then shifted up if they ran past the plot.
  const labelRows = lines
    .map((line) => {
      const last = [...line.values].reverse().find((v) => Number.isFinite(v));
      return { line, at: y(last ?? 0) };
    })
    .sort((a, b) => a.at - b.at);
  for (let i = 1; i < labelRows.length; i++) {
    labelRows[i].at = Math.max(labelRows[i].at, labelRows[i - 1].at + LABEL_GAP);
  }
  const overflow = labelRows.length ? labelRows[labelRows.length - 1].at - (M.top + plotH) : 0;
  if (overflow > 0) labelRows.forEach((row) => { row.at -= overflow; });

  // Time ticks: every second, or every 0.2 s on a span of 2 s or less.
  const tick = t1 - t0 <= 2 ? 0.2 : 1;
  const seconds: number[] = [];
  for (let k = Math.ceil(t0 / tick - 1e-9); k * tick <= t1 + 1e-9; k++) seconds.push(k);
  const timeText = (k: number) => {
    if (k === 0) return "marker";
    const sign = k > 0 ? "+" : "−";
    return tick === 1 ? `${sign}${Math.abs(k)} s` : `${sign}${Math.abs(Math.round(k * tick * 1000))} ms`;
  };
  const valueText = (v: number) =>
    uv ? `${v < 0 ? "−" : ""}${Math.abs(v)} µV` : v === 0 ? "0%" : change(1 + v);

  return (
    <figure className={styles.timeline}>
      <figcaption className={styles.head}>
        <span className={styles.title}>{title}</span>
        {controls}
      </figcaption>
      <div ref={box} className={styles.plot}>
        {empty && !lines.some((l) => l.values.some(Number.isFinite)) && <p className={styles.empty}>{empty}</p>}
        <svg width={width} height={HEIGHT} role="img" aria-label={title}>
          {[yMin, 0, yMax].map((v) => (
            <g key={v}>
              <line x1={M.left} x2={width - right} y1={y(v)} y2={y(v)} className={styles.grid} />
              <text x={M.left - 8} y={y(v)} dy="0.32em" textAnchor="end" className={styles.tick}>
                {valueText(v)}
              </text>
            </g>
          ))}
          {times.length > 0 && seconds.map((k) => (
            // A tick at the very start of the plot starts there, clear of the value labels.
            <text key={k} x={x(k * tick)} y={HEIGHT - 6} textAnchor={k * tick - t0 < 1e-9 ? "start" : "middle"}
              className={styles.tick}>
              {timeText(k)}
            </text>
          ))}
          {times.length > 0 && t0 < 0 && t1 > 0 && (
            <line x1={x(0)} x2={x(0)} y1={M.top} y2={M.top + plotH} className={styles.baseline} />
          )}
          {lines.map((l) => (
            <path key={l.label} d={pathOf(l.values)} className={styles.series} style={{ stroke: l.shade }} />
          ))}
          {/* The lines' bars share their times, so each line's sit a few pixels to the side. */}
          {lines.map((l, i) => l.bars?.map((b) => {
            const at = x(b.t) + (i - (lines.length - 1) / 2) * BAR_SPREAD;
            return (
              <line key={`${l.label}|${b.t}`} x1={at} x2={at} y1={y(b.hi)} y2={y(b.lo)}
                className={styles.errorBar} style={{ stroke: l.shade }} />
            );
          }))}
          {labelRows.map(({ line, at }) => (
            <text key={line.label} x={width - right + 6} y={at} dy="0.32em" className={styles.label}
              style={{ fill: line.shade }}>
              {line.label} · {line.trials}{line.of != null && line.of !== line.trials ? `/${line.of}` : ""}
              {line.note ? ` · ${line.note}` : ""}
            </text>
          ))}
        </svg>
      </div>
    </figure>
  );
}
