import { getToken } from '@/lib/api';
import { EV } from '@/lib/protocol';

interface WSEnvelope {
  type: string;
  token?: string;
  payload?: unknown;
}

export class WSClient {
  ws: WebSocket | null;
  handlers: Record<string, // eslint-disable-next-line @typescript-eslint/no-explicit-any
(payload: any) => void>;
  // Fired once the socket re-authenticates after a drop (not on first connect),
  // so callers can resync runs that kept executing server-side.
  onReconnect: (() => void) | null;
  // Fired when the socket keeps closing before it can authenticate (the token is
  // rejected), so the app prompts a re-login instead of reconnecting forever.
  onAuthFail: (() => void) | null;
  // Fired with true once authenticated, false when the socket drops — the
  // app's persistent connection indicator.
  onStatus: ((connected: boolean) => void) | null;
  private _closed: boolean;
  private _retryDelay: number;
  private _reconnectTimer: ReturnType<typeof setTimeout> | null;
  private _everAuthed: boolean;
  // Consecutive connections closed before auth.ok; one pre-auth drop can be a
  // blip on a valid token, so the signal fires only past a small threshold.
  private _authFailures: number;

  constructor() {
    this.ws = null;
    this.handlers = {};
    this.onReconnect = null;
    this.onAuthFail = null;
    this.onStatus = null;
    this._closed = false;
    this._retryDelay = 1000;
    this._reconnectTimer = null;
    this._everAuthed = false;
    this._authFailures = 0;
  }

  connect(): void {
    if (this._closed) return;
    const token = getToken();
    if (!token) {
      // Not logged in yet (a first visit mounts before the token exists): poll
      // at a fixed short interval, no backoff — the socket comes up on its own
      // after login.
      this._reconnectTimer = setTimeout(() => {
        this._reconnectTimer = null;
        this.connect();
      }, 1000);
      return;
    }
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    this.ws = new WebSocket(`${proto}//${location.host}/ws`);

    let authed = false;
    // Whether the handshake completed: a close with opened === false is an
    // unreachable server, NOT a rejected token.
    let opened = false;

    this.ws.onopen = () => {
      opened = true;
      // Backoff resets only on auth.ok (below): a socket that opens, fails auth
      // and is closed must not reset it, or a rejected token reconnects every second.
      this.ws!.send(JSON.stringify({ type: EV.auth, token }));
    };

    this.ws.onmessage = (e: MessageEvent) => {
      try {
        const env: WSEnvelope = JSON.parse(e.data);
        if (!authed) {
          if (env.type === EV.authOk) {
            authed = true;
            // Authentication succeeded: this is the real success signal, so
            // reset the reconnect backoff and the auth-failure counter here.
            this._retryDelay = 1000;
            this._authFailures = 0;
            // Re-auth after a prior session means we reconnected; let the
            // caller resubscribe/resync. First-ever auth is not a reconnect.
            if (this._everAuthed) this.onReconnect?.();
            this._everAuthed = true;
            this.onStatus?.(true);
          }
          return;
        }
        const handler = this.handlers[env.type];
        if (handler) handler(env.payload);
      } catch (err) {
        console.error('ws parse error:', err);
      }
    };

    this.ws.onclose = () => {
      if (this._closed) return;
      this.onStatus?.(false);
      // Closed after opening but before auth: the server rejects a bad token by
      // silently closing. Counted and surfaced past the threshold; a close that
      // never opened is an unreachable server and does not count.
      if (opened && !authed) {
        this._authFailures++;
        if (this._authFailures >= 3) this.onAuthFail?.();
      }
      this._scheduleReconnect();
    };

    this.ws.onerror = () => {};
  }

  private _scheduleReconnect(): void {
    const delay = Math.min(this._retryDelay, 30000);
    this._retryDelay = Math.min(delay * 2, 30000);
    this._reconnectTimer = setTimeout(() => {
      this._reconnectTimer = null;
      this.connect();
    }, delay);
  }

  on(type: string, handler: // eslint-disable-next-line @typescript-eslint/no-explicit-any
(payload: any) => void): this {
    this.handlers[type] = handler;
    return this;
  }

  isConnected(): boolean {
    return this.ws?.readyState === WebSocket.OPEN;
  }

  // send transmits when the socket is open and reports whether it did: a
  // dropped socket must surface to the caller (optimistic UI rolls back), never
  // swallow a request.
  send(type: string, payload: unknown): boolean {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify({ type, payload }));
      return true;
    }
    return false;
  }

  close(): void {
    this._closed = true;
    if (this._reconnectTimer) {
      clearTimeout(this._reconnectTimer);
      this._reconnectTimer = null;
    }
    if (this.ws) {
      this.ws.onclose = null;
      this.ws.close();
      this.ws = null;
    }
  }
}
