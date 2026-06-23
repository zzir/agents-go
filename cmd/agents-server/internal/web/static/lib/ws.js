export class WSClient {
  constructor() {
    this.ws = null;
    this.handlers = {};
  }

  connect() {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    this.ws = new WebSocket(`${proto}//${location.host}/ws`);

    this.ws.onmessage = (e) => {
      try {
        const env = JSON.parse(e.data);
        const handler = this.handlers[env.type];
        if (handler) handler(env.payload);
      } catch (err) {
        console.error('ws parse error:', err);
      }
    };

    this.ws.onclose = () => {
      setTimeout(() => this.connect(), 2000);
    };

    this.ws.onerror = () => {
      this.ws.close();
    };

    return new Promise((resolve) => {
      this.ws.onopen = resolve;
    });
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
    if (this.ws) {
      this.ws.onclose = null;
      this.ws.close();
      this.ws = null;
    }
  }
}
