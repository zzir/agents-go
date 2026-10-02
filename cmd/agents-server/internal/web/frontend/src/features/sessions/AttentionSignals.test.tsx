// @vitest-environment jsdom
import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, useCallback, useEffect, useState } from 'react';
import { createRoot } from 'react-dom/client';

// rows.value is what the list endpoint answers; lists counts the reads.
const { rows } = vi.hoisted(() => ({ rows: { value: [] as { id: string; name: string; status?: string }[], lists: 0 } }));
vi.mock('@/lib/api', () => ({ api: { sessions: { list: async () => { rows.lists++; return rows.value; } } } }));
vi.mock('@/lib/hooks', () => ({
  useApi: (fetcher: () => Promise<unknown>) => {
    const [data, setData] = useState<unknown>(null);
    const [gen, setGen] = useState(0);
    useEffect(() => {
      let alive = true;
      fetcher().then(d => { if (alive) setData(d); });
      return () => { alive = false; };
      // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [gen]);
    const reload = useCallback(() => setGen(n => n + 1), []);
    return { data, loading: data === null, error: null, reload, mutateData: () => {} };
  },
}));
const notify = vi.hoisted(() => vi.fn());
vi.mock('@/lib/attention', async importOriginal => ({ ...(await importOriginal<typeof import('@/lib/attention')>()), notifyAttention: notify }));
import { AttentionSignals } from '@/features/sessions/AttentionSignals';
import type { SessionStatus } from '@/lib/protocol';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });
beforeEach(() => { notify.mockClear(); rows.lists = 0; document.title = 'agents-go'; });

async function mount(announced: Record<string, SessionStatus>) {
  const host = document.createElement('div');
  document.body.appendChild(host);
  const root = createRoot(host);
  const render = (next: Record<string, SessionStatus>) => act(async () => { root.render(<AttentionSignals announced={next} />); });
  await render(announced);
  await act(async () => {});
  const spoken = () => host.querySelector('[aria-live="polite"]')!.textContent;
  return { render, spoken, unmount: () => { act(() => root.unmount()); host.remove(); } };
}

describe('AttentionSignals', () => {
  // The title counts what the server says is waiting — the list's rows under
  // the statuses announced since — and nothing at load is spoken as news.
  it('counts the waiting conversations in the title, and is quiet about what was already so', async () => {
    rows.value = [
      { id: 'a', name: 'Alpha', status: 'requires_action' },
      { id: 'b', name: 'Beta', status: 'running' },
    ];
    const m = await mount({});
    expect(document.title).toBe('(1) waiting · agents-go');
    expect(m.spoken()).toBe('');
    expect(notify).not.toHaveBeenCalled();

    await m.render({ b: 'requires_action' });
    expect(document.title).toBe('(2) waiting · agents-go');
    expect(m.spoken()).toBe('Beta needs your approval');
    expect(notify.mock.calls).toEqual([['Beta needs your approval', 'b']]);

    // The decision was made: the count drops, the line is cleared, and the
    // next time it waits it is said again.
    await m.render({ b: 'running' });
    expect(document.title).toBe('(1) waiting · agents-go');
    expect(m.spoken()).toBe('');
    await m.render({ b: 'requires_action' });
    expect(m.spoken()).toBe('Beta needs your approval');
    expect(notify).toHaveBeenCalledTimes(2);

    m.unmount();
    expect(document.title).toBe('agents-go');
  });

  it('says a failure, and repeats nothing a row already said', async () => {
    rows.value = [
      { id: 'a', name: 'Alpha', status: 'requires_action' },
      { id: 'b', name: 'Beta', status: 'running' },
    ];
    const m = await mount({});
    // The announcement only confirms the row: not news.
    await m.render({ a: 'requires_action' });
    expect(m.spoken()).toBe('');
    expect(notify).not.toHaveBeenCalled();
    await m.render({ a: 'requires_action', b: 'failed' });
    expect(m.spoken()).toBe('Beta failed');
    expect(notify.mock.calls).toEqual([['Beta failed', 'b']]);
    // Two changes in one render: the one with nothing to say does not swallow
    // the other's line.
    await m.render({ a: 'idle', b: 'requires_action' });
    expect(m.spoken()).toBe('Beta needs your approval');
    m.unmount();
  });

  // A conversation made in another tab is not in this one's list: the status
  // announced for it relists once, and it is then counted and named.
  it('relists once for a conversation it has not listed, then counts and names it', async () => {
    rows.value = [{ id: 'a', name: 'Alpha', status: 'idle' }];
    const m = await mount({});
    expect(rows.lists).toBe(1);
    rows.value = [...rows.value, { id: 'n', name: 'Nightly', status: 'requires_action' }];
    await m.render({ n: 'requires_action' });
    await act(async () => {});
    expect(rows.lists).toBe(2);
    expect(document.title).toBe('(1) waiting · agents-go');
    expect(m.spoken()).toBe('Nightly needs your approval');
    expect(notify.mock.calls).toEqual([['Nightly needs your approval', 'n']]);
    // One the list never returns (deleted since) is asked for once, not forever.
    await m.render({ n: 'requires_action', gone: 'failed' });
    await act(async () => {});
    await act(async () => {});
    expect(rows.lists).toBe(3);
    m.unmount();
  });

  // After an outage the announcements start over and the list is read again:
  // what moved in between is still news.
  it('says what a relist shows to have changed', async () => {
    rows.value = [{ id: 'a', name: 'Alpha', status: 'running' }];
    const m = await mount({ a: 'running' });
    rows.value = [{ id: 'a', name: 'Alpha', status: 'failed' }, { id: 'q', name: 'Quiet', status: 'idle' }];
    // The reconnect clears what was announced; the unknown id forces the relist the socket would ask for.
    await m.render({ zz: 'idle' });
    await act(async () => {});
    expect(m.spoken()).toBe('Alpha failed');
    expect(notify.mock.calls).toEqual([['Alpha failed', 'a']]);
    m.unmount();
  });
});
