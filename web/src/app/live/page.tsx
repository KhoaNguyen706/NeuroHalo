"use client";

// Live: every session being recorded right now, one row each, so one person
// can watch several rooms and see which recording needs attention.

import Link from "next/link";
import { useEffect, useState } from "react";
import ThemeToggle from "@/components/ThemeToggle";
import { api, type LiveRow } from "@/lib/api";
import { clock, sessionName } from "@/lib/format";
import styles from "../studies/studies.module.css";

const POLL_MS = 2000;
const RATE_AFTER_SECONDS = 30; // per-minute rates are shown once this much signal is in

// perMinute formats a count as a rate over the signal so far, or "–" early on.
function perMinute(count: number, seconds: number): string {
  return seconds < RATE_AFTER_SECONDS ? "–" : (count / (seconds / 60)).toFixed(1);
}

// dataState says whether a session's data is arriving.
function dataState(r: LiveRow): string {
  if (r.lastFrameMs < 0) return "waiting";
  if (r.stalled) return `stopped ${Math.round(r.lastFrameMs / 1000)} s`;
  return "arriving";
}

// LivePage reads the overview every two seconds and shows it as a table.
export default function LivePage() {
  const [rows, setRows] = useState<LiveRow[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    const load = () =>
      api.live()
        .then((r) => alive && (setRows(r), setError(null)))
        .catch((e) => alive && setError(`Cannot reach the api: ${e.message}`));
    load();
    const timer = setInterval(load, POLL_MS);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, []);

  return (
    <>
    <header className="bar">
      <Link href="/" className="brand"><span className="brand-ring" aria-hidden />NeuroHalo</Link>
      <span className="bar-end">
        <Link href="/studies" className="bar-link">Studies</Link>
        <ThemeToggle />
      </span>
    </header>
    <main className={styles.page}>
      <Link href="/" className={styles.back}>← Home</Link>
      <div className={styles.head}><h1 className={styles.title}>Live</h1></div>
      {error && <p className="error" role="alert">{error}</p>}

      {rows == null ? (
        <p className="dim small empty">Loading…</p>
      ) : rows.length === 0 ? (
        <p className="dim small empty">Nothing is being recorded.</p>
      ) : (
        <div className={styles.scroll}>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>Session</th><th>Participant</th><th>Time</th><th>Data</th><th>Bad sensors</th>
                <th>Trials</th><th>Eye /min</th><th>Muscle /min</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.session.id}>
                  <td className={styles.name}><Link href={`/sessions/${r.session.id}`}>{sessionName(r.session)}</Link></td>
                  <td className="mono">
                    {r.session.participant ? `${r.session.participant} · ${r.session.task}` : <span className="dim">–</span>}
                  </td>
                  <td className="mono">{clock(r.seconds)}</td>
                  <td className={r.stalled ? "error" : "dim"}>{dataState(r)}</td>
                  <td className={r.bad.length ? "error" : "dim"}>
                    {r.bad.length ? `${r.bad.length} of ${r.session.channelNames.length}: ${r.bad.join(", ")}` : "none"}
                  </td>
                  <td className="mono">{r.trials}</td>
                  <td className="mono">{perMinute(r.eye, r.seconds)}</td>
                  <td className="mono">{perMinute(r.muscle, r.seconds)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </main>
    </>
  );
}
