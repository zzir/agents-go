import { getToken } from '/lib/api.js';

export class WSClient {
  constructor() {
    this.ws = null;
    this.handlers = {};
    this._closed = false;
    this._retryDelay = 1000;
  }

  connect() {
    const token = getToken();
    if (!token || this._closed) return;
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    this.ws = new WebSocket(`${proto}//${location.host}/ws`);

    let authed = false;

    this.ws.onopen = () => {
      this._retryDelay = 1000;
      this.ws.send(JSON.stringify({ type: 'auth', token }));
    };

    this.ws.onmessage = (e) => {
      try {
        const env = JSON.parse(e.data);
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

  _scheduleReconnect() {
    const delay = Math.min(this._retryDelay, 30000);
    this._retryDelay = Math.min(delay * 2, 30000);
    setTimeout(() => this.connect(), delay);
  }

  on(type, handler) {
    this.handlers[type] = handler;
    return this;
  }

  send(type, payload) {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify({ type, payload }));
    }
  }

  close() {
    this._closed = true;
    if (this.ws) {
      this.ws.onclose = null;
      this.ws.close();
      this.ws = null;
    }
  }
}
