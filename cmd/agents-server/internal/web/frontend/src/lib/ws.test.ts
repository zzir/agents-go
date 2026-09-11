// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('@/lib/api', () => ({ getToken: () => 'tok' }));
import { WSClient } from '@/lib/ws';
import { EV } from '@/lib/protocol';

class FakeSocket {
  static instances: FakeSocket[] = [];
  static readonly OPEN = 1;
  static readonly CLOSED = 3;
  readyState = 0;
  onopen: (() => void) | null = null;
  onmessage: ((e: { data: string }) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  constructor() { FakeSocket.instances.push(this); }
  send() { /* the auth frame */ }
  close() { this.readyState = FakeSocket.CLOSED; }
  open() { this.readyState = FakeSocket.OPEN; this.onopen?.(); }
  authOk() { this.onmessage?.({ data: JSON.stringify({ type: EV.authOk }) }); }
  drop() { this.readyState = FakeSocket.CLOSED; this.onclose?.(); }
}

const g = globalThis as Record<string, unknown>;
let savedWS: unknown;
beforeEach(() => {
  savedWS = g.WebSocket; g.WebSocket = FakeSocket;
  FakeSocket.instances = [];
  vi.useFakeTimers();
});
afterEach(() => { vi.useRealTimers(); g.WebSocket = savedWS; });

const last = () => FakeSocket.instances[FakeSocket.instances.length - 1];

describe('WSClient', () => {
  it('signals a rejected token only after three closes that had opened; an unreachable server never counts', () => {
    const ws = new WSClient();
    const onAuthFail = vi.fn();
    ws.onAuthFail = onAuthFail;
    ws.connect();
    // Never opened: the server is down, not the token.
    for (const delay of [1000, 2000, 4000, 8000]) {
      last().drop();
      vi.advanceTimersByTime(delay);
    }
    expect(onAuthFail).not.toHaveBeenCalled();
    // Opened, then closed before auth.ok: the third in a row is the signal.
    last().open(); last().drop(); vi.advanceTimersByTime(16000);
    last().open(); last().drop(); vi.advanceTimersByTime(30000);
    expect(onAuthFail).not.toHaveBeenCalled();
    last().open(); last().drop();
    expect(onAuthFail).toHaveBeenCalledTimes(1);
    ws.close();
  });

  it('auth.ok resets the backoff and the failure count, and a re-auth is a reconnect', () => {
    const ws = new WSClient();
    const onReconnect = vi.fn();
    const status: boolean[] = [];
    ws.onReconnect = onReconnect;
    ws.onStatus = c => status.push(c);
    ws.connect();
    last().open(); last().authOk();
    expect(onReconnect).not.toHaveBeenCalled(); // the first auth is not a reconnect
    // Two pre-auth failures: the delay grows to 1s, then 2s.
    last().drop(); vi.advanceTimersByTime(999); expect(FakeSocket.instances).toHaveLength(1);
    vi.advanceTimersByTime(1); expect(FakeSocket.instances).toHaveLength(2);
    last().open(); last().drop(); vi.advanceTimersByTime(1999); expect(FakeSocket.instances).toHaveLength(2);
    vi.advanceTimersByTime(1); expect(FakeSocket.instances).toHaveLength(3);
    // Authenticated again: a reconnect, and the next drop retries after 1s, not 4s.
    last().open(); last().authOk();
    expect(onReconnect).toHaveBeenCalledTimes(1);
    last().drop(); vi.advanceTimersByTime(1000); expect(FakeSocket.instances).toHaveLength(4);
    // The failure count restarted too: two more pre-auth closes do not sign out.
    const onAuthFail = vi.fn();
    ws.onAuthFail = onAuthFail;
    last().open(); last().drop(); vi.advanceTimersByTime(1000);
    last().open(); last().drop(); vi.advanceTimersByTime(2000);
    expect(onAuthFail).not.toHaveBeenCalled();
    expect(status).toEqual([true, false, false, true, false, false, false]);
    ws.close();
  });
});
