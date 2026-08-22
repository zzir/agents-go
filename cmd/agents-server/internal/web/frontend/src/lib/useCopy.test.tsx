// @vitest-environment jsdom
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { act } from 'react';
import { createRoot } from 'react-dom/client';
// hooks.ts imports useConfirm; Primer's dist drags CSS vitest can't load.
vi.mock('@primer/react', () => ({ useConfirm: () => async () => true }));
import { useCopy } from '@/lib/hooks';
import { onToast } from '@/lib/toast';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });
afterEach(() => { onToast(null); vi.unstubAllGlobals(); });

describe('useCopy', () => {
  it('reports, rather than throws, when there is no clipboard (plain-http origin)', async () => {
    vi.stubGlobal('navigator', { ...navigator, clipboard: undefined });
    const toasts: string[] = [];
    onToast(({ msg, type }) => { toasts.push(type + ':' + msg); });
    let last!: ReturnType<typeof useCopy>;
    function Probe() { last = useCopy(); return null; }
    const root = createRoot(document.createElement('div'));
    await act(async () => { root.render(<Probe />); });
    expect(() => last.copy('secret')).not.toThrow();
    expect(toasts).toEqual(['error:Could not copy — select it and copy by hand']);
    expect(last.copied).toBeNull();
  });
});
