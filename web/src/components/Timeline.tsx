"use client";

// A line chart of band strength over time: one or more lines that share the
// same times, a hover crosshair, a dashed baseline, a tick for each event,
// and (for an ended session) a playhead that a click or drag moves. It sits
// under the 3D head on the session page.

import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { clock, percent } from "@/lib/format";
import styles from "./Timeline.module.css";

// One point: seconds since the session started, and a 0..1 value.
// Built by: the session page. Used by: Timeline.
export type Point = { t: number; v: number };

// One line. Every line on a chart has the same times. label is drawn at the
// line's right end when there is more than one line; shade is its stroke color.
// Built by: the session page. Used by: Timeline.
export type Series = { key: string; label: string; points: Point[]; shade: string };

// One event on the time axis. kind picks the tick's color: "bad" red,
// "rise" amber, "recovered" grey. Built by: the session page. Used by: Timeline.
export type Mark = { key: string; t: number; kind: "bad" | "recovered" | "rise" };

const LABEL_GAP = 13; // least distance between two line labels, in pixels
const HEIGHT = 150;
const M = { top: 10, right: 64, bottom: 22, left: 40 }; // margins around the plot, in pixels; labels go on the right

// niceStep picks a tick spacing in seconds that gives about `want` ticks.
function niceStep(span: number, want: number): number {
  const raw = span / want;
  for (const s of [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600]) if (s >= raw) return s;
  return 3600 * Math.ceil(raw / 3600);
}

// Timeline draws the chart: the lines, a 0 / half / full grid, time ticks,
// the baseline as a dashed line, event ticks along the bottom (the focused
// one across the whole plot), a playhead, and a crosshair with the values
// under the pointer.
// Used by: the session page.
export default function Timeline({ title, controls, lines, baseline, marks, focusKey, cursorT, range, onPick }: {
  title: string; // what the chart shows, e.g. "Alpha · average of all sensors"
  controls?: ReactNode; // shown at the right of the title
  lines: Series[];
  baseline: number | null; // drawn when finite
  marks: Mark[];
  focusKey: string | null; // the mark picked in the event list
  cursorT?: number | null; // the playhead, for an ended session
  range?: [number, number] | null; // a marked stretch of time, shaded
  // A press (dragging false) or a drag (dragging true) on the plot, with the nearest point's time (ended session only).
  onPick?: (t: number, dragging: boolean) => void;
}) {
  const box = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(600);
  const [hoverT, setHoverT] = useState<number | null>(null);
  const dragging = useRef(false);

  useEffect(() => {
    const el = box.current;
    if (!el) return;
    const ro = new ResizeObserver(([e]) => setWidth(Math.max(240, e.contentRect.width)));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  const times = lines[0]?.points ?? [];
  // The y axis tops out at the next 10% above the highest value.
  let top = Number.isFinite(baseline) ? (baseline as number) : 0;
  for (const l of lines) for (const p of l.points) if (p.v > top) top = p.v;
  const yMax = Math.min(1, Math.max(0.1, Math.ceil(top * 10 + 1e-9) / 10));
  const t0 = times.length ? times[0].t : 0;
  const t1 = times.length ? Math.max(times[times.length - 1].t, t0 + 1) : 1;
  const plotW = width - M.left - M.right;
  const plotH = HEIGHT - M.top - M.bottom;
  const x = (t: number) => M.left + ((t - t0) / (t1 - t0)) * plotW;
  const y = (v: number) => M.top + (1 - Math.min(Math.max(v, 0), yMax) / yMax) * plotH;

  const paths = useMemo(
    () => lines.map((l) => l.points
      .filter((p) => Number.isFinite(p.v))
      .map((p, i) => `${i ? "L" : "M"}${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join("")),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [lines, width, yMax],
  );

  const step = niceStep(t1 - t0, Math.max(2, Math.floor(plotW / 90)));
  const ticks: number[] = [];
  for (let t = Math.ceil(t0 / step) * step; t <= t1; t += step) ticks.push(t);

  // nearestIndex returns the index of the time closest to t.
  const nearestIndex = (t: number) => {
    let best = 0;
    times.forEach((p, i) => { if (Math.abs(p.t - t) < Math.abs(times[best].t - t)) best = i; });
    return best;
  };
  const timeAt = (clientX: number) => {
    const rect = box.current!.getBoundingClientRect();
    const px = Math.min(Math.max(clientX - rect.left - M.left, 0), plotW);
    return t0 + (px / plotW) * (t1 - t0);
  };
  const hovered = hoverT != null && times.length ? nearestIndex(hoverT) : null;
  const many = lines.length > 1;

  // Where each line's label goes: beside the line's last point, then pushed
  // down where needed so labels of lines that end close together do not overlap.
  const labelRows = lines
    .flatMap((line) => {
      const last = [...line.points].reverse().find((p) => Number.isFinite(p.v));
      return last ? [{ line, at: y(last.v) }] : [];
    })
    .sort((a, b) => a.at - b.at);
  for (let i = 1; i < labelRows.length; i++) {
    labelRows[i].at = Math.max(labelRows[i].at, labelRows[i - 1].at + LABEL_GAP);
  }
  // If pushing down ran past the bottom of the plot, shift the whole stack up.
  const overflow = labelRows.length ? labelRows[labelRows.length - 1].at - (M.top + plotH) : 0;
  if (overflow > 0) labelRows.forEach((row) => { row.at -= overflow; });

  return (
    <figure className={styles.timeline}>
      <figcaption className={styles.head}>
        <span className={styles.title}>{title}</span>
        {controls}
      </figcaption>
      <div
        ref={box}
        className={styles.plot}
        style={{ cursor: onPick ? "pointer" : "crosshair" }}
        onPointerMove={(e) => {
          if (!times.length) return;
          const t = timeAt(e.clientX);
          setHoverT(t);
          if (dragging.current && onPick) onPick(times[nearestIndex(t)].t, true);
        }}
        onPointerLeave={() => {
          setHoverT(null);
          dragging.current = false;
        }}
        onPointerDown={(e) => {
          if (!onPick || !times.length) return;
          dragging.current = true;
          onPick(times[nearestIndex(timeAt(e.clientX))].t, false);
        }}
        onPointerUp={() => (dragging.current = false)}
      >
        <svg width={width} height={HEIGHT} role="img" aria-label={title}>
          {[0, 0.5, 1].map((f) => (
            <g key={f}>
              <line x1={M.left} x2={width - M.right} y1={y(f * yMax)} y2={y(f * yMax)} className={styles.grid} />
              <text x={M.left - 8} y={y(f * yMax)} dy="0.32em" textAnchor="end" className={styles.tick}>
                {percent(f * yMax)}
              </text>
            </g>
          ))}
          {range && times.length > 0 && (() => {
            // Shade the marked stretch, cut off at the plot's edges.
            const a = Math.max(M.left, x(range[0]));
            const b = Math.min(width - M.right, x(range[1]));
            return b > a && <rect x={a} y={M.top} width={b - a} height={plotH} className={styles.range} />;
          })()}
          {times.length > 0 && ticks.map((t) => (
            <text key={t} x={x(t)} y={HEIGHT - 6} textAnchor="middle" className={styles.tick}>{clock(t)}</text>
          ))}
          {times.length > 0 && baseline != null && Number.isFinite(baseline) && (
            <g>
              <line x1={M.left} x2={width - M.right} y1={y(baseline)} y2={y(baseline)} className={styles.baseline} />
              <text x={width - M.right + 6} y={y(baseline)} dy="0.32em" className={styles.tick}>baseline</text>
            </g>
          )}
          {times.length > 0 && marks.filter((m) => m.t >= t0 && m.t <= t1).map((m) => (
            <line key={m.key} x1={x(m.t)} x2={x(m.t)} y1={m.key === focusKey ? M.top : M.top + plotH - 8}
              y2={M.top + plotH} className={`${styles.mark} ${styles[m.kind]}`} />
          ))}
          {lines.map((l, i) => (
            <path key={l.key} d={paths[i]} className={styles.series} style={{ stroke: l.shade }} />
          ))}
          {many && labelRows.map(({ line, at }) => (
            <text key={line.key} x={width - M.right + 6} y={at} dy="0.32em" className={styles.label}
              style={{ fill: line.shade }}>{line.label}</text>
          ))}
          {cursorT != null && times.length > 0 && (
            <line x1={x(cursorT)} x2={x(cursorT)} y1={M.top} y2={M.top + plotH} className={styles.playhead} />
          )}
          {hovered != null && (
            <g>
              <line x1={x(times[hovered].t)} x2={x(times[hovered].t)} y1={M.top} y2={M.top + plotH}
                className={styles.crosshair} />
              {lines.map((l) => Number.isFinite(l.points[hovered]?.v) && (
                <circle key={l.key} cx={x(l.points[hovered].t)} cy={y(l.points[hovered].v)} r={3.5}
                  style={{ fill: l.shade }} />
              ))}
            </g>
          )}
        </svg>
        {hovered != null && (
          <div className={styles.tooltip} style={{ left: Math.min(x(times[hovered].t) + 10, width - 170), top: 4 }}>
            {lines.map((l) => (
              <span key={l.key}>
                {many && <span className={styles.dim}>{l.label} </span>}
                <span className={styles.value}>{percent(l.points[hovered]?.v)}</span>
              </span>
            ))}
            <span>{clock(times[hovered].t)}</span>
          </div>
        )}
      </div>
    </figure>
  );
}
