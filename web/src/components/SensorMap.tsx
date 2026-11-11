"use client";

// A flat map of the head seen from above, nose up: one dot per sensor, and
// an edge between each sensor and its nearest neighbours. Dots and edges use
// the same amber scale as the 3D view (brighter = stronger); an edge touching
// a bad sensor is red. Unlike the 3D head, it shows every sensor at once.

import { useMemo } from "react";
import type { LiveFrame } from "@/lib/api";
import positions from "@/positions.json";
import styles from "./SensorMap.module.css";

// positions.json maps a channel name to x, y, z on a sphere of radius 1:
// the nose points to -z, the subject's right is +x, up is +y.
const POSITIONS = positions as unknown as Record<string, [number, number, number]>;

const SIZE = 400; // the drawing's width and height, in its own units
const HEAD_R = 132; // radius of the circle where the head is widest (ear level); sensors below it land outside
const DOT_R = 7;
const NEIGHBOURS = 4; // each sensor is joined to this many nearest sensors
const FULL_POWER = 0.6; // relative power drawn at full brightness, as in the 3D view
const QUALITY_OK = 1;

// Colors come from the theme variables in globals.css.
const NO_DATA = "var(--no-data)";
const RED = "var(--red)";

// One sensor on the map: its channel index and where it is drawn.
// Built by: layout. Used by: SensorMap.
type Node = { channel: number; x: number; y: number };

// amber maps relative power to the scale color: the theme's --scale-low at
// zero, --scale-high at full power, mixed by the browser in between.
function amber(power: number): string {
  const t = Math.min(1, Math.max(0, power / FULL_POWER));
  return `color-mix(in srgb, var(--scale-high) ${Math.round(t * 100)}%, var(--scale-low))`;
}

// layout flattens the head: a sensor's distance from the centre of the map
// is its angle down from the top of the head, so the crown is in the middle
// and ear level is the circle. Sensors with no known position are left out.
// It also lists the edges: each sensor joined to its nearest neighbours on
// the head, each pair once.
function layout(names: string[]): { nodes: Node[]; edges: [number, number][] } {
  const nodes: Node[] = [];
  const dirs: [number, number, number][] = [];
  names.forEach((name, channel) => {
    const p = POSITIONS[name];
    if (!p) return;
    const len = Math.hypot(p[0], p[1], p[2]) || 1;
    const d: [number, number, number] = [p[0] / len, p[1] / len, p[2] / len];
    const r = (Math.acos(Math.min(1, Math.max(-1, d[1]))) / (Math.PI / 2)) * HEAD_R;
    const flat = Math.hypot(d[0], d[2]) || 1;
    nodes.push({ channel, x: SIZE / 2 + (r * d[0]) / flat, y: SIZE / 2 + (r * d[2]) / flat });
    dirs.push(d);
  });

  const seen = new Set<string>();
  const edges: [number, number][] = [];
  dirs.forEach((a, i) => {
    dirs
      .map((b, j) => ({ j, dot: a[0] * b[0] + a[1] * b[1] + a[2] * b[2] }))
      .filter((n) => n.j !== i)
      .sort((p, q) => q.dot - p.dot) // a larger dot product means a smaller angle
      .slice(0, NEIGHBOURS)
      .forEach(({ j }) => {
        const key = i < j ? `${i}-${j}` : `${j}-${i}`;
        if (seen.has(key)) return;
        seen.add(key);
        edges.push([Math.min(i, j), Math.max(i, j)]);
      });
  });
  return { nodes, edges };
}

// SensorMap draws the map for the newest frame. The page re-renders it
// twice a second, which is all the animation it needs.
// Used by: the session page.
export default function SensorMap({ names, frame, highlight, onPick }: {
  names: string[]; // channel names, in the session's channel order
  frame: LiveFrame | null; // newest frame, or null before any data
  highlight: number | null; // the channel drawn larger, as in the 3D view
  onPick: (channel: number) => void; // a dot was clicked
}) {
  const { nodes, edges } = useMemo(() => layout(names), [names]);
  const bad = (n: Node) => frame != null && frame.quality[n.channel] !== QUALITY_OK;
  const color = (n: Node) => (frame ? amber(frame.power[n.channel]) : NO_DATA);

  return (
    <svg className={styles.map} viewBox={`0 0 ${SIZE} ${SIZE}`} role="img" aria-label="Sensor map, seen from above">
      {/* the head at ear level, and the nose */}
      <circle cx={SIZE / 2} cy={SIZE / 2} r={HEAD_R} className={styles.head} />
      <path d={`M${SIZE / 2 - 12},${SIZE / 2 - HEAD_R + 1} L${SIZE / 2},${SIZE / 2 - HEAD_R - 14} L${SIZE / 2 + 12},${SIZE / 2 - HEAD_R + 1}`}
        className={styles.head} />

      {edges.map(([i, j]) => {
        const a = nodes[i];
        const b = nodes[j];
        const distrust = bad(a) || bad(b);
        const stroke = distrust
          ? RED
          : frame ? amber((frame.power[a.channel] + frame.power[b.channel]) / 2) : NO_DATA;
        return (
          <line key={`${i}-${j}`} x1={a.x} y1={a.y} x2={b.x} y2={b.y} style={{ stroke }}
            className={distrust ? styles.badEdge : styles.edge} />
        );
      })}

      {nodes.map((n) => (
        <g key={n.channel} className={styles.node} onClick={() => onPick(n.channel)}>
          <title>{names[n.channel]}</title>
          {bad(n) && <circle cx={n.x} cy={n.y} r={DOT_R + 4} className={styles.ring} />}
          <circle cx={n.x} cy={n.y} r={n.channel === highlight ? DOT_R * 1.6 : DOT_R} style={{ fill: color(n) }} />
        </g>
      ))}
    </svg>
  );
}
