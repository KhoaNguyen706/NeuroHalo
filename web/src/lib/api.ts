// Talking to the api service: REST calls, the WebSocket address, and the
// shapes of everything it returns.

export const API_URL = process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";
// v1 uses one static token. It ships to the browser, so it only keeps out
// casual callers, not a determined one.
const TOKEN = process.env.NEXT_PUBLIC_API_TOKEN ?? "dev-token";

export const BANDS = ["delta", "theta", "alpha", "beta", "gamma"] as const;
export type Band = (typeof BANDS)[number];
export const BAND_RANGE: Record<Band, string> = {
  delta: "1–4 Hz",
  theta: "4–8 Hz",
  alpha: "8–13 Hz",
  beta: "13–30 Hz",
  gamma: "30–45 Hz",
};

// The Greek letters the bands are named after.
export const BAND_SYMBOL: Record<Band, string> = { delta: "δ", theta: "θ", alpha: "α", beta: "β", gamma: "γ" };

// Quality codes, as in the proto enum.
export const QUALITY_NAME: Record<number, string> = { 0: "unknown", 1: "ok", 2: "flat", 3: "noisy" };

// One session. Returned by: api.sessions, api.session. Used by: the home page
// list and the session page.
export type Session = {
  id: string;
  label: string;
  source: string;
  sampleRateHz: number;
  channelNames: string[];
  startedAt: string;
  endedAt: string | null;
  status: "live" | "ended";
};

// A stored event. Returned by: api.events. Turned into a SessionEvent by fromStored.
export type StoredEvent = { type: string; channel: string; at: string; details: Record<string, unknown> };

// One event as the pages use it, from REST or the WebSocket.
// Built by: fromStored, fromLive. Used by: EventList, the session page.
export type SessionEvent = { type: string; channel: string; atMs: number; details: Record<string, unknown> };

// A 10-second rollup. Returned by: api.rollups. Used by: useBaseline.
export type Rollup = { bucketStart: string; bandPower: number[]; badChannels: string[]; frames: number };

// The replay job behind a sample or upload. Returned by: api.replay.
// Used by: the session page while it waits for the session to start.
export type Replay = {
  sessionId: string;
  kind: "sample" | "upload";
  label: string;
  speed: "1x" | "fast";
  startSeconds: number; // where in the recording it started; above 0 for "replay from here"
  endSeconds: number; // where it stops; 0 when it plays to the end
  status: "queued" | "running" | "done" | "failed";
  error: string;
  createdAt: string;
};

// A built-in sample. Returned by: api.samples. Used by: the home page.
export type Sample = { name: string; title: string; description: string };

// What the WebSocket sends. Used by: useLive and the session page.
export type LiveFrame = {
  type: "frame";
  windowIndex: number;
  sentTimeMs: number;
  band: Band;
  power: number[];
  quality: number[];
};
// The strongest correlated sensor pairs of one window, as [a, b, strength],
// sent after each frame to a client subscribed with "links": true.
// Used by: useLiveSession (stores it) and HeadScene (draws it).
export type LiveLinks = {
  type: "links";
  windowIndex: number;
  pairs: [number, number, number][];
};
// The raw signal of the subscribed channels for one 100 ms batch, in
// microvolts, sent to a client subscribed with "raw": [channels].
// Used by: useLiveSession, which appends it to the RawBuffer.
export type LiveRaw = {
  type: "raw";
  firstSample: number;
  channels: number[];
  samples: number[][]; // one row per channel in `channels`
};
export type LiveEvent = {
  type: "event";
  eventType: string;
  channel: string;
  atMs: number;
  details: Record<string, unknown>;
};

// An error answer from the api, with its HTTP status.
// Thrown by: call. Caught by: the pages, which show the message.
export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

// call sends one request with the token and returns the parsed JSON body.
async function call<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch(API_URL + path, {
    ...init,
    headers: { Authorization: `Bearer ${TOKEN}`, ...init.headers },
    cache: "no-store",
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new ApiError(res.status, body.error ?? res.statusText);
  }
  return res.json() as Promise<T>;
}

const enc = encodeURIComponent;

export const api = {
  sessions: () => call<Session[]>("/sessions"),
  session: (id: string) => call<Session>(`/sessions/${enc(id)}`),
  // deleteSession removes an ended session and everything stored for it.
  deleteSession: (id: string) => call<{ deleted: string }>(`/sessions/${enc(id)}`, { method: "DELETE" }),
  events: (id: string) => call<StoredEvent[]>(`/sessions/${enc(id)}/events`),
  rollups: (id: string, from?: string, to?: string) => {
    const q = new URLSearchParams();
    if (from) q.set("from", from);
    if (to) q.set("to", to);
    const qs = q.size ? `?${q}` : "";
    return call<{ bands: Band[]; rollups: Rollup[] }>(`/sessions/${enc(id)}/rollups${qs}`);
  },
  replay: (id: string) => call<Replay>(`/replays/${enc(id)}`),
  // replayFrom queues a new live replay of a session's recording, from `from`
  // seconds into it up to `to` seconds (to the end when `to` is 0).
  replayFrom: (id: string, from: number, to: number) =>
    call<{ sessionId: string }>(
      `/sessions/${enc(id)}/replay?from=${enc(String(from))}${to > 0 ? `&to=${enc(String(to))}` : ""}&speed=1x`,
      { method: "POST" },
    ),
  samples: () => call<Sample[]>("/samples"),
  playSample: (name: string, speed: string) =>
    call<{ sessionId: string }>(`/samples/${enc(name)}/play?speed=${enc(speed)}`, { method: "POST" }),
  upload: (file: File, speed: string) => {
    const form = new FormData();
    form.append("file", file);
    form.append("speed", speed);
    return call<{ sessionId: string }>("/uploads", { method: "POST", body: form });
  },
};

// liveUrl is the WebSocket address for one session. Browsers cannot set
// headers on a WebSocket, so the token travels in the query string.
export function liveUrl(id: string): string {
  return `${API_URL.replace(/^http/, "ws")}/ws/sessions/${enc(id)}/live?token=${enc(TOKEN)}`;
}

// fromStored converts a REST event to the page's shape.
export function fromStored(e: StoredEvent): SessionEvent {
  return { type: e.type, channel: e.channel, atMs: Date.parse(e.at), details: e.details ?? {} };
}

// fromLive converts a WebSocket event to the page's shape.
export function fromLive(e: LiveEvent): SessionEvent {
  return { type: e.eventType, channel: e.channel, atMs: e.atMs, details: e.details ?? {} };
}
