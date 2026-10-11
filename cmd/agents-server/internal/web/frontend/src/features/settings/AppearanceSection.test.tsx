// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, beforeEach, vi } from 'vitest';
import { act, Children, isValidElement, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import { ReadOnlyContext } from '@/lib/access';

// Primer ships CSS the node loader cannot import: its pieces are bare elements
// here, each carrying exactly the props the section wires onto it.
vi.mock('@primer/react', () => {
  const Pass = ({ children }: { children?: ReactNode }) => <>{children}</>;
  const PageHeader = Object.assign(Pass, { TitleArea: Pass, Title: Pass, Description: Pass });
  // Primer's controlled form: onChange on the control, `selected` on a Button.
  const SegmentedControl = Object.assign(
    ({ children, onChange }: { children?: ReactNode; onChange: (i: number) => void }) => (
      <div data-modes>
        {Children.map(children, (child, i) => {
          const props = isValidElement<{ children?: ReactNode; selected?: boolean }>(child) ? child.props : {};
          return <button type="button" aria-current={!!props.selected} onClick={() => onChange(i)}>{props.children}</button>;
        })}
      </div>
    ),
    { Button: Pass },
  );
  return {
    ThemeProvider: Pass,
    BaseStyles: Pass,
    PageHeader,
    Stack: Pass,
    Tooltip: Pass,
    SegmentedControl,
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
  const swatches = () => [...el.querySelectorAll<HTMLButtonElement>('[role="radiogroup"] [role="radio"]')];
  const swatch = (name: string) => swatches().find(b => b.getAttribute('data-color-mode') === name.split('_')[0] && b.getAttribute('data-light-theme')?.startsWith(name))!;
  const checked = () => swatches().filter(b => b.getAttribute('aria-checked') === 'true').map(b => b.getAttribute('data-light-theme'));
  const ringed = () => swatches().filter(b => b.getAttribute('data-ring') === 'other').map(b => b.getAttribute('data-light-theme'));
  const mode = (text: string) => [...el.querySelectorAll<HTMLButtonElement>('[data-modes] button')].find(b => b.textContent === text)!;
  const caption = () => el.querySelector('.theme-row-name')!.textContent;
  return { el, swatches, swatch, checked, ringed, mode, caption, unmount: () => { act(() => root.unmount()); el.remove(); } };
}

describe('AppearanceSection', () => {
  it('shows the seven in one row, light then dark, the one on screen checked and the other pick ringed', () => {
    const m = mount();
    expect(m.swatches().map(b => b.getAttribute('data-light-theme'))).toEqual(
      ['light', 'light_colorblind', 'light_tritanopia', 'dark', 'dark_colorblind', 'dark_tritanopia', 'dark_dimmed']);
    expect(m.swatches().map(b => b.getAttribute('data-color-mode'))).toEqual(['light', 'light', 'light', 'dark', 'dark', 'dark', 'dark']);
    expect(m.el.querySelectorAll('.theme-swatches-divider').length).toBe(1);
    expect(m.swatch('light_colorblind').className).toContain('theme-swatch--wedges');
    // System mode on a light system: the light pick shows, the dark pick waits.
    expect(m.mode('System').getAttribute('aria-current')).toBe('true');
    expect(m.checked()).toEqual(['light']);
    expect(m.ringed()).toEqual(['dark']);
    expect(m.caption()).toBe('Light default');
    m.unmount();
  });

  it('under system, a dark pick is remembered without changing what shows', async () => {
    const m = mount();
    act(() => m.swatch('dark_dimmed').click());
    await stamped('data-dark-theme', 'dark_dimmed');
    expect(localStorage.getItem('theme.dark')).toBe('dark_dimmed');
    expect(localStorage.getItem('theme')).toBeNull();
    expect(m.checked()).toEqual(['light']);
    expect(m.ringed()).toEqual(['dark_dimmed']);
    expect(m.caption()).toBe('Light default');
    m.unmount();
  });

  it('a fixed mode follows the swatch clicked, in a read-only dialog too, and the switch renames every swatch', async () => {
    const m = mount();
    act(() => m.mode('Light').click());
    expect(localStorage.getItem('theme')).toBe('light');
    expect(m.ringed()).toEqual([]);
    act(() => m.swatch('dark_colorblind').click());
    await stamped('data-color-mode', 'dark');
    await stamped('data-dark-theme', 'dark_colorblind');
    expect(localStorage.getItem('theme')).toBe('dark');
    expect(m.mode('Dark').getAttribute('aria-current')).toBe('true');
    expect(m.checked()).toEqual(['dark_colorblind']);
    expect(m.caption()).toBe('Dark protanopia and deuteranopia');

    const sw = m.el.querySelector<HTMLButtonElement>('[data-switch]')!;
    expect(sw.disabled).toBe(false);
    act(() => sw.click());
    await stamped('data-dark-theme', 'dark_colorblind_high_contrast');
    expect(htmlAttr('data-light-theme')).toBe('light_high_contrast');
    expect(m.swatches().map(b => b.getAttribute('data-dark-theme'))).toEqual([
      'light_high_contrast', 'light_colorblind_high_contrast', 'light_tritanopia_high_contrast',
      'dark_high_contrast', 'dark_colorblind_high_contrast', 'dark_tritanopia_high_contrast', 'dark_dimmed_high_contrast']);
    m.unmount();
  });
});
