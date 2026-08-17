import { useCallback, useEffect, useRef, useState } from 'react';

// useDecisionHold keeps a decision's buttons held for a moment after a click:
// approve and reject are one-way sends with no reply to await, and a second
// click would send a second decision the server refuses (a toast that reads
// as a failure). Keyed by the pending call, so a NEW pause on the same task
// is not held by the last one's timer. The paused shape leaves the screen as
// soon as the task moves on anyway.
export function useDecisionHold(holdMs = 3000): { held: (callId: string) => boolean; decide: (callId: string, send: () => void) => void } {
  const [held, setHeld] = useState<Set<string>>(() => new Set());
  const timers = useRef<number[]>([]);
  useEffect(() => () => { for (const t of timers.current) clearTimeout(t); }, []);
  const decide = useCallback((callId: string, send: () => void) => {
    setHeld(prev => new Set(prev).add(callId));
    send();
    timers.current.push(window.setTimeout(() => {
      setHeld(prev => { const next = new Set(prev); next.delete(callId); return next; });
    }, holdMs));
  }, [holdMs]);
  return { held: useCallback((callId: string) => held.has(callId), [held]), decide };
}
