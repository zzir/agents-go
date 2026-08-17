import { describe, it, expect } from 'vitest';
import { backgroundItems, stepProgress, stepRows, taskItem } from '@/lib/background';
import type { TaskState } from '@/lib/useAgentSocket';

const steps = [{ id: 's1', name: 'write' }, { id: 's2', name: 'test' }];

function workflow(over: Partial<TaskState> = {}): TaskState {
  return {
    taskId: 'wf1', label: 'codegen', kind: 'workflow', status: 'working',
    state: { steps, step_id: 's2' }, maxAttempts: 3, attempt: 1, ...over,
  };
}

describe('taskItem for a workflow', () => {
  it('reports the step it is on', () => {
    const it_ = taskItem(workflow());
    expect(it_.kind).toBe('workflow');
    expect(it_.status).toBe('working');
    expect(it_.activity).toBe('step 2/2 · test');
    // Progress is the COMPLETED fraction — the step named by activity is
    // still in flight, so on the last of two steps the bar sits at half.
    expect(it_.progress).toBe(0.5);
    expect(it_.retryable).toBe(false);
    expect(taskItem(workflow({ state: { steps, step_id: 's1' } })).progress).toBe(0);
  });

  // A finished execution has no "current step" to report on.
  it('drops the step readout once it ended', () => {
    const it_ = taskItem(workflow({ status: 'completed' }));
    expect(it_.activity).toBeUndefined();
    expect(it_.progress).toBeUndefined();
  });

  // The strip hides a dismissed execution; the panel still lists it — the flag
  // must survive the state → item mapping for both to read it.
  it('carries the dismissed flag through', () => {
    expect(taskItem(workflow({ status: 'failed', dismissed: true })).dismissed).toBe(true);
    expect(taskItem(workflow()).dismissed).toBeUndefined();
  });

  it('is waiting when a step has an approval pending', () => {
    const it_ = taskItem(workflow({ status: 'input_required', pendingCallId: 'c1', pendingToolName: 'write_file' }));
    expect(it_.status).toBe('input_required');
    expect(it_.pendingCallId).toBe('c1');
    expect(it_.pendingToolName).toBe('write_file');
    // Paused mid-sequence still says where.
    expect(it_.activity).toBe('step 2/2 · test');
  });

  // A failed execution retries like any task: under the attempt ceiling.
  it('offers a retry while attempts remain, and carries the reason', () => {
    const failed = taskItem(workflow({ status: 'failed', summary: 'boom' }));
    expect(failed.error).toBe('boom');
    expect(failed.retryable).toBe(true);
    expect(taskItem(workflow({ status: 'failed', attempt: 3 })).retryable).toBe(false);
    expect(taskItem(workflow({ status: 'completed' })).retryable).toBe(false);
    // An execution its budget or the step ceiling stopped is refused a retry
    // before a run — no button for it.
    expect(taskItem(workflow({ status: 'failed', state: { steps, step_id: 's2', stopped: 'budget' } })).retryable).toBe(false);
  });
});

describe('stepProgress', () => {
  it('is empty without a state or a known step', () => {
    expect(stepProgress(undefined)).toEqual({});
    expect(stepProgress({ steps, step_id: 'nope' })).toEqual({});
  });

  it('counts the runs once a step has run again, so a loop shows', () => {
    const run = (step_id: string, n: number) => ({ step_id, run_id: 'r' + n });
    // Two steps, two runs: a plain sequence says nothing about runs.
    const plain = stepProgress({ steps, step_id: steps[1].id, step_runs: [run(steps[0].id, 1), run(steps[1].id, 2)] });
    expect(plain.activity).not.toContain('run');
    // A third run — the sequence came back — is said.
    const looped = stepProgress({ steps, step_id: steps[0].id, step_runs: [run(steps[0].id, 1), run(steps[1].id, 2), run(steps[0].id, 3)] });
    expect(looped.activity).toContain('· run 3');
    // A person's retry runs a step again without the sequence looping: not
    // a run count.
    const retried = stepProgress({ steps, step_id: steps[1].id, step_runs: [run(steps[0].id, 1), run(steps[1].id, 2), { ...run(steps[1].id, 3), retry: true }] });
    expect(retried.activity).not.toContain('run');
  });
});

describe('backgroundItems', () => {
  it('puts tasks and executions in one list', () => {
    const tasks: Record<string, TaskState> = {
      t1: { taskId: 't1', label: 'research', status: 'working', lastTool: 'brave_search', createdAt: 5 },
      wf1: workflow(),
    };
    const items = backgroundItems(tasks);
    expect(items.map(i => [i.kind, i.id])).toEqual([['task', 't1'], ['workflow', 'wf1']]);
    expect(items[0].activity).toBe('brave_search');
  });

  it('is empty for a session with none', () => {
    expect(backgroundItems({})).toEqual([]);
    expect(backgroundItems(undefined)).toEqual([]);
  });
});

describe('stepRows', () => {
  const state = {
    steps: [{ id: 's1', name: 'write' }, { id: 's2', name: 'check' }],
    step_id: 's2',
    step_runs: [{ step_id: 's1', run_id: 'r1', outcome: 'completed' }, { step_id: 's2', run_id: 'r2' }],
  };
  it('joins the launch log with each run\'s trace', () => {
    const rows = stepRows(state, 'working', {
      r1: [
        { kind: 'span', type: 'generation', data: { input_tokens: 10, output_tokens: 5 }, started_at: '2026-01-01T00:00:00Z', ended_at: '2026-01-01T00:00:02Z' },
        { kind: 'span', type: 'function', started_at: '2026-01-01T00:00:02Z', ended_at: '2026-01-01T00:00:03Z' },
      ],
    });
    expect(rows.map(r => [r.index, r.name, r.outcome])).toEqual([[1, 'write', 'completed'], [2, 'check', 'running']]);
    expect(rows[0].tokens).toEqual({ input: 10, output: 5 });
    expect(rows[0].durationMs).toBe(3000);
    // No spans loaded for the live run: nothing to report yet, not zeros.
    expect(rows[1].tokens).toBeUndefined();
    expect(rows[1].durationMs).toBeUndefined();
  });
  it('gives the last run the task\'s own terminal status', () => {
    expect(stepRows(state, 'failed')[1].outcome).toBe('failed');
    expect(stepRows(undefined, 'completed')).toEqual([]);
  });
  it('falls back to the log\'s own stamps for a run without loaded spans', () => {
    const stamped = {
      ...state,
      step_runs: [{ step_id: 's1', run_id: 'r1', outcome: 'completed', started_at: '2026-01-01T00:00:00Z', ended_at: '2026-01-01T00:00:45Z' }],
    };
    expect(stepRows(stamped, 'completed')[0].durationMs).toBe(45000);
    // Spans, when loaded, win — they bound the actual work.
    const rows = stepRows(stamped, 'completed', { r1: [{ kind: 'span', type: 'function', started_at: '2026-01-01T00:00:00Z', ended_at: '2026-01-01T00:00:03Z' }] });
    expect(rows[0].durationMs).toBe(3000);
  });
});
