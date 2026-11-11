// CSV downloads for a session: the stored 10-second averages and the events.

import { api, BANDS, type Session } from "@/lib/api";

// cell quotes one CSV value when it holds a comma, a quote or a line break.
function cell(value: string | number): string {
  const s = String(value);
  return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

// download hands the browser a CSV file made of the rows.
function download(filename: string, rows: (string | number)[][]) {
  const blob = new Blob([rows.map((r) => r.map(cell).join(",")).join("\n") + "\n"], { type: "text/csv" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}

// exportAverages downloads one row per 10-second bucket and sensor: when the
// bucket starts (seconds into the recording), the sensor, each band's
// relative power, and whether the sensor was bad for most of the bucket.
// Used by: the session page's export button.
export async function exportAverages(session: Session) {
  const { rollups } = await api.rollups(session.id);
  const start = Date.parse(session.startedAt);
  const rows: (string | number)[][] = [["start_seconds", "sensor", ...BANDS, "bad", "frames"]];
  for (const r of rollups) {
    const bad = new Set(r.badChannels);
    session.channelNames.forEach((name, c) => {
      rows.push([
        (Date.parse(r.bucketStart) - start) / 1000,
        name,
        ...BANDS.map((_, b) => (r.bandPower[c * BANDS.length + b] ?? 0).toFixed(5)),
        bad.has(name) ? 1 : 0,
        r.frames,
      ]);
    });
  }
  download(`${session.id}-averages.csv`, rows);
}

// exportEvents downloads one row per stored event: seconds into the
// recording, the event type, the sensor (empty for whole-head events) and
// the details as JSON.
// Used by: the session page's export button.
export async function exportEvents(session: Session) {
  const events = await api.events(session.id);
  const start = Date.parse(session.startedAt);
  const rows: (string | number)[][] = [["seconds", "type", "sensor", "details"]];
  for (const e of events) {
    rows.push([(Date.parse(e.at) - start) / 1000, e.type, e.channel, JSON.stringify(e.details ?? {})]);
  }
  download(`${session.id}-events.csv`, rows);
}
