"use client";

// SummaryPanel: the session's written summary under the charts. It shows a
// stored summary at once (free) and writes one only when asked, since that
// is a Claude API call.

import { useEffect, useState } from "react";
import { api, ApiError, type Summary } from "@/lib/api";
import styles from "./SummaryPanel.module.css";

type State =
  | { kind: "loading" }
  | { kind: "none" } // nothing stored yet
  | { kind: "writing" }
  | { kind: "done"; summary: Summary }
  | { kind: "error"; message: string };

// SummaryPanel reads the stored summary of sessionId and offers to write one.
// Used by: the session page (the "Summary" toggle).
export default function SummaryPanel({ sessionId }: { sessionId: string }) {
  const [state, setState] = useState<State>({ kind: "loading" });

  useEffect(() => {
    let alive = true;
    api.summary(sessionId)
      .then((summary) => alive && setState({ kind: "done", summary }))
      .catch((e) => {
        if (!alive) return;
        if (e instanceof ApiError && e.status === 404) setState({ kind: "none" });
        else setState({ kind: "error", message: e instanceof Error ? e.message : String(e) });
      });
    return () => {
      alive = false;
    };
  }, [sessionId]);

  // write asks Claude for a new summary.
  const write = async () => {
    const fresh = state.kind === "done";
    setState({ kind: "writing" });
    try {
      setState({ kind: "done", summary: await api.writeSummary(sessionId, fresh) });
    } catch (e) {
      setState({ kind: "error", message: e instanceof Error ? e.message : String(e) });
    }
  };

  const summary = state.kind === "done" ? state.summary : null;
  return (
    <section className={styles.panel} aria-label="Summary">
      <div className={styles.head}>
        <span className={styles.title}>
          Summary
          {summary && ` · ${summary.model}${summary.final ? "" : " · while recording, not kept"}`}
        </span>
        {state.kind !== "loading" && state.kind !== "writing" && (
          <button type="button" className={styles.write} onClick={write}>
            {summary ? "Write again" : "Write"}
          </button>
        )}
      </div>
      {state.kind === "loading" && <p className={styles.note}>Loading…</p>}
      {state.kind === "none" && <p className={styles.note}>No summary yet.</p>}
      {state.kind === "writing" && <p className={styles.note}>Writing…</p>}
      {state.kind === "error" && <p className={styles.error} role="alert">{state.message}</p>}
      {summary && <p className={styles.body}>{summary.body}</p>}
    </section>
  );
}
