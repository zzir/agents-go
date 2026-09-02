// @vitest-environment jsdom
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';
import { act } from 'react';
import { createRoot } from 'react-dom/client';
// hooks.ts imports useConfirm; Primer's dist drags CSS vitest can't load.
vi.mock('@primer/react', () => ({ useConfirm: () => async () => true }));
import { invalidate, useApi } from '@/lib/hooks';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

type Result = ReturnType<typeof useApi<string[]>>;

// Mounts one useApi consumer of `key` and exposes what it returned last.
async function mount(key: string, fetcher: () => Promise<string[]>) {
  let last!: Result;
  function Probe() { last = useApi(fetcher, [], key); return null; }
  const root = createRoot(document.createElement('div'));
  await act(async () => { root.render(<Probe />); });
  return { last: () => last, unmount: () => act(async () => { root.unmount(); }) };
}

describe('useApi shared cache', () => {
  it('shares one request between concurrent consumers and serves the answer to a later mount', async () => {
    const fetcher = vi.fn(async () => ['a']);
    const a = await mount('k1', fetcher);
    const b = await mount('k1', fetcher);
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(a.last().data).toEqual(['a']);
    expect(b.last().data).toEqual(['a']);

    const c = await mount('k1', fetcher);
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(c.last().loading).toBe(false);
    expect(c.last().data).toEqual(['a']);
    await a.unmount(); await b.unmount(); await c.unmount();
  });

  it('a reload anywhere reaches every consumer of the key', async () => {
    let n = 0;
    const fetcher = vi.fn(async () => ['v' + ++n]);
    const a = await mount('k2', fetcher);
    const b = await mount('k2', fetcher);
    await act(async () => { await a.last().reload(); });
    expect(b.last().data).toEqual(['v2']);
    await a.unmount(); await b.unmount();
  });

  it('invalidate drops the entry and refetches the mounted consumers', async () => {
    let n = 0;
    const fetcher = vi.fn(async () => ['v' + ++n]);
    const a = await mount('k3', fetcher);
    expect(a.last().data).toEqual(['v1']);
    await act(async () => { invalidate(/^k3$/); });
    expect(fetcher).toHaveBeenCalledTimes(2);
    expect(a.last().data).toEqual(['v2']);
    await a.unmount();

    // Gone from the cache: the next mount fetches afresh.
    await act(async () => { invalidate('k3'); });
    const b = await mount('k3', fetcher);
    expect(fetcher).toHaveBeenCalledTimes(3);
    expect(b.last().data).toEqual(['v3']);
    await b.unmount();
  });

  it('a failed fetch is not cached: the next mount retries', async () => {
    let fail = true;
    const fetcher = vi.fn(async () => { if (fail) throw new Error('boom'); return ['ok']; });
    const a = await mount('k4', fetcher);
    expect(a.last().error).toBe('boom');
    expect(a.last().data).toBeNull();
    await a.unmount();
    fail = false;
    const b = await mount('k4', fetcher);
    expect(fetcher).toHaveBeenCalledTimes(2);
    expect(b.last().data).toEqual(['ok']);
    await b.unmount();
  });
});
