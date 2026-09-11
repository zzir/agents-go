// @vitest-environment jsdom
import { describe, expect, it, vi } from 'vitest';
import { createTaskRouter, mergeTaskRows, type TaskRouterDeps } from '@/lib/taskEvents';
import { defaultSS, type SessionState } from '@/lib/useAgentSocket';
import { findToolCall, type TimelineEntry } from '@/lib/timeline';

const P = 'parent'; const T = 'task-1'; const TC = 'tc1';

// A parent conversation whose turn holds the spawn card for T, in `cardState`.
function parentWith(cardState: { status?: string; attempt?: number; summary?: string } | null): SessionState {
  const messages: TimelineEntry[] = [
    { role: 'user', content: 'go', messageId: 'u1' },
    { role: 'turn', messageId: 't1', runId: 'run-p', parts: [{ type: 'tools', toolCalls: [{
      tool_call_id: TC, tool_name: 'spawn_task', arguments: '{}', output: 'spawned', status: 'completed',
      ...(cardState ? { task: { id: T, label: 'L', ...cardState } } : {}),
    }] }] },
  ];
  return { ...defaultSS(), loaded: true, messages };
}

function harness(initial: Record<string, SessionState>) {
  const store = { ...initial };
  const deps: TaskRouterDeps = {
    updateSS: (sid, fn) => { const cur = store[sid] || defaultSS(); const next = fn(cur); if (next !== cur) store[sid] = next; },
    fetchTimeline: vi.fn(async () => ({ timeline: [] })),
    scheduleFrame: (_k, flush) => flush(),
    sessionOfRun: () => undefined,
    isDeleted: () => false,
  };
  return { store, router: createTaskRouter(() => deps) };
}

const card = (s: SessionState) => findToolCall(s.messages, TC)?.task;

describe('task router', () => {
  it('a retry re-arms the spawn card, its outcome folds in, and a replayed start of an older attempt cannot', () => {
    const { store, router } = harness({ [P]: parentWith({ status: 'failed', attempt: 1, summary: 'boom' }) });
    expect(router.runStarted({ run_id: 'r2', session_id: 'child', parent_session_id: P, task_id: T, tool_call_id: TC, label: 'L', attempt: 2, max_attempts: 3 })).toBe(true);
    expect(store[P].tasks[T]).toMatchObject({ status: 'working', attempt: 2, maxAttempts: 3, dismissed: false });
    expect(card(store[P])).toEqual({ label: 'L', attempt: 2 }); // re-armed: no outcome

    expect(router.output({ run_id: 'r2', final_output: 'all done' })).toBe(true);
    expect(store[P].tasks[T]).toMatchObject({ status: 'completed', summary: 'all done' });
    expect(card(store[P])).toMatchObject({ status: 'completed', attempt: 2, summary: 'all done' });

    // The hub replays attempt 1's run.started: the chip goes live again on
    // that event alone, but the card keeps the outcome it has.
    router.runStarted({ run_id: 'r1', session_id: 'child', parent_session_id: P, task_id: T, tool_call_id: TC, label: 'L', attempt: 1 });
    expect(card(store[P])).toMatchObject({ status: 'completed', attempt: 2 });
  });

  it('a run of an unknown parent is not a background run', () => {
    const { router } = harness({});
    expect(router.runStarted({ run_id: 'r', session_id: 's' })).toBe(false);
    expect(router.step({ run_id: 'r', delta: 'x' })).toBe(false);
  });

  it('task.updated never moves a task backwards', () => {
    const { store, router } = harness({ [P]: parentWith({ status: 'completed', attempt: 2 }) });
    store[P] = { ...store[P], tasks: { [T]: { taskId: T, label: 'L', status: 'completed', attempt: 2, toolCallId: TC } } };
    router.taskUpdated({ task_id: T, parent_session_id: P, status: 'working', attempt: 1 });
    router.taskUpdated({ task_id: T, parent_session_id: P, status: 'working', attempt: 2 });
    expect(store[P].tasks[T]).toMatchObject({ status: 'completed', attempt: 2 });
    router.taskUpdated({ task_id: T, parent_session_id: P, status: 'working', attempt: 3, label: 'L2' });
    expect(store[P].tasks[T]).toMatchObject({ status: 'working', attempt: 3, label: 'L2', dismissed: false });
    expect(card(store[P])).toMatchObject({ attempt: 3 });
    expect(card(store[P])?.status).toBeUndefined();
  });
});

describe('mergeTaskRows', () => {
  it('folds a newer durable row into the chip and the card, and drops a stale one', () => {
    let s = parentWith({ attempt: 1 });
    s = { ...s, tasks: { [T]: { taskId: T, label: 'L', status: 'working', attempt: 1, toolCallId: TC } } };
    s = mergeTaskRows(s, [{ task_id: T, status: 'completed', attempt: 1, summary: 'ok', tool_call_id: TC, updated_at: '2026-09-11T00:00:00Z' }]);
    expect(s.tasks[T]).toMatchObject({ status: 'completed', summary: 'ok' });
    expect(card(s)).toMatchObject({ status: 'completed', summary: 'ok', attempt: 1 });

    const again = mergeTaskRows(s, [{ task_id: T, status: 'working', attempt: 1 }]);
    expect(again.tasks[T].status).toBe('completed');
    expect(card(again)).toMatchObject({ status: 'completed' });
  });
});
