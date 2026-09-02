// @vitest-environment jsdom
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { act } from 'react';
import { createRoot } from 'react-dom/client';

vi.mock('@primer/react', () => ({ Tooltip: ({ children }: { children?: unknown }) => children }));
const userLabels = vi.fn();
vi.mock('@/lib/api', () => ({ api: { auth: { userLabels: () => userLabels() } } }));
import { reloadDirectory, useOwnerLabels } from '@/lib/owners';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });
afterEach(() => { vi.useRealTimers(); });

type Labels = ReturnType<typeof useOwnerLabels>;

async function mount() {
  let last!: Labels;
  function Probe() { last = useOwnerLabels(); return null; }
  const root = createRoot(document.createElement('div'));
  await act(async () => { root.render(<Probe />); });
  return { last: () => last, unmount: () => act(async () => { root.unmount(); }) };
}

describe('useOwnerLabels', () => {
  it('fetches once for concurrent mounts, then refetches only when stale or asked', async () => {
    vi.useFakeTimers();
    userLabels.mockResolvedValue([{ id: 'u1', name: 'Ann', email: 'ann@x' }]);
    const a = await mount();
    const b = await mount();
    expect(userLabels).toHaveBeenCalledTimes(1);
    expect(a.last().labelFor('u1')).toBe('Ann');
    expect(b.last().labelFor('u1')).toBe('Ann');

    // Fresh: a third mount reads the copy.
    const c = await mount();
    expect(userLabels).toHaveBeenCalledTimes(1);
    expect(c.last().labelFor('u1')).toBe('Ann');
    await c.unmount();

    // Stale: the next mount refetches, and the answer reaches everyone.
    vi.setSystemTime(Date.now() + 61_000);
    userLabels.mockResolvedValue([{ id: 'u1', name: 'Anne', email: 'ann@x' }]);
    const d = await mount();
    expect(userLabels).toHaveBeenCalledTimes(2);
    expect(a.last().labelFor('u1')).toBe('Anne');
    expect(d.last().labelFor('u1')).toBe('Anne');

    // Asked: reloadDirectory refetches regardless of age.
    userLabels.mockResolvedValue([{ id: 'u1', name: 'Annie', email: 'ann@x' }]);
    await act(async () => { await reloadDirectory(); });
    expect(userLabels).toHaveBeenCalledTimes(3);
    expect(b.last().labelFor('u1')).toBe('Annie');
    await a.unmount(); await b.unmount(); await d.unmount();
  });
});
