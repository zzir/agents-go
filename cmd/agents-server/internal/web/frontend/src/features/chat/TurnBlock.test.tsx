// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// Primer ships CSS the node loader cannot import; the card's Button is a
// plain one here. The markdown pipeline (a worker) and the transcript pieces
// the turn renders are not what this test is about.
vi.mock('@primer/react', () => ({
  Button: ({ children, onClick }: { children?: ReactNode; onClick?: () => void }) => <button type="button" onClick={onClick}>{children}</button>,
  IconButton: () => null,
}));
vi.mock('@/lib/hooks', () => ({ useCopy: () => ({ copied: false, copy: () => {} }) }));
vi.mock('@/lib/markdown', () => ({ useAsyncMarkdown: () => '' }));
vi.mock('@/features/chat/StreamingMarkdown', () => ({ StreamingMarkdown: () => null }));
vi.mock('@/features/chat/TextContent', () => ({ TextContent: () => null }));
vi.mock('@/features/chat/ProcessTimeline', () => ({ ProcessTimeline: () => null }));
import { ErrorCard, endpointTrouble } from '@/features/chat/TurnBlock';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

function mount(node: ReactNode) {
  const host = document.createElement('div');
  document.body.appendChild(host);
  const root = createRoot(host);
  act(() => root.render(node));
  return { host, unmount: () => { act(() => root.unmount()); host.remove(); } };
}

describe('endpointTrouble', () => {
  // The runner's own pre-flight wording (bridge/runner.go, provider_resolve.go).
  it('recognizes the failures a Providers edit fixes', () => {
    expect(endpointTrouble('no API key configured for this agent')).toBe(true);
    expect(endpointTrouble('agent "x": provider 5d1e-…: not found')).toBe(true);
    expect(endpointTrouble('agent "x": provider 5d1e is out of the agent\'s scope — repoint the agent')).toBe(true);
    expect(endpointTrouble('agent "x" names provider 5d1e but no provider store is wired')).toBe(true);
  });

  it('leaves every other error alone', () => {
    expect(endpointTrouble('this agent does not accept images — enable Vision in its Behavior settings')).toBe(false);
    expect(endpointTrouble('the provider returned 500')).toBe(false);
    expect(endpointTrouble('max turns exceeded')).toBe(false);
  });
});

describe('ErrorCard', () => {
  it('offers Open Providers on an endpoint failure, and opens them', () => {
    const open = vi.fn();
    const { host, unmount } = mount(<ErrorCard message="no API key configured for this agent" onOpenProviders={open} />);
    // The disclosure is collapsed by default; open it to reach the body.
    act(() => (host.querySelector('.disclosure-header') as HTMLElement).click());
    const button = host.querySelector('.error-card-actions button') as HTMLButtonElement | null;
    expect(button?.textContent).toBe('Open Providers');
    act(() => button!.click());
    expect(open).toHaveBeenCalledTimes(1);
    unmount();
  });

  it('offers nothing on an error Providers cannot fix, or when Settings cannot be opened', () => {
    const a = mount(<ErrorCard message="max turns exceeded" onOpenProviders={() => {}} />);
    act(() => (a.host.querySelector('.disclosure-header') as HTMLElement).click());
    expect(a.host.querySelector('.error-card-actions')).toBeNull();
    a.unmount();
    const b = mount(<ErrorCard message="no API key configured for this agent" />);
    act(() => (b.host.querySelector('.disclosure-header') as HTMLElement).click());
    expect(b.host.querySelector('.error-card-actions')).toBeNull();
    b.unmount();
  });
});
