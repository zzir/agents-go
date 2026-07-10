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
  private _closed: boolean;
  private _retryDelay: number;
  private _reconnectTimer: ReturnType<typeof setTimeout> | null;
  private _everAuthed: boolean;

  constructor() {
    this.ws = null;
    this.handlers = {};
    this.onReconnect = null;
    this._closed = false;
    this._retryDelay = 1000;
    this._reconnectTimer = null;
    this._everAuthed = false;
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

    this.ws.onopen = () => {
      this._retryDelay = 1000;
      this.ws!.send(JSON.stringify({ type: EV.auth, token }));
    };

    this.ws.onmessage = (e: MessageEvent) => {
      try {
        const env: WSEnvelope = JSON.parse(e.data);
        if (!authed) {
          if (env.type === EV.authOk) {
            authed = true;
            // Re-auth after a prior session means we reconnected; let the
            // caller resubscribe/resync. First-ever auth is not a reconnect.
            if (this._everAuthed) this.onReconnect?.();
            this._everAuthed = true;
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
