// @vitest-environment jsdom
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// Primer ships CSS the node loader cannot import: a Flash is a div here,
// carrying the props the toast wires onto it.
vi.mock('@primer/react', () => ({
  Flash: (p: { children: ReactNode; role?: string; className?: string; onClick: () => void; onKeyDown: (e: React.KeyboardEvent) => void }) => (
    <div role={p.role} className={p.className} onClick={p.onClick} onKeyDown={p.onKeyDown}>{p.children}</div>
  ),
  IconButton: (p: { 'aria-label': string; onClick: (e: React.MouseEvent) => void }) => <button type="button" aria-label={p['aria-label']} onClick={p.onClick} />,
}));
import { toast } from '@/lib/toast';
import { GlobalToast } from './GlobalToast';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

describe('GlobalToast', () => {
  it('Escape dismisses only the toast that has the focus, and stops there', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const el = document.createElement('div');
    document.body.appendChild(el);
    const root = createRoot(el);
    await act(async () => root.render(<GlobalToast />));
    await act(async () => { toast.error('boom'); });
    const item = el.querySelector('[role="alert"]')!;
    expect(item).not.toBeNull();
    expect(el.querySelector('.global-toast-stack')!.getAttribute('role')).toBeNull();

    // Escape elsewhere on the page leaves the toast alone.
    const outside = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true });
    await act(async () => { document.body.dispatchEvent(outside); });
    expect(el.querySelector('[role="alert"]')).not.toBeNull();

    // Escape on the toast's own close button takes it, and nothing beyond hears it.
    const heard = vi.fn();
    document.addEventListener('keydown', heard);
    const close = item.querySelector('button')!;
    await act(async () => { close.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true })); });
    expect(el.querySelector('[role="alert"]')!.className).toContain('global-toast-exit');
    expect(heard).not.toHaveBeenCalled();
    document.removeEventListener('keydown', heard);
    await act(async () => root.unmount());
    el.remove();
    vi.restoreAllMocks();
  });
});
