// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// Primer ships CSS the node loader cannot import; the card's one Primer piece
// is a plain button here.
vi.mock('@primer/react', () => ({
  Link: ({ children, onClick }: { children?: ReactNode; onClick?: () => void }) => <button type="button" onClick={onClick}>{children}</button>,
}));
import { FirstMileCard, composerGate } from '@/features/chat/FirstMile';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

describe('composerGate', () => {
  // A list not yet in hand is never "no agents": the first-mile card and the
  // disabled composer wait for the answer.
  it('treats a null list as loading, or as the failure it was', () => {
    expect(composerGate(null, null).state).toBe('loading');
    expect(composerGate(null, 'boom').state).toBe('error');
    expect(composerGate(null, null).blocked).toBeTruthy();
  });

  it('blocks the composer only on an empty list', () => {
    expect(composerGate([], null)).toEqual({ state: 'none', blocked: expect.stringContaining('Settings') });
    expect(composerGate([{ id: 'a' }], null)).toEqual({ state: 'ready' });
  });
});

describe('FirstMileCard', () => {
  it('lists the three steps, and the first two open their Settings tab', () => {
    const onSettingsOpen = vi.fn();
    const host = document.createElement('div');
    document.body.appendChild(host);
    const root = createRoot(host);
    act(() => root.render(<FirstMileCard onSettingsOpen={onSettingsOpen} />));
    const steps = [...host.querySelectorAll('li')].map(li => li.textContent);
    expect(steps).toHaveLength(3);
    expect(steps[0]).toContain('Add a provider');
    expect(steps[1]).toContain('Create an agent');
    expect(steps[2]).toContain('Send a message');
    const links = [...host.querySelectorAll('button')];
    expect(links).toHaveLength(2);
    act(() => links[0].click());
    act(() => links[1].click());
    expect(onSettingsOpen.mock.calls).toEqual([['providers'], ['agents']]);
    act(() => root.unmount());
    host.remove();
  });

  it('renders the steps as text when Settings cannot be opened from here', () => {
    const host = document.createElement('div');
    document.body.appendChild(host);
    const root = createRoot(host);
    act(() => root.render(<FirstMileCard />));
    expect(host.querySelectorAll('button')).toHaveLength(0);
    expect(host.querySelectorAll('li')).toHaveLength(3);
    act(() => root.unmount());
    host.remove();
  });
});
