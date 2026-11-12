"use client";

// The averaged response chart: how band strength changes around a marker, as
// one line per marker label. Time runs from before the marker (left of the
// vertical line at 0) to after it; the height is the change compared with the
// time before the marker. It sits under the band strength chart on the
// session page and uses that chart's styles.

import { useEffect, useRef, useState } from "react";
import { change } from "@/lib/format";
import styles from "./Timeline.module.css";

// One label's line: the change at each step (0 is "as before the marker",
// -0.5 is half as strong; NaN where unknown), and how many trials are in it.
// Built by: the session page. Used by: ResponseChart.
export type ResponseLine = { label: string; trials: number; values: number[]; shade: string };

const HEIGHT = 150;
const M = { top: 10, right: 64, bottom: 22, left: 48 }; // margins around the plot, in pixels; labels go on the right
const LABEL_GAP = 13; // least distance between two line labels, in pixels

// ResponseChart draws the lines over the step times (seconds after the
// marker), a grid at the top, zero and bottom of the scale, a vertical line at
// the marker, and each line's label with its trial count at the right.
// Used by: the session page.
export default function ResponseChart({ title, times, lines }: {
  title: string; // what the chart shows, e.g. "Alpha · C3 · change after the marker"
  times: number[]; // per step: seconds after the marker (negative before it)
  lines: ResponseLine[];
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

  // The scale reaches the largest change, rounded up to the next 25%, and
  // never more than 200% so one wild line cannot flatten the others. Below
  // zero it stops at -100%: a rhythm cannot lose more than all of its strength.
  let peak = 0;
  for (const l of lines) for (const v of l.values) if (Number.isFinite(v)) peak = Math.max(peak, Math.abs(v));
  const yMax = Math.min(2, Math.max(0.25, Math.ceil(peak * 4 - 1e-9) / 4));
  const yMin = -Math.min(1, yMax);
  const t0 = times.length ? times[0] : -1;
  const t1 = times.length ? times[times.length - 1] : 1;
  const plotW = width - M.left - M.right;
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

  // Where each line's label goes: beside its last known point, pushed down
  // where needed so labels do not overlap, then shifted up if they ran past the plot.
  const labelRows = lines
    .flatMap((line) => {
      const last = [...line.values].reverse().find((v) => Number.isFinite(v));
      return last == null ? [] : [{ line, at: y(last) }];
    })
    .sort((a, b) => a.at - b.at);
  for (let i = 1; i < labelRows.length; i++) {
    labelRows[i].at = Math.max(labelRows[i].at, labelRows[i - 1].at + LABEL_GAP);
  }
  const overflow = labelRows.length ? labelRows[labelRows.length - 1].at - (M.top + plotH) : 0;
  if (overflow > 0) labelRows.forEach((row) => { row.at -= overflow; });

  const seconds: number[] = [];
  for (let t = Math.ceil(t0); t <= t1; t++) seconds.push(t);

  return (
    <figure className={styles.timeline}>
      <figcaption className={styles.head}>
        <span className={styles.title}>{title}</span>
      </figcaption>
      <div ref={box} className={styles.plot}>
        <svg width={width} height={HEIGHT} role="img" aria-label={title}>
          {[yMin, 0, yMax].map((v) => (
            <g key={v}>
              <line x1={M.left} x2={width - M.right} y1={y(v)} y2={y(v)} className={styles.grid} />
              <text x={M.left - 8} y={y(v)} dy="0.32em" textAnchor="end" className={styles.tick}>
                {v === 0 ? "0%" : change(1 + v)}
              </text>
            </g>
          ))}
          {times.length > 0 && seconds.map((t) => (
            <text key={t} x={x(t)} y={HEIGHT - 6} textAnchor="middle" className={styles.tick}>
              {t === 0 ? "marker" : `${t > 0 ? "+" : "−"}${Math.abs(t)} s`}
            </text>
          ))}
          {times.length > 0 && t0 < 0 && t1 > 0 && (
            <line x1={x(0)} x2={x(0)} y1={M.top} y2={M.top + plotH} className={styles.baseline} />
          )}
          {lines.map((l) => (
            <path key={l.label} d={pathOf(l.values)} className={styles.series} style={{ stroke: l.shade }} />
          ))}
          {labelRows.map(({ line, at }) => (
            <text key={line.label} x={width - M.right + 6} y={at} dy="0.32em" className={styles.label}
              style={{ fill: line.shade }}>{line.label} · {line.trials}</text>
          ))}
        </svg>
      </div>
    </figure>
  );
}
