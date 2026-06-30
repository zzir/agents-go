import { getToken } from '@/lib/api';

interface WSEnvelope {
  type: string;
  token?: string;
  payload?: unknown;
}

export class WSClient {
  ws: WebSocket | null;
  handlers: Record<string, // eslint-disable-next-line @typescript-eslint/no-explicit-any
(payload: any) => void>;
  private _closed: boolean;
  private _retryDelay: number;
  private _reconnectTimer: ReturnType<typeof setTimeout> | null;

  constructor() {
    this.ws = null;
    this.handlers = {};
    this._closed = false;
    this._retryDelay = 1000;
    this._reconnectTimer = null;
  }

  connect(): void {
    const token = getToken();
    if (!token || this._closed) return;
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    this.ws = new WebSocket(`${proto}//${location.host}/ws`);

    let authed = false;

    this.ws.onopen = () => {
      this._retryDelay = 1000;
      this.ws!.send(JSON.stringify({ type: 'auth', token }));
    };

    this.ws.onmessage = (e: MessageEvent) => {
      try {
        const env: WSEnvelope = JSON.parse(e.data);
        if (!authed) {
          if (env.type === 'auth.ok') { authed = true; }
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

  send(type: string, payload: unknown): void {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify({ type, payload }));
    }
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
