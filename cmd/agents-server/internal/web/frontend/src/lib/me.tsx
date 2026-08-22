import { createContext, useCallback, useContext, useEffect, useState } from 'react';
import { api, type AuthUser } from '@/lib/api';

// ME_RELOAD asks for a fresh /auth/me. The socket raises it on reconnect: a
// close for a revoked credential or a changed role is what it reconnected from.
export const ME_RELOAD = 'auth:me-reload';

export interface MeState {
  me: AuthUser | null;
  // True until the first /auth/me answers, success or failure.
  loading: boolean;
  // The last fetch failed and nothing newer has answered; `me` may be stale.
  error: boolean;
  reload: () => void;
}

export const MeContext = createContext<MeState>({ me: null, loading: true, error: false, reload: () => {} });

// useMeLoader is the one fetch of /auth/me, at app level; `enabled` is the
// authed flag, so a sign-out drops the user.
export function useMeLoader(enabled: boolean): MeState {
  const [me, setMe] = useState<AuthUser | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const reload = useCallback(() => setAttempt(n => n + 1), []);

  useEffect(() => {
    if (!enabled) { setMe(null); setLoading(true); setError(false); return; }
    let stale = false;
    api.auth.me()
      .then(u => { if (!stale) { setMe(u); setError(false); } })
      .catch(() => { if (!stale) setError(true); })
      .finally(() => { if (!stale) setLoading(false); });
    return () => { stale = true; };
  }, [enabled, attempt]);

  useEffect(() => {
    window.addEventListener(ME_RELOAD, reload);
    return () => window.removeEventListener(ME_RELOAD, reload);
  }, [reload]);

  return { me, loading, error, reload };
}

export function useMe(): MeState {
  return useContext(MeContext);
}

// useIsAdmin is null until /auth/me has answered, so nothing shows an admin
// the member view during the load; a failed fetch counts as member (the
// server refuses the writes anyway).
export function useIsAdmin(): boolean | null {
  const { me, loading } = useMe();
  return loading ? null : me?.role === 'admin';
}
