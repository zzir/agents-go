// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// The two lenses mount with their chrome stubbed: Primer (icon buttons keep
// their label and click), the side panel (its title kept) and the octicons.
vi.mock('@primer/react', () => {
  const stub = (): unknown => {
    const C = (p: { children?: ReactNode }) => <div>{p.children}</div>;
    return new Proxy(C, {
      get: (t, k) => (k in t ? (t as never)[k] : typeof k === 'string' && /^[A-Z]/.test(k) ? stub() : undefined),
    });
  };
  const names = ['Button', 'Checkbox', 'CounterLabel', 'Dialog', 'Flash', 'Link', 'SegmentedControl', 'Select', 'SelectPanel', 'Spinner', 'Textarea', 'TextInput'];
  return {
    ...Object.fromEntries(names.map(n => [n, stub()])),
    IconButton: ({ 'aria-label': label, onClick }: { 'aria-label'?: string; onClick?: () => void }) => <button type="button" aria-label={label} onClick={onClick} />,
  };
});
vi.mock('@/components/Loading', () => ({ Loading: () => <div>loading</div> }));
vi.mock('@/layout/SidePanel', () => ({
  SidePanel: ({ title, count, children }: { title: string; count?: number; children?: ReactNode }) => <div><h2>{title}{count !== undefined ? ` (${count})` : ''}</h2>{children}</div>,
}));
import { ChatSessionProvider, useDerivedChatTasks, type ChatActions, type ChatSessionState } from '@/features/chat/ChatSessionContext';
import { TraceDrawer, TraceRunPanel, TraceRunMissingPanel, type TraceEventData, type TraceGroupView } from '@/features/chat/TracePanel';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => {
  savedActEnv = g.IS_REACT_ACT_ENVIRONMENT;
  g.IS_REACT_ACT_ENVIRONMENT = true;
  Element.prototype.scrollIntoView = () => {};
});
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

const noop = () => {};
const resolve = async () => {};
const session: ChatSessionState = { sessionId: 's1', running: false, compacting: false, agentAvatars: {} };

function Harness({ children }: { children: ReactNode }) {
  const actions: ChatActions = { openTrace: noop, inspectTask: noop, retryTask: resolve, stopTask: resolve, dismissTask: resolve, loadSpan: resolve };
  const tasks = useDerivedChatTasks({});
  return <ChatSessionProvider session={session} actions={actions} tasks={tasks}>{children}</ChatSessionProvider>;
}

function mount(el: ReactNode) {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  act(() => { root.render(<Harness>{el}</Harness>); });
  return { container, unmount: () => { act(() => root.unmount()); container.remove(); } };
}

const t = (s: number) => new Date(Date.UTC(2026, 9, 21, 0, 0, s)).toISOString();
const gen = (id: string, tokens: number): TraceEventData => ({
  kind: 'span', name: 'generation', type: 'generation', span_id: id, started_at: t(0), ended_at: t(1), data: { input_tokens: tokens, output_tokens: 1 },
});
const GROUPS: TraceGroupView[] = [
  { rootId: 'r1', segments: [{ runId: 'r1', events: [gen('g1', 10)] }, { runId: 'w1', events: [gen('g2', 20)] }] },
  { rootId: 'r2', segments: [{ runId: 'r2', events: [{ kind: 'span', name: 'a', type: 'agent', span_id: 'a2', started_at: t(0), ended_at: t(1), error: 'boom' }] }] },
];
const LABELS = { r1: 'hello', r2: 'again' };

describe('TraceDrawer', () => {
  // One row per exchange, carrying what a chain of runs cost in all and how
  // many spans it left; a click opens the exchange by its root run.
  it('lists one row per exchange and opens it on click', () => {
    const opened: string[] = [];
    const m = mount(<TraceDrawer groups={GROUPS} liveRunId="w1" runLabels={LABELS} staleRuns={new Set(['r2'])} onOpen={id => opened.push(id)} onClose={noop} />);
    const rows = Array.from(m.container.querySelectorAll('.trace-row'));
    expect(rows).toHaveLength(2);
    expect(rows[0].querySelector('.trace-row-label')!.textContent).toBe('hello');
    expect(rows[0].querySelector('.trace-row-tokens')!.textContent).toBe('32 tok');
    expect(rows[0].querySelector('.trace-tab-live')).not.toBeNull(); // live through its wake-up run
    expect(rows[1].querySelector('.trace-ev-tag-error')).not.toBeNull();
    expect(rows[1].querySelector('.trace-run-stale')).not.toBeNull();
    expect(m.container.querySelector('.disclosure')).toBeNull(); // rows open a lens, nothing expands in place
    act(() => { (rows[0] as HTMLElement).click(); });
    expect(opened).toEqual(['r1']);
    m.unmount();
  });
});

describe('TraceRunPanel', () => {
  // The lens is titled by the run's label and counted by its spans; its head
  // leads back, shows what the run cost and jumps to the message; the run id
  // is plain text on the meta line, and nothing in the waterfall is a chevron.
  it('shows the run under a head with back, cost and jump', () => {
    let backs = 0, jumps = 0;
    const m = mount(<TraceRunPanel group={GROUPS[0]} liveRunId={null} runLabels={LABELS} onJump={() => { jumps++; }} onBack={() => { backs++; }} onClose={noop} />);
    expect(m.container.querySelector('h2')!.textContent).toBe('hello (2)');
    expect(m.container.querySelector('.trace-head-tokens')!.textContent).toBe('32 tok');
    expect(m.container.querySelector('.trace-meta-id')!.textContent).toBe('r1');
    expect(m.container.querySelector('[aria-label="Copy id"]')).toBeNull();
    expect(m.container.querySelector('.trace-span-chevron')).toBeNull();
    expect(m.container.querySelectorAll('.trace-run-segment')).toHaveLength(2);
    act(() => { (m.container.querySelector('[aria-label="Jump to message"]') as HTMLElement).click(); });
    act(() => { (m.container.querySelector('[aria-label="Back to traces"]') as HTMLElement).click(); });
    expect([backs, jumps]).toEqual([1, 1]);
    m.unmount();
  });

  it('offers no jump for a run without a message', () => {
    const m = mount(<TraceRunPanel group={GROUPS[1]} liveRunId={null} runLabels={LABELS} onBack={noop} onClose={noop} />);
    expect(m.container.querySelector('[aria-label="Jump to message"]')).toBeNull();
    expect(m.container.querySelector('.trace-meta .trace-ev-tag-error')).not.toBeNull();
    m.unmount();
  });
});

describe('TraceRunMissingPanel', () => {
  it('waits while traces load, then says the run is not here', () => {
    const m = mount(<TraceRunMissingPanel runId="r9" loading onBack={noop} onClose={noop} />);
    expect(m.container.querySelector('h2')!.textContent).toBe('Trace');
    expect(m.container.textContent).not.toContain('not found');
    m.unmount();
    const m2 = mount(<TraceRunMissingPanel runId="r9" loading={false} onBack={noop} onClose={noop} />);
    expect(m2.container.querySelector('h2')!.textContent).toBe('Run not found');
    expect(m2.container.querySelector('.lens-head-id-text')!.textContent).toBe('r9');
    m2.unmount();
  });
});
