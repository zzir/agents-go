import { useEffect, useRef, useState, type ReactElement } from 'react';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { attentionMessage, countWaiting, notifyAttention, waitingTitle } from '@/lib/attention';
import type { SessionStatus } from '@/lib/protocol';

interface Row {
  id: string;
  name: string;
  status?: SessionStatus;
}

// AttentionSignals tells a person who is not looking that a conversation
// needs them: the page title counts the ones waiting on a decision, and a
// conversation that starts to wait, or fails, is spoken to a screen reader
// and — when asked for — raised as a desktop notification. It reads the
// sidebar's own list and what session.status announced since (invariant 86).
export function AttentionSignals({ announced }: { announced: Record<string, SessionStatus> }): ReactElement {
  const { data: rows } = useApi(() => api.sessions.list() as Promise<Row[]>, [], 'sessions');

  const waiting = countWaiting(rows, announced);
  useEffect(() => { document.title = waitingTitle(waiting); }, [waiting]);
  useEffect(() => () => { document.title = waitingTitle(0); }, []);

  // What the last announcement said per conversation; one not yet announced
  // is compared against its list row, so a status it already had at load is
  // never news.
  const lastRef = useRef<Record<string, SessionStatus>>({});
  const [spoken, setSpoken] = useState('');
  useEffect(() => {
    const last = lastRef.current;
    lastRef.current = announced;
    let moved = false;
    let said = '';
    for (const [id, next] of Object.entries(announced)) {
      if (last[id] === next) continue;
      moved = true;
      const row = rows?.find(r => r.id === id);
      const message = attentionMessage(row?.name || '', last[id] ?? row?.status, next);
      if (!message) continue;
      said = message;
      notifyAttention(message, id);
    }
    // Emptied when conversations move on with nothing to say, so the same
    // line is spoken again the next time it applies.
    if (moved) setSpoken(said);
  }, [announced, rows]);

  return <div className="sr-only" role="status" aria-live="polite">{spoken}</div>;
}
