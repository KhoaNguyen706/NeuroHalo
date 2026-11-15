"use client";

// Studies: the list of studies, and a field to start a new one.

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import ThemeToggle from "@/components/ThemeToggle";
import { api, type Study } from "@/lib/api";
import styles from "./studies.module.css";

// StudiesPage lists every study, newest first. Creating one opens it.
export default function StudiesPage() {
  const router = useRouter();
  const [studies, setStudies] = useState<Study[] | null>(null);
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api.studies().then(setStudies).catch((e) => {
      setStudies([]);
      setError(`Cannot reach the api: ${e.message}`);
    });
  }, []);

  // create makes the study and opens its page.
  const create = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const { id } = await api.createStudy(name.trim());
      router.push(`/studies/${id}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setBusy(false);
    }
  };

  return (
    <>
    <header className="bar">
      <Link href="/" className="brand"><span className="brand-ring" aria-hidden />NeuroHalo</Link>
      <span className="bar-end"><ThemeToggle /></span>
    </header>
    <main className={styles.page}>
      <Link href="/" className={styles.back}>← Home</Link>
      <div className={styles.head}><h1 className={styles.title}>Studies</h1></div>

      <form className={styles.form} onSubmit={create}>
        <input className={styles.grow} value={name} onChange={(e) => setName(e.target.value)}
          placeholder="New study name" maxLength={100} aria-label="New study name" />
        <button type="submit" className="solid" disabled={busy || name.trim() === ""}>Create</button>
      </form>
      {error && <p className="error" role="alert">{error}</p>}

      {studies == null ? (
        <p className="dim small empty">Loading…</p>
      ) : studies.length === 0 ? (
        <p className="dim small empty">No studies yet.</p>
      ) : (
        <ul className="rows">
          {studies.map((s) => (
            <li key={s.id}>
              <Link href={`/studies/${s.id}`} className={`row ${styles.studyRow}`}>
                <span className="row-title">{s.name}</span>
                <span className="dim small">{s.sessions === 1 ? "1 session" : `${s.sessions} sessions`}</span>
                <span className="row-end mono">
                  {new Date(s.createdAt).toLocaleDateString([], { year: "numeric", month: "short", day: "numeric" })}
                </span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </main>
    </>
  );
}
