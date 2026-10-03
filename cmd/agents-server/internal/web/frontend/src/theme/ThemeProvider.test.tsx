// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, beforeEach, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// Primer's provider and base styles are stubbed: this is about the choice,
// not the styling.
vi.mock('@primer/react', () => ({
  ThemeProvider: ({ children }: { children?: ReactNode }) => <>{children}</>,
  BaseStyles: ({ children }: { children?: ReactNode }) => <>{children}</>,
}));
import { ThemeProvider, readPreference, resolveTheme, useTheme } from '@/theme/ThemeProvider';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

// A matchMedia the test flips: jsdom has none.
let systemDark = false;
const listeners = new Set<(e: { matches: boolean }) => void>();
beforeEach(() => {
  localStorage.clear();
  systemDark = false;
  listeners.clear();
  window.matchMedia = ((query: string) => ({
    matches: query.includes('dark') && systemDark,
    media: query,
    addEventListener: (_: string, fn: (e: { matches: boolean }) => void) => { listeners.add(fn); },
    removeEventListener: (_: string, fn: (e: { matches: boolean }) => void) => { listeners.delete(fn); },
  })) as unknown as typeof window.matchMedia;
});
const flipSystem = (dark: boolean) => { systemDark = dark; for (const fn of listeners) fn({ matches: dark }); };

function mount() {
  let ctx!: ReturnType<typeof useTheme>;
  function Probe() { ctx = useTheme(); return null; }
  const host = document.createElement('div');
  const root = createRoot(host);
  act(() => { root.render(<ThemeProvider><Probe /></ThemeProvider>); });
  return { ctx: () => ctx, unmount: () => act(() => root.unmount()) };
}

describe('ThemeProvider', () => {
  it('follows the system by default, live', () => {
    const m = mount();
    expect(m.ctx().preference).toBe('system');
    expect(m.ctx().theme).toBe('day');
    expect(document.documentElement.getAttribute('data-color-mode')).toBe('light');
    act(() => flipSystem(true));
    expect(m.ctx().theme).toBe('night');
    expect(document.documentElement.getAttribute('data-color-mode')).toBe('dark');
    m.unmount();
  });

  it('a picked theme stands whatever the system does, and is remembered', () => {
    const m = mount();
    act(() => m.ctx().setPreference('light'));
    act(() => flipSystem(true));
    expect(m.ctx().theme).toBe('day');
    expect(localStorage.getItem('theme')).toBe('light');
    act(() => m.ctx().setPreference('system'));
    expect(m.ctx().theme).toBe('night');
    m.unmount();
  });

  it('reads an older stored value and shrugs off storage that throws', () => {
    localStorage.setItem('theme', 'dark');
    expect(readPreference()).toBe('dark');
    localStorage.setItem('theme', 'purple');
    expect(readPreference()).toBe('system');
    const getItem = Storage.prototype.getItem;
    Storage.prototype.getItem = () => { throw new Error('denied'); };
    try { expect(readPreference()).toBe('system'); } finally { Storage.prototype.getItem = getItem; }
    expect(resolveTheme('dark', false)).toBe('night');
    expect(resolveTheme('system', true)).toBe('night');
  });
});
