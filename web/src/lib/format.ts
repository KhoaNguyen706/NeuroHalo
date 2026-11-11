// Small display helpers shared by the pages.

// clock formats seconds as m:ss, e.g. 83.5 -> "1:23".
export function clock(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

// parseClock reads "1:23", "83" or "83.5" as seconds; null for anything else.
export function parseClock(text: string): number | null {
  const m = text.trim().match(/^(?:(\d+):)?(\d+(?:\.\d+)?)$/);
  if (!m) return null;
  return (m[1] ? Number(m[1]) * 60 : 0) + Number(m[2]);
}

// percent formats a 0..1 value as "41%".
export function percent(v: number | null | undefined): string {
  return v == null || Number.isNaN(v) ? "–" : `${Math.round(v * 100)}%`;
}

// change formats a ratio against a baseline as a signed percent: 3.1 -> "+210%", 0.8 -> "−20%".
export function change(ratio: number): string {
  if (!Number.isFinite(ratio)) return "–";
  const p = Math.round((ratio - 1) * 100);
  return `${p >= 0 ? "+" : "−"}${Math.abs(p)}%`;
}

// sessionName is what a session is called on screen: its label, or its ID.
export function sessionName(s: { id: string; label: string }): string {
  return s.label || s.id;
}

// duration formats the time between two ISO dates (or until now) as m:ss.
export function duration(startIso: string, endIso: string | null): string {
  const end = endIso ? Date.parse(endIso) : Date.now();
  return clock((end - Date.parse(startIso)) / 1000);
}
