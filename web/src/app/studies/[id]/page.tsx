"use client";

// One study: its sessions with participant and task, adding and removing
// sessions, and the BIDS download.

import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useCallback, useEffect, useState } from "react";
import ThemeToggle from "@/components/ThemeToggle";
import { api, BIDS_LABEL, bidsUrl, type Session, type StudyDetail } from "@/lib/api";
import { clock, duration, sessionName } from "@/lib/format";
import styles from "../studies.module.css";

// bidsNames gives each session the file name it gets in the BIDS download,
// e.g. "sub-01_task-fists_run-2", or null when its recording is not ready and
// it will be left out. Runs are numbered the way the api numbers them: only
// when a participant has more than one exported session of a task, in the
// order they started.
function bidsNames(sessions: Session[]): Map<string, string | null> {
  const groups = new Map<string, Session[]>();
  for (const s of sessions) {
    if (s.recording?.status !== "ready") continue;
    const key = `${s.participant}\t${s.task}`;
    groups.set(key, [...(groups.get(key) ?? []), s]);
  }
  const names = new Map<string, string | null>(sessions.map((s) => [s.id, null]));
  for (const group of groups.values()) {
    group.sort((a, b) => Date.parse(a.startedAt) - Date.parse(b.startedAt));
    group.forEach((s, i) => {
      const run = group.length > 1 ? `_run-${i + 1}` : "";
      names.set(s.id, `sub-${s.participant}_task-${s.task}${run}`);
    });
  }
  return names;
}

// lengthText is how much signal a session holds: the recorded file's length
// once it is written, the time since it started while it is live. (A fast
// replay ends in seconds of wall time but still holds the whole recording.)
function lengthText(s: Session) {
  if (s.status === "live") return <span className="live">live</span>;
  return s.recording?.status === "ready" ? clock(s.recording.seconds) : duration(s.startedAt, s.endedAt);
}

// recordingText says where a session's EDF file stands.
function recordingText(s: Session): string {
  const r = s.recording;
  if (!r) return s.status === "live" ? "starting" : "none";
  if (r.status === "ready") return `${(r.bytes / 1024 / 1024).toFixed(1)} MB`;
  return r.status;
}

// StudyPage shows one study. While any of its sessions is still live or its
// file is being written, it reloads every 3 seconds.
export default function StudyPage() {
  const { id } = useParams<{ id: string }>();
  const router = useRouter();
  const [study, setStudy] = useState<StudyDetail | null>(null);
  const [others, setOthers] = useState<Session[]>([]); // sessions in no study, to add
  const [error, setError] = useState<string | null>(null);
  const [pick, setPick] = useState("");
  const [participant, setParticipant] = useState("");
  const [task, setTask] = useState("");
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState<string | null>(null); // a session ID, or "study"

  // load reads the study and the sessions that could be added to it.
  const load = useCallback(async () => {
    try {
      const [s, all] = await Promise.all([api.study(id), api.sessions()]);
      setStudy(s);
      setOthers(all.filter((x) => x.studyId === ""));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, [id]);

  const pending = study?.sessionList.some((s) => s.recording?.status !== "ready" && s.recording?.status !== "failed");
  useEffect(() => {
    load();
    if (!pending) return;
    const timer = setInterval(load, 3000);
    return () => clearInterval(timer);
  }, [load, pending]);

  // add files the picked session in this study. Participant and task stay
  // filled, so the next run of the same person is one click.
  const add = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!BIDS_LABEL.test(participant) || !BIDS_LABEL.test(task)) {
      setError("Participant and task: letters and digits only. They become file names.");
      return;
    }
    setBusy(true);
    setError(null);
    try {
      await api.setStudy(pick, id, participant, task);
      setPick("");
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
    setBusy(false);
  };

  // remove takes a session out of the study; the session itself stays.
  const remove = async (sessionId: string) => {
    setConfirming(null);
    try {
      await api.setStudy(sessionId, "");
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  // deleteStudy removes the study and goes back to the list.
  const deleteStudy = async () => {
    try {
      await api.deleteStudy(id);
      router.push("/studies");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const sessions = study?.sessionList ?? [];
  const names = bidsNames(sessions);
  const ready = sessions.filter((s) => names.get(s.id) != null).length;
  const people = new Set(sessions.map((s) => s.participant)).size;

  return (
    <>
    <header className="bar">
      <Link href="/" className="brand"><span className="brand-ring" aria-hidden />NeuroHalo</Link>
      <span className="bar-end"><ThemeToggle /></span>
    </header>
    <main className={styles.page}>
      <Link href="/studies" className={styles.back}>← Studies</Link>
      <div className={styles.head}>
        <h1 className={styles.title}>{study?.name ?? "…"}</h1>
        <span className={styles.actions}>
          <button type="button" className={`text-button${confirming === "study" ? " danger" : ""}`}
            onClick={() => (confirming === "study" ? deleteStudy() : setConfirming("study"))}
            onBlur={() => setConfirming(null)}>
            {confirming === "study" ? "Delete the study? Sessions stay" : "Delete study"}
          </button>
          <a className={styles.download} href={bidsUrl(id)} aria-disabled={ready === 0}>Download BIDS</a>
        </span>
      </div>
      <p className="dim small">
        {sessions.length === 1 ? "1 session" : `${sessions.length} sessions`}
        {" · "}{people === 1 ? "1 participant" : `${people} participants`}
        {ready < sessions.length && ` · ${sessions.length - ready} without a finished recording, left out of the download`}
      </p>
      {error && <p className="error" role="alert">{error}</p>}

      <section className={styles.section}>
        <h2>Sessions</h2>
        {study && sessions.length === 0 ? (
          <p className="dim small empty">None yet. Add one below.</p>
        ) : (
          <div className={styles.scroll}>
            <table className={styles.table}>
              <thead>
                <tr><th>Participant</th><th>Task</th><th>File name</th><th>Session</th><th>Length</th><th>Recording</th><th /></tr>
              </thead>
              <tbody>
                {sessions.map((s) => (
                  <tr key={s.id}>
                    <td className="mono">{s.participant}</td>
                    <td className="mono">{s.task}</td>
                    <td className={styles.file}>{names.get(s.id) ?? <span className="dim">–</span>}</td>
                    <td className={styles.name}><Link href={`/sessions/${s.id}`}>{sessionName(s)}</Link></td>
                    <td className="mono">{lengthText(s)}</td>
                    <td className="mono dim">{recordingText(s)}</td>
                    <td>
                      <button type="button" className={`text-button${confirming === s.id ? " danger" : ""}`}
                        onClick={() => (confirming === s.id ? remove(s.id) : setConfirming(s.id))}
                        onBlur={() => setConfirming(null)}>
                        {confirming === s.id ? "Remove?" : "Remove"}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className={styles.section}>
        <h2>Add a session</h2>
        <form className={styles.form} onSubmit={add}>
          <select className={styles.grow} value={pick} onChange={(e) => setPick(e.target.value)} aria-label="Session">
            <option value="">{others.length ? "Choose a session" : "No session outside a study"}</option>
            {others.map((s) => (
              <option key={s.id} value={s.id}>
                {s.status === "live" ? "● " : ""}{sessionName(s)} ·{" "}
                {new Date(s.startedAt).toLocaleString([], { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" })}
              </option>
            ))}
          </select>
          <input className={styles.label} value={participant} onChange={(e) => setParticipant(e.target.value)}
            placeholder="Participant" maxLength={32} aria-label="Participant" />
          <input className={styles.label} value={task} onChange={(e) => setTask(e.target.value)}
            placeholder="Task" maxLength={32} aria-label="Task" />
          <button type="submit" disabled={busy || !pick || !participant || !task}>Add</button>
        </form>
      </section>
    </main>
    </>
  );
}
