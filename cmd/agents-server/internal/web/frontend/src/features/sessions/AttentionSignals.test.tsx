// @vitest-environment jsdom
import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, useEffect, useState } from 'react';
import { createRoot } from 'react-dom/client';

const { rows } = vi.hoisted(() => ({ rows: { value: [] as { id: string; name: string; status?: string }[] } }));
vi.mock('@/lib/api', () => ({ api: { sessions: { list: async () => rows.value } } }));
vi.mock('@/lib/hooks', () => ({
  useApi: (fetcher: () => Promise<unknown>) => {
    const [data, setData] = useState<unknown>(null);
    useEffect(() => {
      let alive = true;
      fetcher().then(d => { if (alive) setData(d); });
      return () => { alive = false; };
      // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []);
    return { data, loading: data === null, error: null, reload: () => {}, mutateData: () => {} };
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
beforeEach(() => { notify.mockClear(); document.title = 'agents-go'; });

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
});
