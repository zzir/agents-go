// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, beforeEach, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// Primer's provider and base styles are stubbed: this is about the choice,
// not the styling. The provider keeps its last props for the scheme tests.
let primerProps: Record<string, unknown> = {};
vi.mock('@primer/react', () => ({
  ThemeProvider: ({ children, ...props }: { children?: ReactNode }) => { primerProps = props; return <>{children}</>; },
  BaseStyles: ({ children }: { children?: ReactNode }) => <>{children}</>,
}));
import { ThemeProvider, readPreference, resolveTheme, useTheme } from '@/theme/ThemeProvider';
import { primerScheme, readScheme, themeFile } from '@/theme/themes';

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
const htmlAttr = (name: string) => document.documentElement.getAttribute(name);
// The stamp lands once a theme's CSS has loaded: wait for it rather than a tick.
async function stamped(name: string, value: string) {
  for (let i = 0; i < 100 && htmlAttr(name) !== value; i++) await act(async () => { await new Promise(r => setTimeout(r, 5)); });
  expect(htmlAttr(name)).toBe(value);
}

function mount() {
  let ctx!: ReturnType<typeof useTheme>;
  function Probe() { ctx = useTheme(); return null; }
  const host = document.createElement('div');
  const root = createRoot(host);
  act(() => { root.render(<ThemeProvider><Probe /></ThemeProvider>); });
  return { ctx: () => ctx, unmount: () => act(() => root.unmount()) };
}

describe('ThemeProvider', () => {
  it('follows the system by default, live', async () => {
    const m = mount();
    expect(m.ctx().preference).toBe('system');
    expect(m.ctx().theme).toBe('day');
    await stamped('data-color-mode', 'light');
    act(() => flipSystem(true));
    expect(m.ctx().theme).toBe('night');
    await stamped('data-color-mode', 'dark');
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

  it('stamps <html> with both themes, and a picked variant is remembered', async () => {
    const m = mount();
    expect(m.ctx().scheme).toEqual({ light: 'light', dark: 'dark', highContrast: false });
    await stamped('data-light-theme', 'light');
    expect(htmlAttr('data-dark-theme')).toBe('dark');
    act(() => m.ctx().setScheme({ dark: 'dark_dimmed' }));
    await stamped('data-dark-theme', 'dark_dimmed');
    expect(htmlAttr('data-light-theme')).toBe('light');
    expect(localStorage.getItem('theme.dark')).toBe('dark_dimmed');
    expect(primerProps.nightScheme).toBe('dark_dimmed');
    m.unmount();
  });

  it('high contrast names the twin of each theme, with Primer given a name it defines', async () => {
    const m = mount();
    act(() => m.ctx().setScheme({ light: 'light_colorblind', highContrast: true }));
    await stamped('data-light-theme', 'light_colorblind_high_contrast');
    expect(htmlAttr('data-dark-theme')).toBe('dark_high_contrast');
    expect(localStorage.getItem('theme.highContrast')).toBe('true');
    // Primer's object has no combined name: the base stands in; a twin it has is passed as is.
    expect(primerProps.dayScheme).toBe('light_colorblind');
    expect(primerProps.nightScheme).toBe('dark_high_contrast');
    expect(primerProps.contextOnly).toBe(true);
    m.unmount();
  });

  it('reads a stored scheme and falls back on an unknown or throwing value', () => {
    localStorage.setItem('theme.light', 'light_tritanopia');
    localStorage.setItem('theme.dark', 'light');
    localStorage.setItem('theme.highContrast', 'true');
    expect(readScheme()).toEqual({ light: 'light_tritanopia', dark: 'dark', highContrast: true });
    const getItem = Storage.prototype.getItem;
    Storage.prototype.getItem = () => { throw new Error('denied'); };
    try { expect(readScheme()).toEqual({ light: 'light', dark: 'dark', highContrast: false }); } finally { Storage.prototype.getItem = getItem; }
    expect(themeFile('dark_dimmed', true)).toBe('dark_dimmed_high_contrast');
    expect(primerScheme('dark_dimmed', true)).toBe('dark_dimmed');
    expect(primerScheme('light', true)).toBe('light_high_contrast');
  });
});
