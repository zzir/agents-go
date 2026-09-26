// @vitest-environment jsdom
import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';
import { createRoot } from 'react-dom/client';

// Primer ships CSS the node loader cannot import; the confirm answers what
// the test set.
const primer = vi.hoisted(() => ({ answer: true, confirm: vi.fn(async () => primer.answer) }));
vi.mock('@primer/react', () => ({ useConfirm: () => primer.confirm }));
import { useUnsavedRegistry } from '@/lib/useUnsavedRegistry';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });
beforeEach(() => { primer.confirm.mockClear(); primer.answer = true; });

async function mount() {
  let last!: ReturnType<typeof useUnsavedRegistry>;
  function Probe() { last = useUnsavedRegistry(); return null; }
  const root = createRoot(document.createElement('div'));
  await act(async () => { root.render(<Probe />); });
  const leave = () => {
    const e = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(e);
    return e.defaultPrevented;
  };
  return { hook: () => last, leave, unmount: () => act(async () => { root.unmount(); }) };
}

describe('useUnsavedRegistry', () => {
  it('holds the page while a form is dirty, and lets it go once every form is clean', async () => {
    const t = await mount();
    expect(t.leave()).toBe(false);
    t.hook().registry.set('a', true);
    t.hook().registry.set('b', true);
    expect(t.leave()).toBe(true);
    t.hook().registry.set('a', false);
    expect(t.leave()).toBe(true);
    t.hook().registry.set('b', false);
    expect(t.leave()).toBe(false);
    await t.unmount();
    t.hook().registry.set('a', true);
    // The listener left with the hook.
    expect(t.leave()).toBe(false);
  });

  it('a guarded close asks only while dirty, and keeps the surface on no', async () => {
    const t = await mount();
    const close = vi.fn();
    await act(async () => { await t.hook().guardedClose(close); });
    expect(primer.confirm).not.toHaveBeenCalled();
    expect(close).toHaveBeenCalledTimes(1);
    t.hook().registry.set('a', true);
    primer.answer = false;
    await act(async () => { await t.hook().guardedClose(close); });
    expect(primer.confirm).toHaveBeenCalledTimes(1);
    expect(close).toHaveBeenCalledTimes(1);
    primer.answer = true;
    await act(async () => { await t.hook().guardedClose(close); });
    expect(close).toHaveBeenCalledTimes(2);
    await t.unmount();
  });
});
