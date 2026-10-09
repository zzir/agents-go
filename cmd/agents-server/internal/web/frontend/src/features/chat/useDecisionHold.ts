import { useCallback, useEffect, useRef, useState } from 'react';

// useDecisionHold holds a decision's buttons for a moment after a click (approve
// and reject are one-way sends; a second one is refused), keyed by the pending call.
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
