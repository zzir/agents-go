// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import { ReadOnlyContext } from '@/lib/access';

// Primer ships CSS the node loader cannot import: the switch is a bare button
// here, carrying exactly the props the row wires onto it.
vi.mock('@primer/react', () => ({
  ToggleSwitch: (p: Record<string, unknown>) => (
    <button type="button" aria-pressed={p.checked as boolean} disabled={p.disabled as boolean}
      aria-labelledby={p['aria-labelledby'] as string} aria-describedby={p['aria-describedby'] as string | undefined}
      onClick={p.onClick as () => void} />
  ),
}));
import { ToggleRow } from './ToggleRow';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

function mount(node: ReactNode) {
  const el = document.createElement('div');
  document.body.appendChild(el);
  const root = createRoot(el);
  act(() => root.render(node));
  const button = () => el.querySelector('button')!;
  return { button, unmount: () => { act(() => root.unmount()); el.remove(); } };
}

describe('ToggleRow', () => {
  it('names and describes the switch, and a click flips the value', () => {
    const onChange = vi.fn();
    const { button, unmount } = mount(
      <ToggleRow label="Trace sensitive data" description="Records prompts." checked={false} onChange={onChange} />,
    );
    const byRef = (attr: string) => document.getElementById(button().getAttribute(attr) ?? '')?.textContent;
    expect(byRef('aria-labelledby')).toBe('Trace sensitive data');
    expect(byRef('aria-describedby')).toBe('Records prompts.');
    expect(button().disabled).toBe(false);
    act(() => button().click());
    expect(onChange).toHaveBeenCalledWith(true);
    unmount();
  });

  it('references no description when there is none', () => {
    const { button, unmount } = mount(<ToggleRow label="Enabled" checked onChange={() => {}} />);
    expect(button().hasAttribute('aria-describedby')).toBe(false);
    unmount();
  });

  it('is disabled inside a read-only dialog', () => {
    const onChange = vi.fn();
    const { button, unmount } = mount(
      <ReadOnlyContext.Provider value={true}><ToggleRow label="Enabled" checked onChange={onChange} /></ReadOnlyContext.Provider>,
    );
    expect(button().disabled).toBe(true);
    act(() => button().click());
    expect(onChange).not.toHaveBeenCalled();
    unmount();
  });
});
