"use client";

// The session page: the 3D head with a band strength chart under it, and a
// rail with the data quality, a band selector, toggles for signal correlation
// and the flat sensor map, the event list and the CSV exports. While the session is live, frames
// stream in over the WebSocket. Once it has ended, the page shows the stored
// 10-second averages, with a playhead on the chart to move through them.

import dynamic from "next/dynamic";
import { useParams, useRouter } from "next/navigation";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { DotPick } from "@/components/HeadScene";
import RawTraces from "@/components/RawTraces";
import SensorMap from "@/components/SensorMap";
import Timeline, { type Mark, type Series } from "@/components/Timeline";
import { BASELINE_SECONDS, useBaseline } from "@/hooks/useBaseline";
import { useLiveSession, type HistoryFrame } from "@/hooks/useLiveSession";
import {
  api, BANDS, QUALITY_NAME, type Band, type LiveEvent, type LiveFrame, type Rollup, type Session,
} from "@/lib/api";
import { exportAverages, exportEvents } from "@/lib/export";
import { change, clock, parseClock, percent, sessionName } from "@/lib/format";
import { groupChannels, REGIONS } from "@/lib/regions";
import styles from "./page.module.css";

// three.js needs a browser, so the scene is never rendered on the server.
const HeadScene = dynamic(() => import("@/components/HeadScene"), { ssr: false });

const NO_NAMES: string[] = []; // one shared empty list, so HeadScene's props stay the same between renders
const SESSION_POLL_MS = 1000;
const HISTORY_SETTLE_MS = 20_000; // how long after a session ends its stored averages may still change
const HISTORY_POLL_MS = 2000;
const STALE_AFTER_MS = 5000; // no frame for this long: check whether the session has ended
const POPOVER_WIDTH = 150;
const SECONDS_PER_WINDOW = 0.5;
const ROLLUP_SECONDS = 10;
const QUALITY_OK = 1;
const QUALITY_FLAT = 2;
// The sensors shown as raw signal when none is picked: the midline from front
// to back, then the two sides. Names the session lacks are skipped.
const RAW_DEFAULT = ["Fz", "Cz", "Pz", "Oz", "C3", "C4", "O1", "O2"];
const NO_CHANNELS: number[] = [];
const MAX_BAD_LISTED = 6; // more bad sensors than this are summed up as "+N more"

const EVENT_WORD: Record<string, string> = {
  channel_bad: "bad",
  channel_recovered: "recovered",
  alpha_rise: "Alpha rise",
  eye_movement: "Eye movement",
  muscle: "Muscle activity",
};

// Events that get a tick on the chart. Eye movements and muscle activity are
// listed in the rail only: there are several a minute, too many for ticks.
const MARK_KIND: Record<string, Mark["kind"]> = {
  channel_bad: "bad",
  channel_recovered: "recovered",
  alpha_rise: "rise",
};

// What the chart draws: one line, one line per brain region, or left and right.
const CHART_MODES = [["all", "All"], ["regions", "Regions"], ["sides", "Left / right"]] as const;
type ChartMode = (typeof CHART_MODES)[number][0];

// The accent at different brightness, so several lines can be told apart
// without a second hue. The values are per theme, in globals.css.
const SHADES = ["var(--shade-1)", "var(--shade-2)", "var(--shade-3)", "var(--shade-4)", "var(--shade-5)"];

// A clicked dot and where to draw its popover, in pixels from the stage's top-left corner.
// Set by: SessionPage's onPick. Used by: SessionPage's popover.
type Popover = { channel: number; left: number; top: number };

// An event picked in the list: its sensor is drawn larger and followed by the
// chart, and its tick runs across the chart. channel is null for whole-head events.
// Set by: the event list. Used by: SessionPage (chart, highlight).
type Focus = { key: string; channel: number | null };

// eventText is an event in 1 to 3 words: "Cz bad", "PO8 recovered", "Alpha rise".
function eventText(event: LiveEvent): string {
  const word = EVENT_WORD[event.eventType] ?? event.eventType;
  return event.channel ? `${event.channel} ${word}` : word;
}

// eventKey identifies one event in the list and on the chart.
function eventKey(event: LiveEvent): string {
  return `${event.eventType}|${event.channel}|${event.atMs}`;
}

// mean of the finite numbers, NaN when there are none.
function mean(values: number[]): number {
  let sum = 0;
  let n = 0;
  for (const v of values) if (Number.isFinite(v)) { sum += v; n++; }
  return n ? sum / n : NaN;
}

// meanOf averages power over the listed channels (all channels when the list is null).
function meanOf(power: number[], channels: number[] | null): number {
  return mean(channels ? channels.map((c) => power[c]) : power);
}

// SessionPage shows the session named in the address. The key makes a move
// to another session (after "Replay") start from a clean page, with nothing
// left over from the one before.
export default function SessionPage() {
  const { id } = useParams<{ id: string }>();
  return <SessionView key={id} id={id} />;
}

// SessionView loads the session, opens the live stream or the stored
// history, and lays out the head, the chart, the rail and the top-left text.
// Used by: SessionPage.
function SessionView({ id }: { id: string }) {
  const router = useRouter();
  // Where in the recording this session started: above 0 when it is a "replay
  // from here", null when it has no stored recording behind it.
  const [offset, setOffset] = useState<number | null>(null);
  const [replayError, setReplayError] = useState<string | null>(null);
  // The stretch to replay, as typed: "0:20" and "0:40". An empty "from" is the
  // start of the recording, an empty "to" its end. Dragging on the chart fills both in.
  const [replayFromText, setReplayFromText] = useState("");
  const [replayToText, setReplayToText] = useState("");
  const [range, setRange] = useState<[number, number] | null>(null); // the buckets marked on the chart, first and last
  const anchor = useRef<number | null>(null); // the bucket where the current drag began
  const [session, setSession] = useState<Session | null>(null);
  const [band, setBand] = useState<Band>("alpha");
  const [links, setLinks] = useState(false); // "Signal correlation" toggle
  const [map, setMap] = useState(false); // "Sensor map" toggle
  const [raw, setRaw] = useState(false); // "Raw signal" toggle
  const [chartMode, setChartMode] = useState<ChartMode>("all");
  const [popover, setPopover] = useState<Popover | null>(null);
  const [focus, setFocus] = useState<Focus | null>(null);
  const [stored, setStored] = useState<LiveEvent[]>([]); // events from the database
  const [rollups, setRollups] = useState<Rollup[] | null>(null); // history, once the session has ended
  const [cursor, setCursor] = useState<number | null>(null); // the rollup under the playhead
  const stage = useRef<HTMLDivElement>(null);
  const popoverBox = useRef<HTMLDivElement>(null);

  // GET /sessions/{id}. A session started a moment ago may not exist yet
  // (the gateway creates it when the stream opens), so keep asking.
  useEffect(() => {
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    const load = async () => {
      try {
        const found = await api.session(id);
        if (!stopped) setSession(found);
      } catch {
        if (!stopped) timer = setTimeout(load, SESSION_POLL_MS);
      }
    };
    load();
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  }, [id]);

  // The replay job behind the session, if any: where it started, and whether
  // "replay from here" is possible at all.
  useEffect(() => {
    let stopped = false;
    setOffset(null);
    api.replay(id).then((r) => !stopped && setOffset(r.startSeconds ?? 0)).catch(() => {});
    return () => { stopped = true; };
  }, [id]);

  const isLive = session?.status === "live";
  const ended = session?.status === "ended";
  const names = useMemo(() => session?.channelNames ?? NO_NAMES, [session?.id]); // eslint-disable-line react-hooks/exhaustive-deps
  const startMs = session ? Date.parse(session.startedAt) : 0;
  const bandIndex = BANDS.indexOf(band);
  const bandName = band[0].toUpperCase() + band.slice(1);
  // The sensors watched as raw signal: the highlighted one alone (it then
  // gets its spectrum too), otherwise a standard set of eight.
  const highlighted = popover?.channel ?? focus?.channel ?? null;
  const rawChannels = useMemo(() => {
    if (!raw || !isLive) return NO_CHANNELS;
    if (highlighted != null) return [highlighted];
    const standard = RAW_DEFAULT.map((n) => names.indexOf(n)).filter((i) => i >= 0);
    return standard.length ? standard : names.slice(0, 8).map((_, i) => i);
  }, [raw, isLive, highlighted, names]);
  const live = useLiveSession(id, band, links && isLive, rawChannels, isLive);

  // While live, a silence of 5 seconds may mean the recording is over: ask.
  useEffect(() => {
    if (!isLive) return;
    const timer = setTimeout(async () => {
      try {
        const now = await api.session(id);
        if (now.status !== "live") setSession(now);
      } catch { /* the next silence asks again */ }
    }, STALE_AFTER_MS);
    return () => clearTimeout(timer);
  }, [isLive, id, live.elapsedSeconds, live.status]);

  // Stored events: once when the session is found, and again when it ends.
  // They cover what happened before this page was opened.
  useEffect(() => {
    if (!session) return;
    let stopped = false;
    api.events(session.id).then((rows) => {
      if (stopped) return;
      setStored(rows.map((e) => ({
        type: "event", eventType: e.type, channel: e.channel, atMs: Date.parse(e.at), details: e.details ?? {},
      })));
    }).catch(() => {});
    return () => { stopped = true; };
  }, [session?.id, session?.status]); // eslint-disable-line react-hooks/exhaustive-deps

  // History: the stored 10-second averages, once the session has ended.
  useEffect(() => {
    if (!session || !ended) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    // The last averages are saved a few seconds after a session ends, so a
    // session that ended a moment ago is asked for again until they settle.
    const settled = Date.parse(session.endedAt ?? "") + HISTORY_SETTLE_MS;
    const load = async () => {
      let rows: Rollup[] = [];
      try {
        rows = (await api.rollups(session.id)).rollups;
      } catch { /* shown as no stored data, and tried again below */ }
      if (stopped) return;
      setRollups(rows);
      // Keep the playhead where the user put it; start at the end otherwise.
      setCursor((c) => (rows.length === 0 ? null : c != null && c < rows.length ? c : rows.length - 1));
      if (Date.now() < settled) timer = setTimeout(load, HISTORY_POLL_MS);
    };
    load();
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  }, [session?.id, ended]); // eslint-disable-line react-hooks/exhaustive-deps

  // Every event known, newest first, each one once.
  const events = useMemo(() => {
    const seen = new Set<string>();
    return [...live.events, ...stored]
      .filter((e) => !seen.has(eventKey(e)) && seen.add(eventKey(e)))
      .sort((a, b) => b.atMs - a.atMs);
  }, [live.events, stored]);

  // The frames behind the chart: live frames, or one per stored rollup.
  const frames: HistoryFrame[] = useMemo(() => {
    if (!ended) return live.history;
    return (rollups ?? []).map((r) => ({
      t: (Date.parse(r.bucketStart) - startMs) / 1000 + ROLLUP_SECONDS / 2,
      power: names.map((_, c) => r.bandPower[c * BANDS.length + bandIndex] ?? 0),
    }));
  }, [ended, live.history, rollups, names, bandIndex, startMs]);

  // In history, the head shows the rollup under the playhead, shaped like a live frame.
  const historyFrame: LiveFrame | null = useMemo(() => {
    const r = ended && rollups && cursor != null ? rollups[cursor] : null;
    if (!r) return null;
    const bad = new Set(r.badChannels);
    return {
      type: "frame",
      windowIndex: cursor!,
      sentTimeMs: 0,
      band,
      power: frames[cursor!]?.power ?? [],
      quality: names.map((n) => (bad.has(n) ? QUALITY_FLAT : QUALITY_OK)),
    };
  }, [ended, rollups, cursor, band, frames, names]);

  // The page re-renders once per frame from the server (twice a second), so
  // reading the refs here keeps the popover, the quality summary and the chart current.
  const frame = ended ? historyFrame : live.frameRef.current;
  const stats = live.statsRef.current;
  // HeadScene reads the frame to draw from this ref, live or history.
  const viewRef = useRef<LiveFrame | null>(null);
  viewRef.current = frame;

  const baseline = useBaseline(id, names, session?.startedAt ?? null, ended ? Infinity : live.elapsedSeconds, offset ?? 0, ended);

  // A dot was clicked: open the popover next to the click, kept inside the stage.
  const onPick = useCallback((pick: DotPick) => {
    const box = stage.current?.getBoundingClientRect();
    if (!box) return;
    setFocus(null);
    setPopover({
      channel: pick.channel,
      left: Math.min(pick.clientX - box.left + 12, box.width - POPOVER_WIDTH - 8),
      top: Math.max(8, pick.clientY - box.top - 12),
    });
  }, []);
  const closePopover = useCallback(() => setPopover(null), []);
  // A click on the canvas away from any dot clears the popover and the picked event.
  const onMiss = useCallback(() => {
    setPopover(null);
    setFocus(null);
  }, []);

  // "Click elsewhere to close": any press outside the popover closes it. This
  // runs on pointer-down, before the click that may open a popover for another dot.
  useEffect(() => {
    if (!popover) return;
    const onDown = (e: PointerEvent) => {
      if (!popoverBox.current?.contains(e.target as Node)) setPopover(null);
    };
    window.addEventListener("pointerdown", onDown);
    return () => window.removeEventListener("pointerdown", onDown);
  }, [popover]);

  // The highlighted sensor: the open popover's, else the picked event's.
  // HeadScene reads it from a ref, so changing it does not re-render the scene.
  const highlight = popover?.channel ?? focus?.channel ?? null;
  const highlightRef = useRef<number | null>(null);
  useEffect(() => { highlightRef.current = highlight; }, [highlight]);

  // Data quality right now. Live: from the newest frame, with how long each
  // bad sensor has been bad. History: the sensors bad for most of the bucket.
  const badNow = frame
    ? frame.quality.flatMap((q, i) => (q === QUALITY_OK ? [] : [{
        channel: i,
        text: ended
          ? `${names[i]} bad`
          : `${names[i]} ${QUALITY_NAME[q] ?? "bad"} ${clock((stats.badRun[i] ?? 0) * SECONDS_PER_WINDOW)}`,
      }]))
    : [];

  // badShare is the share of the time a sensor was bad: of the frames watched
  // while live, of the stored buckets in history.
  const badShare = (c: number): number => {
    if (ended) {
      if (!rollups?.length) return 0;
      return rollups.filter((r) => r.badChannels.includes(names[c])).length / rollups.length;
    }
    return stats.frames ? (stats.badFrames[c] ?? 0) / stats.frames : 0;
  };

  // The chart's lines. A highlighted sensor always gets the chart to itself.
  const groups = useMemo(() => groupChannels(names), [names]);
  const lines: Series[] = useMemo(() => {
    const line = (key: string, label: string, shade: string, channels: number[] | null): Series => ({
      key, label, shade, points: frames.map((f) => ({ t: f.t, v: meanOf(f.power, channels) })),
    });
    if (highlight != null) return [line("sensor", names[highlight] ?? "", SHADES[0], [highlight])];
    if (chartMode === "regions") {
      return REGIONS.filter((r) => groups.regions[r].length > 0)
        .map((r, i) => line(r, r, SHADES[i % SHADES.length], groups.regions[r]));
    }
    if (chartMode === "sides" && groups.left.length && groups.right.length) {
      return [line("left", "Left", SHADES[0], groups.left), line("right", "Right", SHADES[1], groups.right)];
    }
    return [line("all", "All", SHADES[0], null)];
  }, [frames, highlight, chartMode, groups, names]);

  // When the baseline becomes known: 30 s after the first full bucket of this session.
  const baselineAt = Math.ceil((offset ?? 0) / ROLLUP_SECONDS) * ROLLUP_SECONDS + BASELINE_SECONDS;

  // The chart's title: what is drawn, and how it compares.
  const single = lines.length === 1;
  const baselineValue = !single || !baseline
    ? null
    : highlight != null ? baseline.value(highlight, bandIndex) : mean(names.map((_, c) => baseline.value(c, bandIndex)));
  const shownIndex = ended ? cursor : frames.length - 1; // the point the title's numbers describe
  const at = (l: Series) => (shownIndex != null && shownIndex >= 0 ? l.points[shownIndex]?.v : undefined);
  let chartTitle: string;
  if (single) {
    const v = at(lines[0]);
    const versus = !baseline
      ? `baseline at ${clock(baselineAt)}`
      : v != null && baselineValue != null && Number.isFinite(baselineValue) && baselineValue > 0
        ? `${change(v / baselineValue)} vs baseline`
        : "no baseline";
    chartTitle = `${bandName} · ${highlight != null ? names[highlight] : "average of all sensors"} · ${versus}`;
  } else if (chartMode === "sides") {
    const l = at(lines[0]);
    const r = at(lines[1]);
    chartTitle = `${bandName} · left and right${l && r != null ? ` · right ${change(r / l)} vs left` : ""}`;
  } else {
    chartTitle = `${bandName} · by region`;
  }

  const marks: Mark[] = useMemo(
    () => events
      .filter((e) => MARK_KIND[e.eventType])
      .map((e) => ({ key: eventKey(e), t: (e.atMs - startMs) / 1000, kind: MARK_KIND[e.eventType] })),
    [events, startMs],
  );

  // pickTime moves the playhead to the stored bucket nearest a time (history
  // only). A press marks that one bucket; dragging on from it marks every
  // bucket in between. The marked stretch fills in the replay's "from" and
  // "to": one bucket means "from here to the end".
  const pickTime = (t: number, dragging = false) => {
    if (!ended || !frames.length) return;
    let best = 0;
    frames.forEach((f, i) => { if (Math.abs(f.t - t) < Math.abs(frames[best].t - t)) best = i; });
    setCursor(best);
    if (!dragging || anchor.current == null) anchor.current = best;
    const first = Math.min(anchor.current, best);
    const last = Math.max(anchor.current, best);
    setRange([first, last]);
    setReplayFromText(clock(Math.max(0, frames[first].t - ROLLUP_SECONDS / 2)));
    setReplayToText(last > first ? clock(frames[last].t + ROLLUP_SECONDS / 2) : "");
  };
  // The marked stretch in seconds, for the chart's shading. One bucket is just the playhead.
  const rangeT: [number, number] | null = ended && range && range[1] > range[0] && frames[range[1]]
    ? [frames[range[0]].t - ROLLUP_SECONDS / 2, frames[range[1]].t + ROLLUP_SECONDS / 2]
    : null;
  const cursorT = ended && cursor != null && frames[cursor] ? frames[cursor].t : null;

  // Replay: stream the typed stretch of the recording again, as a new live
  // session, and open it.
  const replayPart = async () => {
    const from = replayFromText.trim() ? parseClock(replayFromText) : 0;
    const to = replayToText.trim() ? parseClock(replayToText) : 0;
    if (from == null || to == null) return setReplayError("Times look like 0:20 or 20.");
    if (to > 0 && to - from < 2) return setReplayError("The end must be at least 2 seconds after the start.");
    setReplayError(null);
    try {
      const { sessionId } = await api.replayFrom(id, from, to);
      router.push(`/sessions/${sessionId}`);
    } catch (e) {
      setReplayError(e instanceof Error ? e.message : String(e));
    }
  };

  // The popover's comparison with the baseline, for its sensor in this band.
  const popoverVersus = (c: number) => {
    if (!baseline) return `baseline at ${clock(baselineAt)}`;
    const b = baseline.value(c, bandIndex);
    const v = frame?.power[c];
    if (!Number.isFinite(b) || b <= 0 || v == null) return "no baseline";
    return `${change(v / b)} vs baseline`;
  };

  return (
    <main className={styles.page}>
      <div className={styles.main}>
      <div ref={stage} className={styles.stage}>
        <HeadScene names={names} frameRef={viewRef} linksRef={live.linksRef} highlightRef={highlightRef}
          onPick={onPick} onMiss={onMiss} />

        <div className={styles.info}>
          <span>{session ? sessionName(session) : id}</span>
          {ended ? (
            <>
              <span>Ended</span>
              {rollups == null ? null : cursorT == null ? (
                <span>No stored data</span>
              ) : (
                <span>{clock(cursorT - ROLLUP_SECONDS / 2)}–{clock(cursorT + ROLLUP_SECONDS / 2)}</span>
              )}
            </>
          ) : live.status === "reconnecting" ? (
            <span>Reconnecting…</span>
          ) : !live.hasData ? (
            <span>Waiting for data</span>
          ) : (
            <>
              <span>{clock(live.elapsedSeconds)}</span>
              <span>{Math.round(live.delayMs ?? 0)} ms</span>
            </>
          )}
        </div>

        {map && (
          <SensorMap
            names={names}
            frame={frame}
            highlight={highlight}
            onPick={(c) => {
              setPopover(null);
              setFocus(focus?.key === `sensor|${c}` ? null : { key: `sensor|${c}`, channel: c });
            }}
          />
        )}

        {popover && (
          <div ref={popoverBox} className={styles.popover} style={{ left: popover.left, top: popover.top }}>
            <button type="button" className={styles.close} aria-label="Close" onClick={closePopover}>×</button>
            <span className={styles.channel}>{names[popover.channel]}</span>
            <span>{percent(frame?.power[popover.channel])}</span>
            <span>{popoverVersus(popover.channel)}</span>
            <span>{frame ? QUALITY_NAME[frame.quality[popover.channel]] ?? "unknown" : "no data"}</span>
            {badShare(popover.channel) > 0 && (
              <span className={styles.bad}>bad {percent(badShare(popover.channel))} of the time</span>
            )}
          </div>
        )}
      </div>
      <Timeline
        title={chartTitle}
        controls={
          <div className={styles.modes} role="group" aria-label="Chart lines">
            {CHART_MODES.map(([mode, label]) => (
              <button key={mode} type="button" aria-pressed={chartMode === mode} onClick={() => setChartMode(mode)}>
                {label}
              </button>
            ))}
          </div>
        }
        lines={lines}
        baseline={baselineValue}
        marks={marks}
        focusKey={focus?.key ?? null}
        cursorT={cursorT}
        range={rangeT}
        onPick={ended ? pickTime : undefined}
      />
      {raw && isLive && session && (
        <RawTraces rawRef={live.rawRef} frameRef={viewRef} names={names} rate={session.sampleRateHz} />
      )}
      </div>

      <aside className={styles.rail}>
        {frame && (
          <div className={styles.quality}>
            <span>{names.length - badNow.length} of {names.length} sensors OK</span>
            {badNow.length > 0 && (
              <span className={styles.bad}>
                {badNow.slice(0, MAX_BAD_LISTED).map((b) => b.text).join(" · ")}
                {badNow.length > MAX_BAD_LISTED && ` · +${badNow.length - MAX_BAD_LISTED} more`}
              </span>
            )}
          </div>
        )}

        <div className={styles.bands} role="group" aria-label="Band">
          {BANDS.map((b) => (
            <button key={b} type="button" aria-pressed={band === b} onClick={() => setBand(b)}>
              {b[0].toUpperCase() + b.slice(1)}
            </button>
          ))}
        </div>

        {/* Links are computed on the live stream only, so the toggle is off for an ended session. */}
        <button type="button" className={styles.toggle} aria-pressed={links && isLive} disabled={ended}
          onClick={() => setLinks((on) => !on)}>
          Signal correlation
        </button>

        {/* Raw samples are not stored, so the raw signal exists only while a session is live. To see it for a past moment, replay that stretch. */}
        <button type="button" className={styles.toggle} aria-pressed={raw && isLive} disabled={ended}
          onClick={() => setRaw((on) => !on)}>
          Raw signal
        </button>

        <button type="button" className={styles.toggle} aria-pressed={map} onClick={() => setMap((on) => !on)}>
          Sensor map
        </button>

        {/* Only a session with a stored recording behind it can be replayed. */}
        {ended && offset != null && (
          <div className={styles.quality}>
            <form className={styles.replay} onSubmit={(e) => { e.preventDefault(); replayPart(); }}>
              <span>Replay from</span>
              <input aria-label="Replay from" placeholder="0:00" value={replayFromText}
                onChange={(e) => setReplayFromText(e.target.value)} />
              <span>to</span>
              <input aria-label="Replay to" placeholder="end" value={replayToText}
                onChange={(e) => setReplayToText(e.target.value)} />
              <button type="submit">Play</button>
            </form>
            {replayError && <span className={styles.bad}>{replayError}</span>}
          </div>
        )}

        <ol className={styles.events} aria-label="Events">
          {events.map((event) => {
            const key = eventKey(event);
            const channel = event.channel ? names.indexOf(event.channel) : -1;
            return (
              <li key={key}>
                <button
                  type="button"
                  aria-pressed={focus?.key === key}
                  onClick={() => {
                    setPopover(null);
                    if (focus?.key === key) return setFocus(null);
                    setFocus({ key, channel: channel >= 0 ? channel : null });
                    pickTime((event.atMs - startMs) / 1000);
                  }}
                >
                  <span className={styles.time}>{clock((event.atMs - startMs) / 1000)}</span>
                  <span>{eventText(event)}</span>
                </button>
              </li>
            );
          })}
        </ol>

        {session && (
          <div className={styles.exports}>
            <button type="button" onClick={() => exportAverages(session).catch(() => {})}>Averages CSV</button>
            <button type="button" onClick={() => exportEvents(session).catch(() => {})}>Events CSV</button>
          </div>
        )}
      </aside>
    </main>
  );
}
