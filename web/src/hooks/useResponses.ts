"use client";

// The averaged response to each marker label, read from the api. While the
// session is live it is asked for again every two seconds, so the average
// grows as trials finish; for an ended session it is read once more a few
// seconds later, to catch the last trials.

import { useEffect, useState } from "react";
import { api, type Band, type Responses } from "@/lib/api";

const POLL_MS = 2000;
const SETTLE_POLLS = 3; // extra reads after a session has ended

// useResponses returns the averaged responses of session `id` in `band`, or
// null until the first answer (and while `enabled` is false, when nothing is
// asked for at all).
// Used by: the session page.
export function useResponses(id: string, band: Band, enabled: boolean, live: boolean): Responses | null {
  const [responses, setResponses] = useState<Responses | null>(null);

  useEffect(() => {
    if (!enabled) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    let left = SETTLE_POLLS;
    const load = async () => {
      try {
        const found = await api.responses(id, band);
        if (!stopped) setResponses(found);
      } catch { /* the next read tries again */ }
      if (!stopped && (live || left-- > 0)) timer = setTimeout(load, POLL_MS);
    };
    load();
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  }, [id, band, enabled, live]);

  // An answer for another band is not shown while the new one loads.
  return enabled && responses?.band === band ? responses : null;
}
