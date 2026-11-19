"use client";

// Reads something from the api that grows while a session is live, such as
// the averaged responses or the ERP. While the session is live it is asked
// for again every two seconds, so the average grows as trials finish; for an
// ended session it is read a few more times, to catch the last trials.

import { useEffect, useState } from "react";

const POLL_MS = 2000;
const SETTLE_POLLS = 3; // extra reads after a session has ended

// usePoll returns the latest answer of load() for `key`, or null until the
// first one arrives. A null key means "nothing to read": nothing is asked
// for. A new key starts over, and an answer for an earlier key is never
// returned. load must change exactly when key does.
// Used by: the session page (averaged response and ERP).
export function usePoll<T>(key: string | null, load: () => Promise<T>, live: boolean): T | null {
  const [answer, setAnswer] = useState<{ key: string; value: T } | null>(null);

  useEffect(() => {
    if (key == null) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    let left = SETTLE_POLLS;
    const read = async () => {
      try {
        const value = await load();
        if (!stopped) setAnswer({ key, value });
      } catch { /* the next read tries again */ }
      if (!stopped && (live || left-- > 0)) timer = setTimeout(read, POLL_MS);
    };
    read();
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  }, [key, live]); // eslint-disable-line react-hooks/exhaustive-deps -- load changes with key

  return key != null && answer?.key === key ? answer.value : null;
}
