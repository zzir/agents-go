// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, beforeEach, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import { ReadOnlyContext } from '@/lib/access';

// Primer ships CSS the node loader cannot import: its pieces are bare elements
// here, the switch carrying exactly the props the row wires onto it.
vi.mock('@primer/react', () => {
  const Pass = ({ children }: { children?: ReactNode }) => <>{children}</>;
  const PageHeader = Object.assign(Pass, { TitleArea: Pass, Title: Pass, Description: Pass });
  return {
    ThemeProvider: Pass,
    BaseStyles: Pass,
    PageHeader,
    Stack: Pass,
    Tooltip: Pass,
    ToggleSwitch: (p: Record<string, unknown>) => (
      <button type="button" data-switch aria-pressed={p.checked as boolean} disabled={p.disabled as boolean} onClick={p.onClick as () => void} />
    ),
  };
});
import { ThemeProvider } from '@/theme/ThemeProvider';
import { AppearanceSection } from './AppearanceSection';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });
beforeEach(() => { localStorage.clear(); });
const htmlAttr = (name: string) => document.documentElement.getAttribute(name);
// The stamp lands once a theme's CSS has loaded: wait for it rather than a tick.
async function stamped(name: string, value: string) {
  for (let i = 0; i < 100 && htmlAttr(name) !== value; i++) await act(async () => { await new Promise(r => setTimeout(r, 5)); });
  expect(htmlAttr(name)).toBe(value);
}

function mount() {
  const el = document.createElement('div');
  document.body.appendChild(el);
  const root = createRoot(el);
  // Inside a member's read-only dialog: the section is personal and stays live.
  act(() => root.render(
    <ReadOnlyContext.Provider value={true}><ThemeProvider><AppearanceSection /></ThemeProvider></ReadOnlyContext.Provider>,
  ));
  const swatches = (group: 'Light theme' | 'Dark theme') => {
    const label = [...el.querySelectorAll('.theme-row-title')].find(n => n.textContent === group)!;
    return [...document.querySelectorAll<HTMLButtonElement>(`[role="radiogroup"][aria-labelledby="${label.id}"] [role="radio"]`)];
  };
  return { el, swatches, unmount: () => { act(() => root.unmount()); el.remove(); } };
}

describe('AppearanceSection', () => {
  it('shows three light and four dark swatches painted by their own theme, the chosen one named', () => {
    const { el, swatches, unmount } = mount();
    const light = swatches('Light theme');
    const dark = swatches('Dark theme');
    expect(light.map(b => b.getAttribute('data-light-theme'))).toEqual(['light', 'light_colorblind', 'light_tritanopia']);
    expect(dark.map(b => b.getAttribute('data-dark-theme'))).toEqual(['dark', 'dark_colorblind', 'dark_tritanopia', 'dark_dimmed']);
    expect(dark.map(b => b.getAttribute('data-color-mode'))).toEqual(['dark', 'dark', 'dark', 'dark']);
    expect(light[0].getAttribute('aria-checked')).toBe('true');
    expect(light[1].className).toContain('theme-swatch--wedges');
    expect(el.querySelectorAll('.theme-row-name')[0].textContent).toBe('Light default');
    unmount();
  });

  it('a click applies at once, in a read-only dialog too, and the switch renames every swatch', async () => {
    const { el, swatches, unmount } = mount();
    act(() => swatches('Light theme')[1].click());
    await stamped('data-light-theme', 'light_colorblind');
    expect(localStorage.getItem('theme.light')).toBe('light_colorblind');
    expect(swatches('Light theme')[1].getAttribute('aria-checked')).toBe('true');
    expect(el.querySelectorAll('.theme-row-name')[0].textContent).toBe('Light protanopia and deuteranopia');

    const sw = el.querySelector<HTMLButtonElement>('[data-switch]')!;
    expect(sw.disabled).toBe(false);
    act(() => sw.click());
    await stamped('data-light-theme', 'light_colorblind_high_contrast');
    expect(htmlAttr('data-dark-theme')).toBe('dark_high_contrast');
    expect(swatches('Dark theme').map(b => b.getAttribute('data-dark-theme'))).toEqual(
      ['dark_high_contrast', 'dark_colorblind_high_contrast', 'dark_tritanopia_high_contrast', 'dark_dimmed_high_contrast']);
    unmount();
  });
});
