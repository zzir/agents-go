import React from 'react';

const { useState, useEffect, useCallback, useRef } = React;

export function useApi(fetcher, deps = []) {
  const [data, setData] = useState(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);

  const reload = useCallback(async () => {
    setLoading(true);
    try {
      const result = await fetcher();
      setData(result);
      setError(null);
    } catch (e) {
      setError(e.message);
    } finally {
      setLoading(false);
    }
  }, deps);

  useEffect(() => { reload(); }, [reload]);

  return { data, loading, error, reload };
}

// useScrollToBottom keeps a scroll container pinned to the bottom as `dep`
// changes (e.g. streaming tokens) — but only while the user is already near the
// bottom. Once they scroll up, auto-scrolling stops so they can read freely;
// it resumes when they scroll back down. A change in `resetDep` (e.g. the
// session id) force-repins, so opening a conversation starts at the latest
// message. Returns a callback ref to attach to the scroll container.
export function useScrollToBottom(dep, resetDep) {
  const elRef = useRef(null);
  const handlerRef = useRef(null);
  const stick = useRef(true); // whether to keep pinned to the bottom

  const ref = useCallback((node) => {
    if (elRef.current && handlerRef.current) {
      elRef.current.removeEventListener('scroll', handlerRef.current);
    }
    elRef.current = node;
    if (node) {
      const handler = () => {
        const dist = node.scrollHeight - node.scrollTop - node.clientHeight;
        stick.current = dist < 80; // within 80px of the bottom counts as "at bottom"
      };
      handlerRef.current = handler;
      node.addEventListener('scroll', handler, { passive: true });
      node.scrollTop = node.scrollHeight; // start pinned
    }
  }, []);

  // A new conversation re-pins to the bottom regardless of prior scroll state.
  useEffect(() => { stick.current = true; }, [resetDep]);

  useEffect(() => {
    if (elRef.current && stick.current) {
      elRef.current.scrollTop = elRef.current.scrollHeight;
    }
  }, [dep, resetDep]);

  return ref;
}
