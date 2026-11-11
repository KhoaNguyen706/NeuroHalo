"use client";

// Home: upload a recording or play a sample, and reopen or delete earlier sessions.

import dynamic from "next/dynamic";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useEffect, useRef, useState } from "react";
import ThemeToggle from "@/components/ThemeToggle";
import { api, type Sample, type Session } from "@/lib/api";
import { duration, sessionName } from "@/lib/format";

// three.js needs a browser, so the head is never rendered on the server.
const Head3D = dynamic(() => import("@/components/Head3D"), { ssr: false });

const NO_SENSORS: string[] = [];
const NO_FLAGS: boolean[] = [];
const SESSIONS_SHOWN = 8; // the list shows this many until "Show all" is pressed
const DELETE_ALL = "all"; // the value of `confirming` while "Delete all ended" waits for its second click

// HomePage holds the upload area, the samples and the session list.
export default function HomePage() {
  const router = useRouter();
  const [samples, setSamples] = useState<Sample[]>([]);
  const [sessions, setSessions] = useState<Session[] | null>(null);
  const [speed, setSpeed] = useState<"1x" | "fast">("1x");
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [file, setFile] = useState<File | null>(null);
  const [dragOver, setDragOver] = useState(false);
  const input = useRef<HTMLInputElement>(null);
  const [showAll, setShowAll] = useState(false);
  // Deleting takes two clicks: the first puts the session's ID (or DELETE_ALL) here, the second deletes.
  const [confirming, setConfirming] = useState<string | null>(null);

  useEffect(() => {
    api.samples().then(setSamples).catch((e) => setError(`Cannot reach the api: ${e.message}`));
    let alive = true;
    const load = () =>
      api.sessions().then((s) => alive && setSessions(s)).catch(() => alive && setSessions((x) => x ?? []));
    load();
    const timer = setInterval(load, 3000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, []);

  // start runs a request that creates a session, then opens that session's page.
  const start = async (what: string, run: () => Promise<{ sessionId: string }>) => {
    setBusy(what);
    setError(null);
    try {
      const { sessionId } = await run();
      router.push(`/sessions/${sessionId}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setBusy(null);
    }
  };

  // remove deletes sessions one after another and takes them off the list.
  // A session that cannot be deleted stays, and its reason is shown.
  const remove = async (ids: string[]) => {
    setConfirming(null);
    setError(null);
    for (const id of ids) {
      try {
        await api.deleteSession(id);
        setSessions((list) => list?.filter((s) => s.id !== id) ?? list);
      } catch (e) {
        setError(`Could not delete the session: ${e instanceof Error ? e.message : String(e)}`);
        return;
      }
    }
  };

  const ended = sessions?.filter((s) => s.status === "ended") ?? [];
  const shown = showAll ? sessions ?? [] : (sessions ?? []).slice(0, SESSIONS_SHOWN);

  // pick checks a chosen or dropped file before it is offered for upload.
  const pick = (f: File | undefined) => {
    if (!f) return;
    setError(null);
    if (!f.name.toLowerCase().endsWith(".edf")) return setError("Only .edf files are accepted.");
    if (f.size > 50 * 1024 * 1024) return setError("The file is larger than 50 MB.");
    setFile(f);
  };

  return (
    <>
    <header className="bar">
      <Link href="/" className="brand"><span className="brand-ring" aria-hidden />NeuroHalo</Link>
      <span className="bar-end">
        <span className="bar-note">EEG replay · research and education only, not for diagnosis</span>
        <ThemeToggle />
      </span>
    </header>
    <main className="home">
      <section className="hero">
        <div className="hero-copy">
          <h1>Replay an EEG recording.<br />Watch it on the brain.</h1>
          <p className="lede">
            Upload an EDF file or play a sample. It streams through the pipeline as if it were being recorded now:
            each rhythm band is mapped onto a 3D head twice a second, and failing sensors are flagged as they happen.
          </p>

          <div
            className={`drop${dragOver ? " over" : ""}${file ? " has-file" : ""}`}
            onDragOver={(e) => {
              e.preventDefault();
              setDragOver(true);
            }}
            onDragLeave={() => setDragOver(false)}
            onDrop={(e) => {
              e.preventDefault();
              setDragOver(false);
              pick(e.dataTransfer.files[0]);
            }}
          >
            <input ref={input} type="file" accept=".edf" hidden onChange={(e) => pick(e.target.files?.[0])} />
            <div className="drop-main">
              <span className="drop-title">{file ? file.name : "Drop an .edf file here"}</span>
              <span className="dim small">
                {file ? `${(file.size / 1024 / 1024).toFixed(1)} MB, ready to upload` : "One recording, up to 50 MB"}
              </span>
            </div>
            <div className="drop-actions">
              <button type="button" onClick={() => input.current?.click()} disabled={busy != null}>
                {file ? "Change file" : "Choose file"}
              </button>
              <button type="button" className="solid" disabled={!file || busy != null}
                onClick={() => file && start("upload", () => api.upload(file, speed))}>
                {busy === "upload" ? "Uploading…" : "Upload and play"}
              </button>
            </div>
          </div>

          <fieldset className="speed">
            <legend>Playback</legend>
            <label>
              <input type="radio" name="speed" checked={speed === "1x"} onChange={() => setSpeed("1x")} />
              <span><strong>Real time</strong><span className="dim"> as it was recorded, to watch live</span></span>
            </label>
            <label>
              <input type="radio" name="speed" checked={speed === "fast"} onChange={() => setSpeed("fast")} />
              <span><strong>Fast</strong><span className="dim"> as quickly as the pipeline allows, to review afterwards</span></span>
            </label>
          </fieldset>
          {error && <p className="error" role="alert">{error}</p>}
        </div>

        <div className="hero-view" aria-hidden>
          <Head3D ambient names={NO_SENSORS} values={null} scaleMax={1} bad={NO_FLAGS} selected={null} />
        </div>
      </section>

      <section className="lists">
        <div>
          <h2>Sample recordings</h2>
          <ul className="rows">
            {samples.map((s) => (
              <li key={s.name}>
                <button type="button" className="row" disabled={busy != null}
                  onClick={() => start(s.name, () => api.playSample(s.name, speed))}>
                  <span className="row-icon" aria-hidden>{busy === s.name ? "…" : "▶"}</span>
                  <span className="row-main">
                    <span className="row-title">{s.title}</span>
                    <span className="dim small">{s.description}</span>
                  </span>
                </button>
              </li>
            ))}
            {samples.length === 0 && <li className="dim small empty">Loading samples…</li>}
          </ul>
        </div>

        <div>
          <div className="list-head">
            <h2>Sessions</h2>
            {ended.length > 1 && (
              <button type="button" className={`text-button${confirming === DELETE_ALL ? " danger" : ""}`}
                onClick={() => (confirming === DELETE_ALL ? remove(ended.map((s) => s.id)) : setConfirming(DELETE_ALL))}
                onBlur={() => setConfirming(null)}>
                {confirming === DELETE_ALL ? `Delete ${ended.length} ended sessions?` : "Delete all ended"}
              </button>
            )}
          </div>
          {sessions == null ? (
            <p className="dim small empty">Loading…</p>
          ) : sessions.length === 0 ? (
            <p className="dim small empty">Nothing yet. Upload a recording or play a sample.</p>
          ) : (
            <>
            <ul className="rows">
              {shown.map((s) => (
                <li key={s.id} className="session">
                  <Link href={`/sessions/${s.id}`} className="row session-row">
                    <span className={`row-icon ${s.status === "live" ? "live" : "dim"}`} aria-hidden>
                      {s.status === "live" ? "●" : "○"}
                    </span>
                    <span className="row-title">{sessionName(s)}</span>
                    <span className="dim small mono">
                      {s.status === "live"
                        ? "live"
                        : new Date(s.startedAt).toLocaleString([], { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" })}
                    </span>
                    <span className="row-end mono">{duration(s.startedAt, s.endedAt)}</span>
                  </Link>
                  {/* A live session cannot be deleted: its stream is still writing. */}
                  {s.status === "ended" && (
                    <button type="button" className={`text-button delete${confirming === s.id ? " danger" : ""}`}
                      aria-label={`Delete ${sessionName(s)}`}
                      onClick={() => (confirming === s.id ? remove([s.id]) : setConfirming(s.id))}
                      onBlur={() => setConfirming(null)}>
                      {confirming === s.id ? "Delete?" : "×"}
                    </button>
                  )}
                </li>
              ))}
            </ul>
            {sessions.length > SESSIONS_SHOWN && (
              <button type="button" className="text-button more" onClick={() => setShowAll((all) => !all)}>
                {showAll ? "Show fewer" : `Show all ${sessions.length}`}
              </button>
            )}
            </>
          )}
        </div>
      </section>

      <footer className="credits dim small">
        Sample recordings: EEG Motor Movement/Imagery Dataset (Schalk et al., 2004), PhysioNet, Open Data Commons
        Attribution License v1.0. Head and brain surfaces: FreeSurfer fsaverage, via MNE-Python.
      </footer>
    </main>
    </>
  );
}
