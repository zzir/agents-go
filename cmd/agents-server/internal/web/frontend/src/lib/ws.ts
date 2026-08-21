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
  // Fired when the socket keeps closing before it can authenticate — i.e. the
  // token is being rejected. Lets the app prompt a re-login instead of silently
  // reconnecting forever.
  onAuthFail: (() => void) | null;
  // Fired with true once authenticated, false when the socket drops — the
  // app's persistent connection indicator.
  onStatus: ((connected: boolean) => void) | null;
  private _closed: boolean;
  private _retryDelay: number;
  private _reconnectTimer: ReturnType<typeof setTimeout> | null;
  private _everAuthed: boolean;
  // Consecutive connections that closed before receiving auth.ok. A single
  // pre-auth drop can be a transient network blip on a valid token, so the
  // auth-fail signal only fires past a small threshold.
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
      // Not logged in yet (first-ever visit mounts before the token exists).
      // Poll at a fixed short interval instead of giving up forever — the
      // socket comes up on its own right after login, no page reload needed.
      // No exponential backoff here: this isn't a failing server, just a
      // missing credential.
      this._reconnectTimer = setTimeout(() => {
        this._reconnectTimer = null;
        this.connect();
      }, 1000);
      return;
    }
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    this.ws = new WebSocket(`${proto}//${location.host}/ws`);

    let authed = false;
    // Whether this socket ever completed the WebSocket handshake. A close with
    // opened === false means the server was unreachable (down, restarting,
    // offline) — NOT a rejected token, so it must not count as an auth failure.
    let opened = false;

    this.ws.onopen = () => {
      opened = true;
      // Backoff is reset only once auth.ok arrives (below), not here: a socket
      // that opens, fails auth, and is closed by the server must NOT reset the
      // delay, or a rejected token reconnects every second forever.
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
      // Closed AFTER opening but before authenticating: the server rejects a
      // bad token by silently closing (no error frame). Count consecutive such
      // closes and, once it's clearly not a one-off blip, surface it so the app
      // can prompt a re-login instead of hammering reconnects. A close that
      // never opened is an unreachable server (restart, network drop) — that
      // must NOT log the user out, so it is not counted.
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

  // send transmits when the socket is open and reports whether it did — a
  // dropped socket must surface to the caller (roll back optimistic UI, show
  // an error) instead of silently swallowing approvals or run requests.
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
