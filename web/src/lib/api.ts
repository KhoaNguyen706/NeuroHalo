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

// One session. Returned by: api.sessions, api.session, api.setStudy; part of
// StudyDetail. Used by: the home page list, the session page and the study page.
export type Session = {
  id: string;
  label: string;
  source: string;
  sampleRateHz: number;
  channelNames: string[];
  startedAt: string;
  endedAt: string | null;
  status: "live" | "ended";
  recording: Recording | null; // null until the recorder has stored the first second
  studyId: string; // "" when the session is in no study
  participant: string; // who was recorded, inside the study
  task: string; // what they did
};

// One study in the list. Returned by: api.studies. Used by: the studies page.
export type Study = { id: string; name: string; createdAt: string; sessions: number };

// One study with its sessions, ordered by participant, task and start time.
// Returned by: api.study. Used by: the study page.
export type StudyDetail = { id: string; name: string; createdAt: string; sessionList: Session[] };

// BIDS_LABEL is what a participant or task may be: letters and digits only,
// because they become part of the file names. The api checks the same rule.
// Used by: the study page.
export const BIDS_LABEL = /^[A-Za-z0-9]{1,32}$/;

// The recorder's EDF file of a session's raw signal. Part of: Session.
// Used by: the session page, which offers the download once it is ready.
export type Recording = {
  status: "recording" | "writing" | "ready" | "failed";
  bytes: number; // the file's size, once ready
  seconds: number; // the signal it holds, once ready
};

// A stored event. Returned by: api.events. Turned into a SessionEvent by fromStored.
export type StoredEvent = { type: string; channel: string; at: string; details: Record<string, unknown> };

// One event as the pages use it, from REST or the WebSocket.
// Built by: fromStored, fromLive. Used by: EventList, the session page.
export type SessionEvent = { type: string; channel: string; atMs: number; details: Record<string, unknown> };

// A 10-second rollup. Returned by: api.rollups. Used by: useBaseline.
export type Rollup = { bucketStart: string; bandPower: number[]; badChannels: string[]; frames: number };

// The averaged response to one marker label, in one band.
// Part of: Responses. Used by: the session page.
export type LabelResponse = {
  label: string;
  trials: number; // markers with this label that have a finished epoch
  good: number[]; // per channel: how many of those trials are in its average (bad ones are left out)
  power: number[]; // steps x channels, row by row: mean band power, in an arbitrary unit
};

// The averaged responses of a session. Returned by: api.responses.
// Used by: useResponses, the session page.
export type Responses = {
  band: Band;
  times: number[]; // per step: seconds after the marker (the middle of the step's window)
  preSteps: number; // the first this-many steps hold only signal from before the marker
  stepSeconds: number;
  windowSeconds: number;
  labels: LabelResponse[];
};

// The ERP of one marker label: the voltage averaged over its trials.
// Part of: Erp. Used by: the session page.
export type LabelErp = {
  label: string;
  trials: number; // markers with this label that have a finished trial
  good: number[]; // per channel: how many of those are in its average (over the threshold or flat: left out)
  mean: number[]; // samples x channels, row by row: mean voltage in µV after filtering and baseline
  sme: number[]; // bins x channels, row by row: SME of each bin's mean voltage in µV, -1 below 2 trials
};

// The ERPs of a session. Returned by: api.erp. Used by: the session page.
export type Erp = {
  times: number[]; // per sample: seconds after the marker, from -0.2 to 0.8
  lowpassHz: number; // the low-pass filter applied before averaging
  reject: number; // µV peak-to-peak above which a trial is left out; 0 keeps all
  bins: number[]; // per SME bin (100 ms): its start, in seconds after the marker
  labels: LabelErp[];
};

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
// Used by: every method of api, and by send.
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

// send makes a call with a JSON body.
function send<T>(method: string, path: string, body: unknown): Promise<T> {
  return call<T>(path, { method, body: JSON.stringify(body), headers: { "Content-Type": "application/json" } });
}

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
  // responses returns the averaged response to each marker label in one band.
  responses: (id: string, band: Band) => call<Responses>(`/sessions/${enc(id)}/responses?band=${enc(band)}`),
  // erp returns the ERP of each marker label, leaving out per sensor the
  // trials over `reject` µV peak-to-peak (0 keeps every trial).
  erp: (id: string, reject: number) => call<Erp>(`/sessions/${enc(id)}/erp?reject=${reject}`),
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
  studies: () => call<Study[]>("/studies"),
  study: (id: string) => call<StudyDetail>(`/studies/${enc(id)}`),
  createStudy: (name: string) => send<{ id: string }>("POST", "/studies", { name }),
  // deleteStudy removes a study; its sessions stay, untagged.
  deleteStudy: (id: string) => call<{ deleted: string }>(`/studies/${enc(id)}`, { method: "DELETE" }),
  // setStudy files a session in a study with its participant and task, or
  // takes it out of its study when studyId is "".
  setStudy: (sessionId: string, studyId: string, participant = "", task = "") =>
    send<Session>("PUT", `/sessions/${enc(sessionId)}/study`, { studyId, participant, task }),
};

// bidsUrl is the download address of a study as a BIDS dataset (a zip). A
// plain link cannot set headers, so the token travels in the query string.
export function bidsUrl(id: string): string {
  return `${API_URL}/studies/${enc(id)}/bids.zip?token=${enc(TOKEN)}`;
}

// recordingUrl is the download address of a session's EDF file. A plain link
// cannot set headers, so the token travels in the query string.
export function recordingUrl(id: string): string {
  return `${API_URL}/sessions/${enc(id)}/recording.edf?token=${enc(TOKEN)}`;
}

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
