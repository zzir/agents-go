// @vitest-environment jsdom
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';
import { createRoot } from 'react-dom/client';

vi.mock('@/lib/apiCache', async importOriginal => ({ ...(await importOriginal<typeof import('@/lib/apiCache')>()), invalidate: vi.fn() }));
const apiMock = vi.hoisted(() => ({
  sessions: {
    messages: vi.fn(async (_sid: string, _o?: unknown) => [] as unknown[]),
    approvals: vi.fn(async (_sid: string) => [] as unknown[]),
    tasks: vi.fn(async (_sid: string) => [] as unknown[]),
    runs: vi.fn(async (_sid: string) => [] as unknown[]),
    traces: vi.fn(async (_sid: string, _o?: unknown) => [] as unknown[]),
    traceSpan: vi.fn(),
  },
}));
vi.mock('@/lib/api', () => ({ api: apiMock, getToken: () => 'tok', clearToken: vi.fn() }));

import { invalidate } from '@/lib/apiCache';
import { EV, ERR } from '@/lib/protocol';
import { useAgentSocket, defaultSS, type SessionEvents, type SessionState } from '@/lib/useAgentSocket';
import type { TurnEntry } from '@/lib/timeline';

// The socket the hook opens: the test drives open/auth/events/drop by hand.
class FakeSocket {
  static instances: FakeSocket[] = [];
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 3;
  readyState = 0;
  sent: Array<{ type: string; payload?: unknown }> = [];
  onopen: (() => void) | null = null;
  onmessage: ((e: { data: string }) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  constructor() { FakeSocket.instances.push(this); }
  send(data: string) { this.sent.push(JSON.parse(data)); }
  close() { this.readyState = FakeSocket.CLOSED; }
  // Opens and authenticates in one step.
  up() { this.readyState = FakeSocket.OPEN; this.onopen?.(); this.receive(EV.authOk); }
  receive(type: string, payload?: unknown) { this.onmessage?.({ data: JSON.stringify({ type, payload }) }); }
  drop() { this.readyState = FakeSocket.CLOSED; this.onclose?.(); }
  sentOf(type: string) { return this.sent.filter(m => m.type === type).map(m => m.payload); }
}

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
let savedWS: unknown;
let savedRAF: unknown;
beforeAll(() => {
  savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true;
  savedWS = g.WebSocket; g.WebSocket = FakeSocket;
  // Delta frames flush synchronously, so an assertion reads state right after
  // an event; the id is 0 so the hook never thinks a frame is still pending.
  savedRAF = g.requestAnimationFrame; g.requestAnimationFrame = (cb: FrameRequestCallback) => { cb(0); return 0; };
});
afterAll(() => {
  if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv;
  g.WebSocket = savedWS; g.requestAnimationFrame = savedRAF;
});

let visibility: DocumentVisibilityState = 'visible';
beforeEach(() => {
  FakeSocket.instances = [];
  visibility = 'visible';
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility });
  vi.mocked(invalidate).mockClear();
  for (const fn of Object.values(apiMock.sessions)) fn.mockClear();
  apiMock.sessions.messages.mockImplementation(async () => []);
});
afterEach(() => { vi.useRealTimers(); });

const S1 = 's1'; const S2 = 's2'; const RUN = 'run-1';
const userRow = (sid: string, text: string) => ({ id: sid + '-u', run_id: RUN, kind: 'item', role: 'user', content: text, entry_id: sid + '-e' });

// Mounts the hook against a plain store; `events` is what the app would pass.
async function mount(active: () => string | null) {
  const store: Record<string, SessionState> = {};
  const updateSS = (sid: string, fn: (s: SessionState) => SessionState) => {
    const cur = store[sid] || defaultSS();
    const next = fn(cur);
    if (next !== cur) store[sid] = next;
  };
  const events: SessionEvents = { activeSession: active, onTitleUpdated: vi.fn(), onProjectBound: vi.fn() };
  let hook!: ReturnType<typeof useAgentSocket>;
  function Probe() { hook = useAgentSocket(updateSS, events); return null; }
  const root = createRoot(document.createElement('div'));
  await act(async () => { root.render(<Probe />); });
  const sock = () => FakeSocket.instances[FakeSocket.instances.length - 1];
  await act(async () => { sock().up(); });
  return {
    store, events, sock,
    hook: () => hook,
    // Drops the socket and lets the client reconnect; the new socket comes up authenticated.
    reconnect: async () => {
      vi.useFakeTimers();
      await act(async () => { sock().drop(); });
      await act(async () => { vi.advanceTimersByTime(1000); });
      vi.useRealTimers();
      await act(async () => { sock().up(); });
    },
    unmount: () => act(async () => { root.unmount(); }),
  };
}

const textParts = (s: SessionState | undefined) =>
  (s?.messages || []).filter(m => m.role === 'turn').flatMap(m => (m as TurnEntry).parts.filter(p => p.type === 'text').map(p => (p as { content: string }).content));

describe('useAgentSocket reconnect', () => {
  it('re-reads the open conversation, forgets the others until their next select, and relists the sidebar', async () => {
    apiMock.sessions.messages.mockImplementation(async (sid: string) => [userRow(sid, 'before')]);
    const t = await mount(() => S1);
    await act(async () => { await t.hook().loadSession(S1); await t.hook().loadSession(S2); });
    expect(apiMock.sessions.messages.mock.calls.map(c => c[0])).toEqual([S1, S2]);
    expect(t.store[S1].messages).toHaveLength(1);
    // A second load of a loaded session is a no-op.
    await act(async () => { await t.hook().loadSession(S1); });
    expect(apiMock.sessions.messages).toHaveBeenCalledTimes(2);

    apiMock.sessions.messages.mockImplementation(async (sid: string) => [userRow(sid, 'before'), { ...userRow(sid, 'while away'), id: sid + '-u2', run_id: 'run-2' }]);
    await t.reconnect();
    // The open conversation was re-read at once; the other was not.
    expect(apiMock.sessions.messages.mock.calls.slice(2).map(c => c[0])).toEqual([S1]);
    expect(t.store[S1].messages.map(m => (m as { content?: string }).content)).toEqual(['before', 'while away']);
    expect(vi.mocked(invalidate)).toHaveBeenCalledWith('sessions');
    // The other one refetches on its next select instead of serving its stale copy.
    await act(async () => { await t.hook().loadSession(S2); });
    expect(apiMock.sessions.messages.mock.calls.slice(3).map(c => c[0])).toEqual([S2]);
    expect(t.store[S2].messages).toHaveLength(2);
    await t.unmount();
  });

  it('keeps the live turn across the re-read and re-subscribes its run', async () => {
    apiMock.sessions.messages.mockImplementation(async (sid: string) => [userRow(sid, 'q')]);
    const t = await mount(() => S1);
    await act(async () => { await t.hook().loadSession(S1); });
    await act(async () => {
      t.sock().receive(EV.runStarted, { session_id: S1, run_id: RUN, input: 'q' });
      t.sock().receive(EV.runMessage, { run_id: RUN, text: 'partial', item_id: 'm1' });
    });
    expect(textParts(t.store[S1])).toEqual(['partial']);
    await t.reconnect();
    expect(t.sock().sentOf(EV.runSubscribe)).toEqual([{ run_id: RUN }]);
    expect(t.store[S1].running).toBe(true);
    expect(textParts(t.store[S1])).toEqual(['partial']);
    await t.unmount();
  });

  it('defers the re-read of a reconnect while hidden to the next visible moment', async () => {
    const t = await mount(() => S1);
    await act(async () => { await t.hook().loadSession(S1); });
    visibility = 'hidden';
    await t.reconnect();
    expect(apiMock.sessions.messages).toHaveBeenCalledTimes(1);
    visibility = 'visible';
    await act(async () => { document.dispatchEvent(new Event('visibilitychange')); });
    expect(apiMock.sessions.messages).toHaveBeenCalledTimes(2);
    // Once repaired, a later visibility flip does nothing.
    await act(async () => { document.dispatchEvent(new Event('visibilitychange')); });
    expect(apiMock.sessions.messages).toHaveBeenCalledTimes(2);
    await t.unmount();
  });
});

describe('useAgentSocket replay', () => {
  it('a gap drops the delta preview and ignores deltas until the next complete item', async () => {
    const t = await mount(() => S1);
    await act(async () => {
      t.sock().receive(EV.runStarted, { session_id: S1, run_id: RUN, input: 'q' });
      t.sock().receive(EV.runStep, { run_id: RUN, delta: 'Hel' });
    });
    expect(t.store[S1].streaming).toBe('Hel');
    await act(async () => { t.sock().receive(EV.runGap, { run_id: RUN, dropped: 2, last_good: 5 }); });
    expect(t.sock().sentOf(EV.runSubscribe)).toEqual([{ run_id: RUN, from_seq: 5 }]);
    expect(t.store[S1].streaming).toBe('');
    // What the old subscription still pushes, and what the replay re-delivers,
    // cannot be told apart: neither reaches the preview.
    await act(async () => {
      t.sock().receive(EV.runStep, { run_id: RUN, delta: 'lo' });
      t.sock().receive(EV.runStep, { run_id: RUN, delta: 'lo' });
    });
    expect(t.store[S1].streaming).toBe('');
    await act(async () => {
      t.sock().receive(EV.runMessage, { run_id: RUN, text: 'Hello', item_id: 'm1' });
      t.sock().receive(EV.runStep, { run_id: RUN, delta: ' world' });
    });
    expect(textParts(t.store[S1])).toEqual(['Hello']);
    expect(t.store[S1].streaming).toBe(' world');
    await t.unmount();
  });

  it('a reconnect drops the live run\'s preview, and the replayed message item ends the mute', async () => {
    const t = await mount(() => S1);
    await act(async () => {
      t.sock().receive(EV.runStarted, { session_id: S1, run_id: RUN, input: 'q' });
      t.sock().receive(EV.runMessage, { run_id: RUN, text: 'first', item_id: 'm1' });
      t.sock().receive(EV.runStep, { run_id: RUN, delta: 'sec' });
    });
    expect(t.store[S1].streaming).toBe('sec');
    await t.reconnect();
    expect(t.store[S1].streaming).toBe('');
    // The hub replays the run from the start: the first item is a duplicate
    // (kept once), the deltas after it rebuild the preview.
    await act(async () => {
      t.sock().receive(EV.runStarted, { session_id: S1, run_id: RUN, input: 'q' });
      t.sock().receive(EV.runStep, { run_id: RUN, delta: 'fir' });
      t.sock().receive(EV.runMessage, { run_id: RUN, text: 'first', item_id: 'm1' });
      t.sock().receive(EV.runStep, { run_id: RUN, delta: 'sec' });
      t.sock().receive(EV.runStep, { run_id: RUN, delta: 'ond' });
    });
    expect(textParts(t.store[S1])).toEqual(['first']);
    expect(t.store[S1].streaming).toBe('second');
    expect(t.store[S1].messages.filter(m => m.role === 'turn')).toHaveLength(1);
    await t.unmount();
  });
});

describe('useAgentSocket run events', () => {
  it('a pause for approval stands the run down, and the resume on the same id brings it back without a second turn', async () => {
    const t = await mount(() => S1);
    await act(async () => {
      t.sock().receive(EV.runStarted, { session_id: S1, run_id: RUN, input: 'q' });
      t.sock().receive(EV.runStep, { run_id: RUN, delta: 'thinking about it' });
      t.sock().receive(EV.runToolCall, { run_id: RUN, tool_call_id: 'tc1', tool_name: 'exec', arguments: '{}', needs_approval: true });
      t.sock().receive(EV.runInterrupted, { run_id: RUN });
    });
    let s = t.store[S1];
    expect(s.running).toBe(false);
    expect(s.liveRunId).toBeNull();
    expect(s.streaming).toBe('');
    await act(async () => {
      t.sock().receive(EV.runStarted, { session_id: S1, run_id: RUN, input: 'q' });
      t.sock().receive(EV.runToolResult, { run_id: RUN, tool_call_id: 'tc1', output: 'ok' });
    });
    s = t.store[S1];
    expect(s.running).toBe(true);
    expect(s.liveRunId).toBe(RUN);
    const turns = s.messages.filter(m => m.role === 'turn') as TurnEntry[];
    expect(turns).toHaveLength(1);
    const tools = turns[0].parts.find(p => p.type === 'tools');
    expect(tools && tools.type === 'tools' ? tools.toolCalls.map(tc => [tc.tool_call_id, tc.status]) : null).toEqual([['tc1', 'completed']]);
    await t.unmount();
  });

  it('a failed first load records why, and the retry clears it', async () => {
    apiMock.sessions.messages.mockImplementation(async () => { throw new Error('502 bad gateway'); });
    const t = await mount(() => S1);
    await act(async () => { await t.hook().loadSession(S1).catch(() => undefined); });
    expect(t.store[S1].loaded).toBe(false);
    expect(t.store[S1].loadError).toBe('502 bad gateway');
    apiMock.sessions.messages.mockImplementation(async (sid: string) => [userRow(sid, 'hi')]);
    await act(async () => { await t.hook().loadSession(S1); });
    expect(t.store[S1].loadError).toBeUndefined();
    expect(t.store[S1].loaded).toBe(true);
    expect(t.store[S1].messages).toHaveLength(1);
    await t.unmount();
  });

  it('a deleted conversation takes no writes until it is loaded again', async () => {
    const t = await mount(() => S1);
    await act(async () => { t.hook().deleteSession(S1); });
    await act(async () => { t.sock().receive(EV.runStarted, { session_id: S1, run_id: RUN, input: 'late' }); });
    expect(t.store[S1]).toBeUndefined();
    // Transferred back: the select loads it, and its runs render again.
    await act(async () => { await t.hook().loadSession(S1); });
    await act(async () => { t.sock().receive(EV.runStarted, { session_id: S1, run_id: 'run-2', input: 'again' }); });
    expect(t.store[S1].running).toBe(true);
    await t.unmount();
  });

  it('session_busy rolls back the newest unsent bubble and leaves the rest', async () => {
    const t = await mount(() => S1);
    const updateSS = (fn: (s: SessionState) => SessionState) => { t.store[S1] = fn(t.store[S1] || defaultSS()); };
    updateSS(s => ({ ...s, loaded: true, messages: [
      { role: 'user', content: 'first', messageId: 'row-1', entryId: 'e1' },
      { role: 'user', content: 'again', clientMsgId: 'c1' },
    ] }));
    await act(async () => { t.sock().receive(EV.runError, { session_id: S1, code: ERR.sessionBusy, message: 'busy' }); });
    expect(t.store[S1].messages.map(m => (m as { content?: string }).content)).toEqual(['first']);
    await t.unmount();
  });
});
