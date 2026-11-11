"use client";

// The session's baseline: each sensor's band strength over the first 30
// seconds of the session, read from the stored 10-second rollups. For a
// session replayed from a later point, that is the first 30 seconds from
// the first full 10-second bucket after that point. Because
// it comes from the database, it is the same whenever the page was opened,
// and it does not change when the band changes.

import { useEffect, useState } from "react";
import { api, BANDS } from "@/lib/api";

export const BASELINE_SECONDS = 30;
const BUCKET_SECONDS = 10;
const POLL_MS = 3000;

// The baseline per sensor and band: value(channel, bandIndex), NaN for a
// sensor that was bad in every baseline rollup.
// Returned by: useBaseline. Used by: the session page (popover, chart).
export type Baseline = { value: (channel: number, band: number) => number };

// useBaseline waits until the recording is past the baseline period, then
// asks for the first three rollups until all three are stored. A sensor's
// bad rollups are left out of its average. It returns null until then.
// With `final` (the session has ended) it settles for the rollups there are,
// because a short session will never have three.
export function useBaseline(id: string, names: string[], startedAt: string | null, elapsedSeconds: number,
  offsetSeconds: number, final: boolean): Baseline | null {
  const [baseline, setBaseline] = useState<Baseline | null>(null);
  // The baseline period starts at the first bucket boundary at or after the session's own start point.
  const fromSeconds = Math.ceil(offsetSeconds / BUCKET_SECONDS) * BUCKET_SECONDS;
  const due = startedAt != null && elapsedSeconds >= fromSeconds + BASELINE_SECONDS;

  useEffect(() => setBaseline(null), [id]);

  useEffect(() => {
    if (!due || baseline || !startedAt || names.length === 0) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    const start = Date.parse(startedAt) + fromSeconds * 1000;
    const want = BASELINE_SECONDS / BUCKET_SECONDS;

    const load = async () => {
      try {
        const { rollups } = await api.rollups(id, new Date(start).toISOString(),
          new Date(start + BASELINE_SECONDS * 1000).toISOString());
        if (stopped) return;
        if (rollups.length < want && !final) {
          timer = setTimeout(load, POLL_MS); // the last bucket is written a moment after it ends
          return;
        }
        if (rollups.length === 0) return; // an ended session with nothing stored in its first 30 s
        const n = names.length;
        const sum = new Float64Array(n * BANDS.length);
        const count = new Int32Array(n);
        for (const r of rollups) {
          const bad = new Set(r.badChannels);
          for (let c = 0; c < n; c++) {
            if (bad.has(names[c])) continue;
            count[c]++;
            for (let b = 0; b < BANDS.length; b++) sum[c * BANDS.length + b] += r.bandPower[c * BANDS.length + b] ?? 0;
          }
        }
        setBaseline({
          value: (c, b) => (count[c] ? sum[c * BANDS.length + b] / count[c] : NaN),
        });
      } catch {
        if (!stopped) timer = setTimeout(load, POLL_MS);
      }
    };
    load();
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  }, [due, baseline, id, names, startedAt, fromSeconds, final]);

  return baseline;
}
