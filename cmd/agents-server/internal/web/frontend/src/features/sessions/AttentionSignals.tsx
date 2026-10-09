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

// AttentionSignals tells a person who is not looking that a session needs
// them: the page title's count, a spoken line and the opt-in desktop
// notification, read from the sidebar's list and session.status (invariant 86).
export function AttentionSignals({ announced }: { announced: Record<string, SessionStatus> }): ReactElement {
  const { data: rows, reload } = useApi(() => api.sessions.list() as Promise<Row[]>, [], 'sessions');

  const waiting = countWaiting(rows, announced);
  useEffect(() => { document.title = waitingTitle(waiting); }, [waiting]);
  useEffect(() => () => { document.title = waitingTitle(0); }, []);

  // A status announced for a conversation the list does not hold (made in
  // another tab): relist once, so it is counted and named.
  const askedRef = useRef<Set<string>>(new Set());
  useEffect(() => {
    if (!rows) return;
    const unknown = Object.keys(announced).filter(id => !askedRef.current.has(id) && !rows.some(r => r.id === id));
    if (unknown.length === 0) return;
    for (const id of unknown) askedRef.current.add(id);
    reload();
  }, [announced, rows, reload]);

  // What each row showed last (the announced status over the row's own); the
  // first list is never news, a row entering requires_action or failed after it is.
  const lastRef = useRef<Record<string, SessionStatus> | null>(null);
  const [spoken, setSpoken] = useState('');
  useEffect(() => {
    if (!rows) return;
    const now: Record<string, SessionStatus> = {};
    for (const r of rows) {
      const st = announced[r.id] ?? r.status;
      if (st) now[r.id] = st;
    }
    const last = lastRef.current;
    lastRef.current = now;
    if (!last) return;
    let moved = false;
    let said = '';
    for (const r of rows) {
      const next = now[r.id];
      if (!next || last[r.id] === next) continue;
      moved = true;
      const message = attentionMessage(r.name, last[r.id], next);
      if (!message) continue;
      said = message;
      notifyAttention(message, r.id);
    }
    // Emptied when conversations move on with nothing to say, so the same
    // line is spoken again the next time it applies.
    if (moved) setSpoken(said);
  }, [announced, rows]);

  return <div className="sr-only" role="status" aria-live="polite">{spoken}</div>;
}
