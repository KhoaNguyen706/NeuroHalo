"use client";

// All WebSocket logic for the live session page lives in this hook.

import { useEffect, useRef, useState, type RefObject } from "react";
import { liveUrl, type Band, type LiveEvent, type LiveFrame, type LiveLinks, type LiveRaw } from "@/lib/api";

const MAX_BACKOFF_MS = 10_000;
const QUALITY_OK = 1;
const MAX_EVENTS = 200; // the list scrolls; older events are dropped
const SECONDS_PER_WINDOW = 0.5; // the processor emits one frame every 0.5 s of recording
const RAW_CAPACITY = 8192; // raw samples kept per watched sensor; 5 s is 800 at 160 Hz
const HISTORY_FRAMES = 240; // two minutes of frames kept for the chart

// One past frame for the chart: seconds into the recording, and each channel's power.
// Kept by: useLiveSession. Used by: the session page's Timeline.
export type HistoryFrame = { t: number; power: number[] };

// Where the connection stands.
// Set by: useLiveSession. Used by: the session page's top-left text.
export type LiveStatus = "connecting" | "open" | "reconnecting";

// Running quality counts for every sensor, since the page started watching.
// frames: frames seen; badFrames: frames each sensor was not OK in;
// badRun: how many frames in a row each sensor has been not OK, up to now.
// Kept by: useLiveSession. Used by: the session page (rail summary, popover).
export type QualityStats = { frames: number; badFrames: number[]; badRun: number[] };

// The newest raw samples of the watched sensors: one ring per sensor.
// `written` counts every sample ever stored, so the newest one is at
// (written - 1) % ring length.
// Filled by: useLiveSession. Drawn by: RawTraces.
export type RawBuffer = { channels: number[]; data: Float32Array[]; written: number };

// emptyRaw makes a RawBuffer for a set of channels, with nothing in it yet.
function emptyRaw(channels: number[]): RawBuffer {
  return { channels, data: channels.map(() => new Float32Array(RAW_CAPACITY)), written: 0 };
}

// What the hook gives the page.
// Returned by: useLiveSession. Used by: the session page; frameRef is passed on to HeadScene.
export type LiveSession = {
  frameRef: RefObject<LiveFrame | null>; // newest frame; read it without causing a render
  linksRef: RefObject<LiveLinks | null>; // newest links message; null while links are off
  statsRef: RefObject<QualityStats>; // read it on render; the page re-renders once per frame
  rawRef: RefObject<RawBuffer>; // raw samples of the watched sensors; empty while none are watched
  status: LiveStatus;
  hasData: boolean; // false until the first frame arrives
  elapsedSeconds: number; // how far into the recording the newest frame is
  delayMs: number | null; // Date.now() - sentTimeMs of the newest frame
  events: LiveEvent[]; // newest first
  history: HistoryFrame[]; // the last two minutes of frames in the current band, oldest first
};

// subscribeMessage is what the server expects on open and whenever the band,
// the links setting or the sensors watched as raw signal change.
function subscribeMessage(band: Band, links: boolean, raw: number[]): string {
  return JSON.stringify({ type: "subscribe", band, links, raw });
}

// useLiveSession keeps one WebSocket open for a session while `enabled`. It
// subscribes to `band` (and to correlation links when `links` is on),
// resubscribes when either changes, and reconnects with backoff when the
// socket closes.
//
// The newest frame goes into a ref, so the 3D scene can read it every
// animation frame without React re-rendering. The numbers shown as text
// (elapsed, delay) are state, which changes once per frame from the server,
// that is twice a second.
//
// `rawChannels` are the sensors to receive raw signal for (at most 8; an
// empty list turns it off). Their samples go into rawRef.
export function useLiveSession(id: string, band: Band, links: boolean, rawChannels: number[],
  enabled: boolean): LiveSession {
  const frameRef = useRef<LiveFrame | null>(null);
  const linksRef = useRef<LiveLinks | null>(null);
  const statsRef = useRef<QualityStats>({ frames: 0, badFrames: [], badRun: [] });
  const statsWindow = useRef(-1); // the last window counted, so a resent frame counts once
  const linksOnRef = useRef(links); // the socket callbacks need the current setting
  const rawRef = useRef<RawBuffer>(emptyRaw([]));
  const rawKey = rawChannels.join(","); // changes only when the watched sensors do
  const socketRef = useRef<WebSocket | null>(null);
  const bandRef = useRef(band); // the socket callbacks need the current band, not the one at connect time
  const [status, setStatus] = useState<LiveStatus>("connecting");
  const [latest, setLatest] = useState<{ windowIndex: number; delayMs: number } | null>(null);
  const [events, setEvents] = useState<LiveEvent[]>([]);
  const [history, setHistory] = useState<HistoryFrame[]>([]);

  useEffect(() => {
    if (!enabled) return;
    statsRef.current = { frames: 0, badFrames: [], badRun: [] };
    statsWindow.current = -1;
    let stopped = false; // set on unmount, so a closing socket does not reconnect
    let attempt = 0;
    let timer: ReturnType<typeof setTimeout>;

    const connect = () => {
      const socket = new WebSocket(liveUrl(id));
      socketRef.current = socket;

      socket.onopen = () => {
        attempt = 0;
        setStatus("open");
        socket.send(subscribeMessage(bandRef.current, linksOnRef.current, rawRef.current.channels));
      };

      socket.onmessage = (message) => {
        const data = JSON.parse(message.data);
        if (data.type === "frame") {
          const frame = data as LiveFrame;
          // Quality does not depend on the band, so every new window counts.
          if (frame.windowIndex > statsWindow.current) {
            statsWindow.current = frame.windowIndex;
            const st = statsRef.current;
            st.frames++;
            frame.quality.forEach((q, i) => {
              const ok = q === QUALITY_OK;
              st.badFrames[i] = (st.badFrames[i] ?? 0) + (ok ? 0 : 1);
              st.badRun[i] = ok ? 0 : (st.badRun[i] ?? 0) + 1;
            });
          }
          // A frame sent just before a band change still carries the old band.
          if (frame.band !== bandRef.current) return;
          frameRef.current = frame;
          setLatest({ windowIndex: frame.windowIndex, delayMs: Date.now() - frame.sentTimeMs });
          setHistory((list) => {
            const t = frame.windowIndex * SECONDS_PER_WINDOW;
            // A reconnect can resend a window the chart already has; keep time moving forward.
            if (list.length && t <= list[list.length - 1].t) return list;
            return [...list, { t, power: frame.power }].slice(-HISTORY_FRAMES);
          });
        } else if (data.type === "raw") {
          const raw = data as LiveRaw;
          const buf = rawRef.current;
          // A batch for the sensors watched before a change is ignored.
          if (raw.channels.join(",") !== buf.channels.join(",")) return;
          const n = raw.samples[0]?.length ?? 0;
          buf.data.forEach((ring, i) => {
            const row = raw.samples[i] ?? [];
            for (let k = 0; k < n; k++) ring[(buf.written + k) % ring.length] = row[k] ?? 0;
          });
          buf.written += n;
        } else if (data.type === "links") {
          // One sent just before links were turned off is ignored.
          if (linksOnRef.current) linksRef.current = data as LiveLinks;
        } else if (data.type === "event") {
          setEvents((list) => [data as LiveEvent, ...list].slice(0, MAX_EVENTS));
        }
      };

      socket.onclose = () => {
        if (stopped) return;
        setStatus("reconnecting");
        // 1 s, 2 s, 4 s, 8 s, then 10 s between tries.
        timer = setTimeout(connect, Math.min(1000 * 2 ** attempt, MAX_BACKOFF_MS));
        attempt++;
      };
    };

    connect();
    return () => {
      stopped = true;
      clearTimeout(timer);
      socketRef.current?.close();
    };
  }, [id, enabled]);

  // A new band starts a new chart: old frames are in the other band.
  useEffect(() => setHistory([]), [band]);

  // Tell the server when the band or links change. On first connect, onopen does it.
  // Turning links off forgets the last links message, which removes every line.
  useEffect(() => {
    bandRef.current = band;
    linksOnRef.current = links;
    if (!links) linksRef.current = null;
    const socket = socketRef.current;
    // New sensors to watch start with empty traces.
    if (rawRef.current.channels.join(",") !== rawKey) rawRef.current = emptyRaw(rawKey ? rawKey.split(",").map(Number) : []);
    if (socket?.readyState === WebSocket.OPEN) socket.send(subscribeMessage(band, links, rawRef.current.channels));
  }, [band, links, rawKey]);

  return {
    frameRef,
    linksRef,
    statsRef,
    rawRef,
    status,
    hasData: latest != null,
    elapsedSeconds: latest ? latest.windowIndex * SECONDS_PER_WINDOW : 0,
    delayMs: latest ? latest.delayMs : null,
    events,
    history,
  };
}
