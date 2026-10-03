// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// Primer ships CSS the node loader cannot import; the card's Button is a
// plain one here. The markdown pipeline (a worker) and the transcript pieces
// the turn renders are not what this test is about.
vi.mock('@primer/react', () => ({
  Button: ({ children, onClick }: { children?: ReactNode; onClick?: () => void }) => <button type="button" onClick={onClick}>{children}</button>,
  IconButton: ({ 'aria-label': label, onClick }: { 'aria-label'?: string; onClick?: () => void }) => <button type="button" aria-label={label} onClick={onClick} />,
}));
vi.mock('@/lib/hooks', () => ({ useCopy: () => ({ copied: false, copy: () => {} }) }));
vi.mock('@/lib/markdown', () => ({ useAsyncMarkdown: () => '' }));
vi.mock('@/features/chat/StreamingMarkdown', () => ({ StreamingMarkdown: () => null }));
vi.mock('@/features/chat/TextContent', () => ({ TextContent: () => null }));
vi.mock('@/features/chat/ProcessTimeline', () => ({ ProcessTimeline: () => null }));
import { ERROR_TITLES, ErrorCard, TurnBlock, endpointTrouble, errorTitle } from '@/features/chat/TurnBlock';
import { ChatSessionProvider, type ChatSessionState, type ChatActions } from '@/features/chat/ChatSessionContext';

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

describe('ErrorCard titles', () => {
  // The first line says what failed, per code; an unknown code is generic.
  it('names each code in words and falls back for an unknown one', () => {
    for (const [code, title] of Object.entries(ERROR_TITLES)) {
      const { host, unmount } = mount(<ErrorCard message="raw" code={code} />);
      expect(host.querySelector('.disclosure-label')?.textContent).toBe(title);
      unmount();
    }
    expect(errorTitle('something_new')).toBe('The run failed');
    expect(errorTitle(undefined)).toBe('The run failed');
  });

  it('offers Compact only for a context overflow, and Retry / the failing span when given', () => {
    const buttons = (host: HTMLElement) => {
      act(() => (host.querySelector('.disclosure-header') as HTMLElement).click());
      return [...host.querySelectorAll('.error-card-actions button')].map(b => b.textContent);
    };
    const over = mount(<ErrorCard message="too long" code="context_overflow" onCompact={() => {}} onRetry={() => {}} onOpenSpan={() => {}} />);
    expect(buttons(over.host)).toEqual(['Retry', 'Compact', 'Open failing span']);
    over.unmount();
    const other = mount(<ErrorCard message="boom" code="provider_error" onCompact={() => {}} onRetry={() => {}} />);
    expect(buttons(other.host)).toEqual(['Retry']);
    other.unmount();
  });
});

describe('TurnBlock controls', () => {
  const actions: ChatActions = { fork: () => {}, regenerate: () => {}, switchBranch: () => {}, openTrace: () => {}, inspectTask: () => {}, retryTask: async () => {}, stopTask: async () => {}, dismissTask: async () => {} };
  const turn = (session: ChatSessionState) => mount(
    <ChatSessionProvider session={session} actions={actions} tasks={{ items: [], lookups: { retryableByCallId: {}, liveTaskStatusByCallId: {}, liveTaskLabelByCallId: {}, taskLabelById: {} } }}>
      <TurnBlock parts={[{ type: 'text', content: 'done' }]} streaming={null} reasoning={null} isLive={false}
        prompt={{ entryId: 'e1', content: 'go' }} messageId="m1" branches={{ parentId: 'e1', tips: ['a', 'b'], active: 0 }} />
    </ChatSessionProvider>,
  );
  const labels = (host: HTMLElement) => Array.from(host.querySelectorAll('button[aria-label]')).map(b => b.getAttribute('aria-label'));

  // A session bound to a project: fork, regenerate and the attempt switch
  // say the project's files are shared (decisions §5.28).
  it('says the project files are shared on a bound session', () => {
    const { host, unmount } = turn({ sessionId: 's1', running: false, compacting: false, agentAvatars: {}, projectBound: true });
    const got = labels(host);
    expect(got).toContain("Fork — the new session shares the project's files with this one");
    expect(got).toContain("Regenerate — the project's files keep what the first attempt changed");
    expect(got).toContain("Next attempt — attempts share the project's files");
    unmount();
  });

  // A turn waiting on a decision offers no branch: forking, regenerating or
  // switching attempts there would abandon the pause.
  it('hides fork, regenerate and the attempt switch while a call awaits a decision', () => {
    const { host, unmount } = mount(
      <ChatSessionProvider session={{ sessionId: 's1', running: false, compacting: false, agentAvatars: {} }} actions={actions} tasks={{ items: [], lookups: { retryableByCallId: {}, liveTaskStatusByCallId: {}, liveTaskLabelByCallId: {}, taskLabelById: {} } }}>
        <TurnBlock parts={[{ type: 'tools', toolCalls: [{ tool_call_id: 'c1', tool_name: 'exec_command', arguments: '{}', output: null, status: null, needs_approval: true }] }]}
          streaming={null} reasoning={null} isLive={false} prompt={{ entryId: 'e1', content: 'go' }} messageId="m1" branches={{ parentId: 'e1', tips: ['a', 'b'], active: 0 }} />
      </ChatSessionProvider>,
    );
    const got = labels(host);
    expect(got.some(l => l?.startsWith('Fork') || l?.startsWith('Regenerate') || l?.includes('attempt'))).toBe(false);
    unmount();
  });

  // A pause of two or more calls offers one Approve all, counting only the
  // calls a person need not confirm one by one.
  it('offers Approve all for a pause of several calls, a plan left out', () => {
    const approveAll = vi.fn();
    const calls = (names: string[]) => names.map((n, i) => ({ tool_call_id: 'c' + i, tool_name: n, arguments: '{}', output: null, status: null, needs_approval: true }));
    const render = (names: string[]) => mount(
      <ChatSessionProvider session={{ sessionId: 's1', running: false, compacting: false, agentAvatars: {} }} actions={{ ...actions, approveAll }} tasks={{ items: [], lookups: { retryableByCallId: {}, liveTaskStatusByCallId: {}, liveTaskLabelByCallId: {}, taskLabelById: {} } }}>
        <TurnBlock parts={[{ type: 'tools', toolCalls: calls(names) }]} streaming={null} reasoning={null} isLive={false} prompt={null} />
      </ChatSessionProvider>,
    );
    const a = render(['exec_command', 'write_file', 'submit_plan']);
    const button = a.host.querySelector('.turn-approve-all button') as HTMLButtonElement | null;
    expect(button?.textContent).toBe('Approve all (2)');
    act(() => button!.click());
    expect(approveAll).toHaveBeenCalledWith(['c0', 'c1']);
    a.unmount();
    const b = render(['exec_command']);
    expect(b.host.querySelector('.turn-approve-all')).toBeNull();
    b.unmount();
  });

  it('keeps the plain words on an unbound session', () => {
    const { host, unmount } = turn({ sessionId: 's1', running: false, compacting: false, agentAvatars: {} });
    const got = labels(host);
    expect(got).toContain('Fork');
    expect(got).toContain('Regenerate');
    expect(got.some(l => l?.includes('project'))).toBe(false);
    unmount();
  });
});
