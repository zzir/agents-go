import { describe, it, expect } from 'vitest';
import { mergeBackground, workflowItem, type WorkflowRunRow } from '@/lib/background';
import type { TaskState } from '@/lib/useAgentSocket';

const steps = [{ id: 's1', name: 'write' }, { id: 's2', name: 'test' }];

function run(over: Partial<WorkflowRunRow> = {}): WorkflowRunRow {
  return { id: 'wf1', name: 'codegen', status: 'running', step_id: 's2', steps, ...over };
}

describe('workflowItem', () => {
  it('reports the step it is on', () => {
    const it_ = workflowItem(run());
    expect(it_.status).toBe('working');
    expect(it_.activity).toBe('step 2/2 · test');
    // Progress is the COMPLETED fraction — the step named by activity is
    // still in flight, so on the last of two steps the bar sits at half.
    expect(it_.progress).toBe(0.5);
    expect(it_.retryable).toBe(false);
    expect(workflowItem(run({ step_id: 's1' })).progress).toBe(0);
  });

  // The strip hides a dismissed execution; the panel still lists it — the flag
  // must survive the row → item mapping for both to read it.
  it('carries the dismissed flag through', () => {
    expect(workflowItem(run({ status: 'failed', dismissed: true })).dismissed).toBe(true);
    expect(workflowItem(run()).dismissed).toBeUndefined();
  });

  it('is waiting when a step has an approval pending', () => {
    const it_ = workflowItem(run(), {
      workflow_run_id: 'wf1',
      tool_calls: [{ tool_call_id: 'c1', tool_name: 'write_file' }],
    });
    expect(it_.status).toBe('input_required');
    expect(it_.pendingCallId).toBe('c1');
    expect(it_.pendingToolName).toBe('write_file');
  });

  // A pending approval must not resurrect a finished execution: the row is the
  // authority on whether it is still going.
  it('stays terminal when a stale approval is still on file', () => {
    const it_ = workflowItem(run({ status: 'failed', error: 'boom' }), {
      workflow_run_id: 'wf1',
      tool_calls: [{ tool_call_id: 'c1', tool_name: 'write_file' }],
    });
    expect(it_.status).toBe('failed');
    expect(it_.error).toBe('boom');
    expect(it_.retryable).toBe(true);
  });

  it('offers no retry once it completed', () => {
    expect(workflowItem(run({ status: 'completed' })).retryable).toBe(false);
  });

  // An unknown status is shown as live rather than silently terminal.
  it('treats an unrecognized status as still running', () => {
    expect(workflowItem(run({ status: 'reticulating' })).status).toBe('working');
  });
});

describe('mergeBackground', () => {
  it('puts tasks and executions in one list', () => {
    const tasks: Record<string, TaskState> = {
      t1: { taskId: 't1', label: 'research', status: 'working', lastTool: 'brave_search', createdAt: 5 },
    };
    const items = mergeBackground(tasks, [run()], []);
    expect(items.map(i => [i.kind, i.id])).toEqual([['task', 't1'], ['workflow', 'wf1']]);
    expect(items[0].activity).toBe('brave_search');
  });

  it('is empty for a session with neither', () => {
    expect(mergeBackground({}, [], [])).toEqual([]);
    expect(mergeBackground(undefined, null, null)).toEqual([]);
  });
});
