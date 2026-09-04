import React, { useState, useCallback, useEffect } from 'react';
import { Flash, Button } from '@primer/react';
import { SecretInput } from '@/components/SecretInput';
import { login, authConfig, type AuthConfig } from '@/lib/api';
import { stashReturnHash } from '@/lib/route';
import './login.css';

// AUTH_ERROR_TEXT maps the callback's coarse #auth_error tags to a sentence.
const AUTH_ERROR_TEXT: Record<string, string> = {
  state_mismatch: 'The sign-in expired or was already used — try again.',
  exchange_failed: 'The provider rejected the sign-in — try again.',
  not_allowed: 'This account is not on the allowlist for this server.',
  cancelled: 'The sign-in was cancelled at the provider.',
  disabled: 'This account has been disabled by an admin.',
  rate_limited: 'Too many sign-in attempts from your address — wait a minute and try again.',
  login_failed: 'Sign-in failed on the server — try again.',
};

// exchangeErrorTag maps a failed code exchange to the login page's message:
// the server refuses a used or expired code with 401; anything else is not
// the code's fault.
export function exchangeErrorTag(e: unknown): string {
  const status = (e as { status?: number } | null)?.status;
  if (status === 401) return 'state_mismatch';
  if (status === 429) return 'rate_limited';
  return 'login_failed';
}

export function LoginPage({ onLogin, authError }: { onLogin: () => void; authError?: string }) {
  const [token, setTokenVal] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  // null while /auth/config is in flight. A failure shows as such, with a
  // retry — guessing token mode would offer a password box that an OAuth
  // server answers with 400.
  const [cfg, setCfg] = useState<AuthConfig | null>(null);
  const [cfgError, setCfgError] = useState(false);
  const [cfgAttempt, setCfgAttempt] = useState(0);

  useEffect(() => {
    let stale = false;
    setCfgError(false);
    authConfig()
      .then(c => { if (!stale) setCfg(c); })
      .catch(() => { if (!stale) setCfgError(true); });
    return () => { stale = true; };
  }, [cfgAttempt]);

  const handleSubmit = useCallback(async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');
    setLoading(true);
    try {
      await login(token);
      onLogin();
    } catch {
      setError('Invalid token');
    } finally {
      setLoading(false);
    }
  }, [token, onLogin]);

  const oauthMsg = authError ? (AUTH_ERROR_TEXT[authError] || AUTH_ERROR_TEXT.login_failed) : '';

  return (
    <div className="login-page">
      <form className="login-card" onSubmit={handleSubmit}>
        <h2 className="login-heading">Sign in</h2>
        {oauthMsg ? <Flash variant="danger">{oauthMsg}</Flash> : null}
        {cfgError ? (
          <>
            <Flash variant="danger">Couldn&apos;t load the sign-in options from the server.</Flash>
            <Button block onClick={() => setCfgAttempt(n => n + 1)}>Retry</Button>
          </>
        ) : cfg?.mode === 'oauth' ? (
          (cfg.providers || []).map(p => (
            // A full-page navigation: the flow returns via the server's
            // redirect with a one-time code in the fragment.
            <Button
              key={p} block variant="primary"
              onClick={() => { stashReturnHash(); window.location.href = `/api/v1/auth/oauth/${p}/start`; }}
            >
              Continue with {p.charAt(0).toUpperCase() + p.slice(1)}
            </Button>
          ))
        ) : cfg ? (
          <>
            <SecretInput
              aria-label="API token"
              placeholder="Token"
              block
              value={token}
              autoFocus
              loading={loading || undefined}
              onChange={(e) => setTokenVal(e.target.value)}
              validationStatus={error ? 'error' : undefined}
            />
            <Button type="submit" variant="primary" block disabled={loading || !token.trim()}>Continue</Button>
          </>
        ) : null}
      </form>
    </div>
  );
}
