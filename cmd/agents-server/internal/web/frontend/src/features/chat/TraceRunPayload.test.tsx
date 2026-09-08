// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// Primer ships CSS the node loader cannot import; every Primer piece the trace
// panel names is a plain element here (statics like Select.Option included).
vi.mock('@primer/react', () => {
  // Statics (Select.Option, …) are stubs too; React's own introspection keys
  // (symbols, $$typeof, defaultProps, …) must read as absent.
  const stub = (): unknown => {
    const C = (p: { children?: ReactNode }) => <div>{p.children}</div>;
    return new Proxy(C, {
      get: (t, k) => (k in t ? (t as never)[k] : typeof k === 'string' && /^[A-Z]/.test(k) ? stub() : undefined),
    });
  };
  const names = ['Button', 'Checkbox', 'CounterLabel', 'Dialog', 'Flash', 'IconButton', 'Link', 'SegmentedControl', 'Select', 'SelectPanel', 'Textarea', 'TextInput'];
  return Object.fromEntries(names.map(n => [n, stub()]));
});
import { ChatSessionProvider, useDerivedChatTasks, type ChatActions, type ChatSessionState } from '@/features/chat/ChatSessionContext';
import { TraceRun, type TraceEventData } from '@/features/chat/TracePanel';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => {
  savedActEnv = g.IS_REACT_ACT_ENVIRONMENT;
  g.IS_REACT_ACT_ENVIRONMENT = true;
  // jsdom lays nothing out: the expanded card's scroll-into-view is a no-op.
  Element.prototype.scrollIntoView = () => {};
});
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

const noop = () => {};
const resolve = async () => {};
const session: ChatSessionState = { sessionId: 's1', running: false, compacting: false, agentAvatars: {} };

function Harness({ events, loadSpan }: { events: TraceEventData[]; loadSpan: ChatActions['loadSpan'] }) {
  const actions: ChatActions = { openTrace: noop, inspectTask: noop, retryTask: resolve, stopTask: resolve, dismissTask: resolve, loadSpan };
  const tasks = useDerivedChatTasks({});
  return (
    <ChatSessionProvider session={session} actions={actions} tasks={tasks}>
      <TraceRun runId="r1" segments={[{ runId: 'r1', events }]} label="hello" isLive={false} isExpanded onToggle={noop} />
    </ChatSessionProvider>
  );
}

// A summary row (payload left out) opens on a fetch: the row says it is
// loading, asks once for exactly this span of this run in this session, and
// renders the payload once the parent has swapped the whole span in.
describe('TraceRun', () => {
  it('fetches an opened span\'s payload once and renders it when it lands', async () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    const summary: TraceEventData = {
      kind: 'span', name: 'generation', type: 'generation', span_id: 'sp1',
      started_at: '2026-08-19T00:00:00.000Z', ended_at: '2026-08-19T00:00:01.000Z',
      data: { model: 'm', input_tokens: 5 }, payloadOmitted: true,
    };
    let release: () => void = noop;
    const calls: string[][] = [];
    const loadSpan = (sid: string, runId: string, spanId: string) => {
      calls.push([sid, runId, spanId]);
      return new Promise<void>(res => { release = res; });
    };
    act(() => { root.render(<Harness events={[summary]} loadSpan={loadSpan} />); });
    const row = container.querySelector('.trace-span-clickable') as HTMLElement | null;
    expect(row).not.toBeNull(); // a payload to fetch is details to open
    act(() => { row!.click(); });
    expect(calls).toEqual([['s1', 'r1', 'sp1']]);
    expect(container.textContent).toContain('Loading the payload');
    // The parent swaps the whole span in (what loadSpanPayload does) and the
    // fetch settles: the payload renders, and nothing is asked again.
    const full: TraceEventData = { ...summary, payloadOmitted: false, data: { ...summary.data, input: [{ role: 'user', content: 'the question' }], output: [] } };
    await act(async () => { root.render(<Harness events={[full]} loadSpan={loadSpan} />); release(); });
    expect(container.textContent).not.toContain('Loading the payload');
    expect(container.textContent).toContain('the question');
    expect(calls).toHaveLength(1);
    act(() => { root.unmount(); });
  });

  // A parent's track carries its children's extents as segments over its own
  // dimmed bar, each in the child's color; a leaf's bar stands alone.
  it('overlays a parent\'s bar with its children\'s extents', () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    const t = (s: number) => new Date(Date.UTC(2026, 7, 19, 0, 0, s)).toISOString();
    const events: TraceEventData[] = [
      { kind: 'span', name: 'a', type: 'agent', span_id: 'p', started_at: t(0), ended_at: t(100) },
      { kind: 'span', name: 'a', type: 'generation', span_id: 'c1', parent_id: 'p', started_at: t(0), ended_at: t(10) },
      { kind: 'span', name: 'function:ls', type: 'function', span_id: 'c2', parent_id: 'p', started_at: t(50), ended_at: t(51), error: 'boom' },
    ];
    act(() => { root.render(<Harness events={events} loadSpan={resolve} />); });
    const tracks = container.querySelectorAll('.trace-span-track');
    expect(tracks).toHaveLength(3);
    expect(tracks[0].querySelector('.trace-span-bar')!.className).toContain('covered');
    const segs = Array.from(tracks[0].querySelectorAll('.trace-span-seg')) as HTMLElement[];
    expect(segs.map(g => [g.style.left, g.style.width])).toEqual([['0%', '10%'], ['50%', '1%']]);
    expect(segs[1].getAttribute('style')).toContain('var(--fgColor-danger)');
    expect(tracks[1].querySelector('.trace-span-bar')!.className).not.toContain('covered');
    expect(tracks[1].querySelectorAll('.trace-span-seg')).toHaveLength(0);
    act(() => { root.unmount(); });
  });

  // A run that paused and went on (an approval's resume restarts the loop
  // under the same run id) renders each stretch on its own timeline, the later
  // one headed by the length of the pause, a before-run compaction going with
  // the agent after it; a handoff's agents stay on one timeline.
  it('gives a resumed run\'s stretches their own timelines', () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    const t = (s: number) => new Date(Date.UTC(2026, 8, 8, 0, 0, s)).toISOString();
    const events: TraceEventData[] = [
      { kind: 'span', name: 'a', type: 'agent', span_id: 'a1', started_at: t(0), ended_at: t(2) },
      { kind: 'span', name: 'a', type: 'generation', span_id: 'g1', parent_id: 'a1', started_at: t(0), ended_at: t(2) },
      { kind: 'span', name: 'compaction', type: 'compaction', span_id: 'c1', started_at: t(134), ended_at: t(135) },
      { kind: 'span', name: 'a', type: 'agent', span_id: 'a2', started_at: t(135), ended_at: t(137) },
      { kind: 'span', name: 'function:memory_write', type: 'function', span_id: 'f1', parent_id: 'a2', started_at: t(135), ended_at: t(136) },
      { kind: 'span', name: 'transfer_to_b', type: 'handoff', span_id: 'h1', parent_id: 'a2', started_at: t(136), ended_at: t(137) },
      { kind: 'span', name: 'b', type: 'agent', span_id: 'a3', started_at: t(137), ended_at: t(140) },
    ];
    act(() => { root.render(<Harness events={events} loadSpan={resolve} />); });
    const segs = container.querySelectorAll('.trace-run-segment');
    expect(segs).toHaveLength(2);
    expect(segs[0].querySelector('.trace-segment-label')).toBeNull();
    expect((segs[0].querySelector('.trace-span-bar') as HTMLElement).style.width).toBe('100%');
    expect(segs[1].querySelector('.trace-segment-label')!.textContent).toBe('2m12s later');
    const lefts = Array.from(segs[1].querySelectorAll('.trace-span-bar')).map(b => (b as HTMLElement).style.left);
    expect(lefts).toEqual(['0%', '16.67%', '16.67%', '33.33%', '50%']);
    act(() => { root.unmount(); });
  });

  it('says so when the payload cannot be fetched', async () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    const summary: TraceEventData = { kind: 'span', name: 'function:ls', type: 'function', span_id: 'sp2', payloadOmitted: true };
    const loadSpan = () => Promise.reject(new Error('not found'));
    act(() => { root.render(<Harness events={[summary]} loadSpan={loadSpan} />); });
    await act(async () => { (container.querySelector('.trace-span-clickable') as HTMLElement).click(); });
    expect(container.textContent).toContain('not stored yet');
    act(() => { root.unmount(); });
  });
});
