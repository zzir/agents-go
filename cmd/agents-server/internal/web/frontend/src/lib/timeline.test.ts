// The isomorphism contract between the two ways a turn is built:
//
//   streaming — useAgentSocket applies run events via the streamReducer
//               transforms as they arrive;
//   replay    — buildTimeline rebuilds the same turn from the ENTRIES the
//               backend persisted (the displays the runner recorded, plus
//               runner.savePartialTurn's annotations).
//
// What the user watched stream in must equal what a reload shows. These tests
// drive BOTH paths for the same logical turn and assert the resulting
// turn.parts are identical, so a shape change on one side fails here instead
// of shipping a UI that renders differently after refresh.
//
// Documented intentional differences (asserted below, keep this list in sync
// with docs/explanation/workbench-invariants.md):
//   1. handoff parts are live-only — a reload conveys the transfer via the
//      transfer_to_* tool-call card instead.
//   2. a user-rejected tool call keeps status 'rejected' live, but replays as
//      'completed' — per-call status is not persisted; the rejection notice
//      survives in the call's output text.
import { describe, it, expect } from 'vitest';
import { buildTimeline, findToolCall, rowKeys, type EntryView, type TimelineEntry, type TurnEntry } from '@/lib/timeline';
import {
  ensureLiveTurn, mergeLiveTail, appendInjected, appendMessageItem, appendReasoningItem, finalizeTurn,
  appendErrorPart, appendCancelledPart, appendToolCall, applyToolResult, applyTaskTerminal, startTaskAttempt, syncTaskCard, appendHandoffPart,
  resolvePendingApprovals, supersedePendingApprovals,
} from '@/lib/streamReducer';

const RUN = 'run-1';

// streamTurn replays a sequence of reducer applications the way useAgentSocket
// does (null = deliberate no-op keeps the previous array) and returns the live
// turn's parts.
function partsOf(msgs: ReturnType<typeof buildTimeline>): TurnEntry['parts'] {
  const turn = msgs.find(m => m.role === 'turn') as TurnEntry | undefined;
  expect(turn, 'expected a turn entry').toBeDefined();
  return turn!.parts;
}

describe('stream/replay isomorphism', () => {
  it('full turn: thinking → tool call → interim narration → final text', () => {
    // --- streaming path: the event order the hub delivers.
    let live = ensureLiveTurn([{ role: 'user', content: 'q', messageId: '1' }], RUN)!;
    live = appendReasoningItem(live, 'pondering', false)!;
    // needs_approval arrives normalized: the socket layer maps the wire's
    // explicit false to undefined so streamed calls match replayed ones.
    live = appendToolCall(live, {
      tool_call_id: 'c1', tool_name: 'search', arguments: '{"q":"x"}',
      needs_approval: undefined, status: null, output: null,
    }, '')!;
    live = applyToolResult(live, 'c1', 'found it')!;
    live = appendMessageItem(live, 'let me check', false)!;
    live = appendMessageItem(live, 'the answer', false)!;
    // run.output carries the final text again; finalizeTurn must dedup it.
    live = finalizeTurn(live, 'the answer', '') ?? live;

    // --- replay path: the rows the backend persists for that same turn.
    const rows: EntryView[] = [
      { id: "1", run_id: RUN, kind: 'item', role: 'user', content: 'q' },
      { id: "2", run_id: RUN, kind: 'item', role: 'assistant', content: 'pondering', display: { kind: 'reasoning', text: 'pondering' } },
      { id: "3", run_id: RUN, kind: 'item', role: 'assistant', content: 'search({"q":"x"})', display: { kind: 'tool_call', call_id: 'c1', tool_name: 'search', arguments: '{"q":"x"}' } },
      { id: "4", run_id: RUN, kind: 'item', role: 'tool', content: 'found it', display: { kind: 'tool_output', call_id: 'c1', output: 'found it' } },
      { id: "5", run_id: RUN, kind: 'item', role: 'assistant', content: 'let me check', display: { kind: 'message', text: 'let me check' } },
      { id: "6", run_id: RUN, kind: 'item', role: 'assistant', content: 'the answer', display: { kind: 'message', text: 'the answer' } },
    ];
    const replayed = buildTimeline(rows);

    const streamParts = (live[live.length - 1] as TurnEntry).parts;
    const replayParts = partsOf(replayed);
    expect(streamParts).toEqual([
      { type: 'thinking', content: 'pondering' },
      { type: 'tools', toolCalls: [{ tool_call_id: 'c1', tool_name: 'search', arguments: '{"q":"x"}', status: 'completed', output: 'found it' }] },
      { type: 'text', content: 'let me check' },
      { type: 'text', content: 'the answer' },
    ]);
    expect(replayParts).toEqual(streamParts);
  });

  it('tool result display overrides: title/summary/renderer/error travel both paths', () => {
    // The tool's result declared how to present itself (ToolResult.Title/
    // Summary/Display/IsError). run.tool_result carries these live and the
    // stored output entry's display carries them on replay — dropping them on
    // either side is how a card renders differently after a refresh.
    let live = ensureLiveTurn([], RUN)!;
    live = appendToolCall(live, {
      tool_call_id: 'c1', tool_name: 'apply_patch', arguments: '{}',
      needs_approval: undefined, status: null, output: null,
    }, '')!;
    live = applyToolResult(live, 'c1', 'patch failed', {
      title: 'Apply patch', summary: '0 of 3 hunks applied', renderer: 'diff', is_error: true,
      extra: { command: 'apply', partial: true },
    })!;

    const rows: EntryView[] = [
      { id: "1", run_id: RUN, kind: 'item', role: 'assistant', content: 'apply_patch({})', display: { kind: 'tool_call', call_id: 'c1', tool_name: 'apply_patch', arguments: '{}' } },
      { id: "2", run_id: RUN, kind: 'item', role: 'tool', content: 'patch failed', display: { kind: 'tool_output', call_id: 'c1', output: 'patch failed', title: 'Apply patch', summary: '0 of 3 hunks applied', renderer: 'diff', is_error: true, extra: { command: 'apply', partial: true } } },
    ];

    const streamParts = (live[live.length - 1] as TurnEntry).parts;
    expect(streamParts).toEqual([
      {
        type: 'tools',
        toolCalls: [{
          tool_call_id: 'c1', tool_name: 'apply_patch', arguments: '{}', status: 'completed',
          output: 'patch failed', title: 'Apply patch', summary: '0 of 3 hunks applied',
          renderer: 'diff', is_error: true, extra: { command: 'apply', partial: true },
        }],
      },
    ]);
    expect(partsOf(buildTimeline(rows))).toEqual(streamParts);
  });

  it('task terminal outcome: live fold equals the replayed call-display update', () => {
    // When a task finishes, the server appends a call-display UPDATE to the
    // spawn entry (title=label, summary, extra.task_id/task_status) and replay
    // folds it into ToolCall.task. applyTaskTerminal is the live counterpart,
    // fed from the task's run events — the spawn card must show the same
    // "task completed" badge and result summary without a reload.
    let live = ensureLiveTurn([], RUN)!;
    live = appendToolCall(live, {
      tool_call_id: 'c1', tool_name: 'spawn_task', arguments: '{"agent":"researcher"}',
      needs_approval: undefined, status: null, output: null,
    }, '')!;
    live = applyToolResult(live, 'c1', 'task_id: t1\nstatus: working')!;
    // A pre-terminal update stays off the card (liveTaskStatus props own it).
    expect(applyTaskTerminal(live, 'c1', { id: 't1', label: 'Research topic', status: 'working' })).toBeNull();
    live = applyTaskTerminal(live, 'c1', { id: 't1', label: 'Research topic', status: 'completed', summary: 'Found 3 sources' })!;
    // A late event cannot move a terminal card.
    expect(applyTaskTerminal(live, 'c1', { id: 't1', status: 'failed' })).toBeNull();

    const rows: EntryView[] = [
      { id: "1", run_id: RUN, kind: 'item', role: 'assistant', content: 'spawn_task({"agent":"researcher"})', display: { kind: 'tool_call', call_id: 'c1', tool_name: 'spawn_task', arguments: '{"agent":"researcher"}', title: 'Research topic', summary: 'Found 3 sources', extra: { task_id: 't1', task_status: 'completed' } } },
      { id: "2", run_id: RUN, kind: 'item', role: 'tool', content: 'task_id: t1\nstatus: working', display: { kind: 'tool_output', call_id: 'c1', output: 'task_id: t1\nstatus: working' } },
    ];

    const streamParts = (live[live.length - 1] as TurnEntry).parts;
    expect(streamParts).toEqual([
      {
        type: 'tools',
        toolCalls: [{
          tool_call_id: 'c1', tool_name: 'spawn_task', arguments: '{"agent":"researcher"}',
          status: 'completed', output: 'task_id: t1\nstatus: working',
          task: { id: 't1', label: 'Research topic', status: 'completed', summary: 'Found 3 sources' },
        }],
      },
    ]);
    expect(partsOf(buildTimeline(rows))).toEqual(streamParts);
  });

  it('task terminal before its spawn card: the fold is recoverable after append', () => {
    // Parent and task runs are delivered on independent subscriptions with no
    // cross-run ordering (a reconnect replays both buffers), so a fast task's
    // terminal event can precede the parent's run.tool_call. The early fold
    // finds no card and reports null — nothing patched, nothing invented; the
    // socket layer re-folds from s.tasks right after appending the card. This
    // pins that recovery: append-then-fold lands the same parts as replay.
    let live = ensureLiveTurn([], RUN)!;
    expect(applyTaskTerminal(live, 'c1', { id: 't1', label: 'Quick job', status: 'failed', summary: 'boom' })).toBeNull();
    live = appendToolCall(live, {
      tool_call_id: 'c1', tool_name: 'spawn_task', arguments: '{}',
      needs_approval: undefined, status: null, output: null,
    }, '')!;
    live = applyTaskTerminal(live, 'c1', { id: 't1', label: 'Quick job', status: 'failed', summary: 'boom' })!;

    const rows: EntryView[] = [
      { id: "1", run_id: RUN, kind: 'item', role: 'assistant', content: 'spawn_task({})', display: { kind: 'tool_call', call_id: 'c1', tool_name: 'spawn_task', arguments: '{}', title: 'Quick job', summary: 'boom', extra: { task_id: 't1', task_status: 'failed' } } },
    ];

    const streamParts = (live[live.length - 1] as TurnEntry).parts;
    expect(streamParts).toEqual([
      {
        type: 'tools',
        toolCalls: [{
          tool_call_id: 'c1', tool_name: 'spawn_task', arguments: '{}', status: null, output: null,
          task: { id: 't1', label: 'Quick job', status: 'failed', summary: 'boom' },
        }],
      },
    ]);
    expect(partsOf(buildTimeline(rows))).toEqual(streamParts);
  });

  it('task retry: the card re-arms and the new outcome lands, matching replay', () => {
    // A retry reopens a task the card already reported as failed. Without
    // re-arming, applyTaskTerminal's no-move-backwards guard drops the second
    // outcome and the card keeps a stale failure — while the Tasks panel and a
    // reload both show it completed.
    let live = ensureLiveTurn([], RUN)!;
    live = appendToolCall(live, {
      tool_call_id: 'c1', tool_name: 'spawn_task', arguments: '{}',
      needs_approval: undefined, status: null, output: null,
    }, '')!;
    live = applyTaskTerminal(live, 'c1', { id: 't1', label: 'Flaky job', status: 'failed', summary: 'rate limited', attempt: 1 })!;
    // The new attempt's run.started.
    live = startTaskAttempt(live, 'c1', 2)!;
    // A replayed run.started for the attempt already shown cannot wipe an
    // outcome: only a run BEYOND the card's attempt is a new one.
    expect(startTaskAttempt(live, 'c1', 2)).toBeNull();
    live = applyTaskTerminal(live, 'c1', { id: 't1', label: 'Flaky job', status: 'completed', summary: 'done', attempt: 2 })!;

    const rows: EntryView[] = [
      // task_summary_attempt mirrors the server's terminal update: the fold's
      // summary belongs to attempt 2, so replay keeps it.
      { id: "1", run_id: RUN, kind: 'item', role: 'assistant', content: 'spawn_task({})', display: { kind: 'tool_call', call_id: 'c1', tool_name: 'spawn_task', arguments: '{}', title: 'Flaky job', summary: 'done', extra: { task_id: 't1', task_status: 'completed', task_attempt: 2, task_summary_attempt: 2 } } },
    ];

    const streamParts = (live[live.length - 1] as TurnEntry).parts;
    expect(streamParts).toEqual([
      {
        type: 'tools',
        toolCalls: [{
          tool_call_id: 'c1', tool_name: 'spawn_task', arguments: '{}', status: null, output: null,
          task: { id: 't1', label: 'Flaky job', status: 'completed', summary: 'done', attempt: 2 },
        }],
      },
    ]);
    expect(partsOf(buildTimeline(rows))).toEqual(streamParts);
  });

  it('task retry: a summary from a voided attempt is dropped on replay', () => {
    // The fold keeps the last NON-EMPTY summary (merge cannot blank), so after
    // a retry the previous attempt's failure text survives in the display
    // beside the new attempt's status. task_summary_attempt is its provenance:
    // older than the card's attempt means a retry voided it, and rendering it
    // would show "Task result: <old failure>" against a task that is working
    // again — or against a later attempt that finished with nothing to say.
    const spawnRow = (extra: Record<string, unknown>): EntryView[] => ([
      { id: "1", run_id: RUN, kind: 'item', role: 'assistant', content: 'spawn_task({})', display: { kind: 'tool_call', call_id: 'c1', tool_name: 'spawn_task', arguments: '{}', title: 'Flaky job', summary: 'rate limited', extra } },
    ]);
    const taskOf = (rows: EntryView[]) => {
      const parts = partsOf(buildTimeline(rows));
      return (parts[0] as { toolCalls: Array<{ task?: { status?: string; summary?: string } }> }).toolCalls[0].task;
    };

    // Reload mid-retry: attempt 2 running, attempt 1's failure still in the fold.
    expect(taskOf(spawnRow({ task_id: 't1', task_status: 'working', task_attempt: 2, task_summary_attempt: 1 })))
      .toEqual({ id: 't1', label: 'Flaky job', status: 'working', summary: undefined, attempt: 2 });
    // Attempt 2 finished with an empty summary: the leftover must not pose as
    // its result.
    expect(taskOf(spawnRow({ task_id: 't1', task_status: 'completed', task_attempt: 2, task_summary_attempt: 1 })))
      .toEqual({ id: 't1', label: 'Flaky job', status: 'completed', summary: undefined, attempt: 2 });
    // A row written before attempts existed carries neither key and keeps its
    // summary — the legacy shape must not regress.
    expect(taskOf(spawnRow({ task_id: 't1', task_status: 'failed' })))
      .toEqual({ id: 't1', label: 'Flaky job', status: 'failed', summary: 'rate limited', attempt: undefined });
  });

  it('task retry: a folded outcome carries its attempt, so a replay cannot wipe it', () => {
    // syncTaskCard is the one way task state becomes card state, and it folds
    // the attempt in with the outcome. When each caller passed the parts
    // separately the live path forgot the attempt, the card read as attempt 1
    // whatever it was, and a replayed run.started for the attempt it was
    // ALREADY showing looked like a new one — wiping a real result.
    let live = ensureLiveTurn([], RUN)!;
    live = appendToolCall(live, {
      tool_call_id: 'c1', tool_name: 'spawn_task', arguments: '{}',
      needs_approval: undefined, status: null, output: null,
    }, '')!;
    live = syncTaskCard(live, 'c1', { id: 't1', label: 'Flaky job', status: 'failed', summary: 'boom', attempt: 1 })!;
    live = syncTaskCard(live, 'c1', { id: 't1', label: 'Flaky job', attempt: 2 })!;
    live = syncTaskCard(live, 'c1', { id: 't1', label: 'Flaky job', status: 'completed', summary: 'done', attempt: 2 })!;

    const card = () => (live[live.length - 1] as TurnEntry).parts[0];
    expect(card()).toEqual({
      type: 'tools',
      toolCalls: [{
        tool_call_id: 'c1', tool_name: 'spawn_task', arguments: '{}', status: null, output: null,
        task: { id: 't1', label: 'Flaky job', status: 'completed', summary: 'done', attempt: 2 },
      }],
    });
    // The replay of the attempt now on the card changes nothing.
    expect(syncTaskCard(live, 'c1', { id: 't1', label: 'Flaky job', attempt: 2 })).toBeNull();
  });

  it('task retry: one sync both re-arms and folds a retry that already ended', () => {
    // What the reconnect sweep sees: the card is on the previous attempt's
    // outcome, the row is a later attempt that has already finished, and no
    // broadcast is coming for either step.
    let live = ensureLiveTurn([], RUN)!;
    live = appendToolCall(live, {
      tool_call_id: 'c1', tool_name: 'spawn_task', arguments: '{}',
      needs_approval: undefined, status: null, output: null,
    }, '')!;
    live = syncTaskCard(live, 'c1', { id: 't1', label: 'Flaky job', status: 'failed', summary: 'boom', attempt: 1 })!;
    live = syncTaskCard(live, 'c1', { id: 't1', label: 'Flaky job', status: 'completed', summary: 'done', attempt: 2 })!;

    expect((live[live.length - 1] as TurnEntry).parts[0]).toEqual({
      type: 'tools',
      toolCalls: [{
        tool_call_id: 'c1', tool_name: 'spawn_task', arguments: '{}', status: null, output: null,
        task: { id: 't1', label: 'Flaky job', status: 'completed', summary: 'done', attempt: 2 },
      }],
    });
  });

  it('task retry: a card that never finished is left alone', () => {
    // Re-arming is for a card showing an outcome. A working card has none, and
    // its badge already comes from the live status props.
    let live = ensureLiveTurn([], RUN)!;
    live = appendToolCall(live, {
      tool_call_id: 'c1', tool_name: 'spawn_task', arguments: '{}',
      needs_approval: undefined, status: null, output: null,
    }, '')!;
    expect(startTaskAttempt(live, 'c1', 2)).toBeNull();
    expect(startTaskAttempt(live, 'nope', 2)).toBeNull();
  });

  it('guardrail-blocked turn: thinking → typed error card', () => {
    let live = ensureLiveTurn([], RUN)!;
    // run.error flushes the reasoning buffer into a part, then the typed card.
    live = appendErrorPart(live, { type: 'error', content: 'blocked', guardrail: 'no_secrets', stage: 'input' }, 'was thinking', '');

    const rows: EntryView[] = [
      { id: "1", run_id: RUN, kind: 'annotation', role: 'assistant', content: 'was thinking', display: { kind: 'reasoning', text: 'was thinking' } },
      { id: "2", run_id: RUN, kind: 'annotation', role: 'system', content: 'blocked', display: { kind: 'error', text: 'blocked', extra: { guardrail: 'no_secrets', stage: 'input' } } },
    ];

    const streamParts = (live[live.length - 1] as TurnEntry).parts;
    expect(streamParts).toEqual([
      { type: 'thinking', content: 'was thinking' },
      { type: 'error', content: 'blocked', guardrail: 'no_secrets', stage: 'input' },
    ]);
    expect(partsOf(buildTimeline(rows))).toEqual(streamParts);
  });

  it('cancelled turn: partial text → cancelled marker', () => {
    let live = ensureLiveTurn([], RUN)!;
    live = appendCancelledPart(live, '', 'partial answer')!;

    const rows: EntryView[] = [
      { id: "1", run_id: RUN, kind: 'annotation', role: 'assistant', content: 'partial answer', display: { kind: 'message', text: 'partial answer' } },
      { id: "2", run_id: RUN, kind: 'annotation', role: 'system', content: '', display: { kind: 'cancelled' } },
    ];

    const streamParts = (live[live.length - 1] as TurnEntry).parts;
    expect(streamParts).toEqual([
      { type: 'text', content: 'partial answer' },
      { type: 'cancelled', content: '' },
    ]);
    expect(partsOf(buildTimeline(rows))).toEqual(streamParts);
  });

  // A pause abandoned by a newer message (invariant 19): the pending card
  // resolves to not run / superseded on both paths, and no cancelled marker
  // follows — the newer turn does. The stored form is the call's display
  // carrying not_run.
  it('abandoned pause: pending call → not run (superseded), no marker', () => {
    let live = ensureLiveTurn([], RUN)!;
    live = appendToolCall(live, { tool_call_id: 'c1', tool_name: 'exec_command', arguments: '{}', needs_approval: true, status: null, output: null }, '')!;
    live = resolvePendingApprovals(live, RUN, 'superseded')!;

    const rows: EntryView[] = [
      { id: "1", run_id: RUN, kind: 'annotation', role: 'assistant', display: { kind: 'tool_call', call_id: 'c1', tool_name: 'exec_command', arguments: '{}', extra: { not_run: 'superseded' } } },
    ];
    const streamParts = (live[live.length - 1] as TurnEntry).parts;
    expect(streamParts).toEqual([
      { type: 'tools', toolCalls: [{ tool_call_id: 'c1', tool_name: 'exec_command', arguments: '{}', needs_approval: true, status: 'not_run', not_run: 'superseded', output: null }] },
    ]);
    const replayed = partsOf(buildTimeline(rows));
    // needs_approval is a live-only flag (the stored call carries not_run
    // instead); everything else matches.
    expect(replayed).toEqual([{ type: 'tools', toolCalls: [{ tool_call_id: 'c1', tool_name: 'exec_command', arguments: '{}', status: 'not_run', not_run: 'superseded', output: null }] }]);
    // A newer run's start settles the older run's pending card the same way.
    let other = ensureLiveTurn([], RUN)!;
    other = appendToolCall(other, { tool_call_id: 'c2', tool_name: 'exec_command', arguments: '{}', needs_approval: true, status: null, output: null }, '')!;
    other = ensureLiveTurn(other, 'run-2', 'next')!;
    other = supersedePendingApprovals(other, 'run-2')!;
    expect(findToolCall(other, 'c2')?.status).toBe('not_run');
    expect(supersedePendingApprovals(other, 'run-2')).toBeNull();
  });

  // The same pause stopped by an explicit cancel: not run / stopped, and the
  // cancelled marker a stop always leaves.
  it('abandoned pause: pending call → not run (stopped) → cancelled marker', () => {
    let live = ensureLiveTurn([], RUN)!;
    live = appendToolCall(live, { tool_call_id: 'c1', tool_name: 'exec_command', arguments: '{}', needs_approval: true, status: null, output: null }, '')!;
    live = resolvePendingApprovals(live, RUN, 'stopped')!;
    live = appendCancelledPart(live, '', '')!;

    const rows: EntryView[] = [
      { id: "1", run_id: RUN, kind: 'annotation', role: 'assistant', display: { kind: 'tool_call', call_id: 'c1', tool_name: 'exec_command', arguments: '{}', extra: { not_run: 'stopped' } } },
      { id: "2", run_id: RUN, kind: 'annotation', role: 'system', content: '', display: { kind: 'cancelled' } },
    ];
    const streamParts = (live[live.length - 1] as TurnEntry).parts;
    expect(streamParts[1]).toEqual({ type: 'cancelled', content: '' });
    const replayed = partsOf(buildTimeline(rows));
    expect(replayed[0]).toEqual({ type: 'tools', toolCalls: [{ tool_call_id: 'c1', tool_name: 'exec_command', arguments: '{}', status: 'not_run', not_run: 'stopped', output: null }] });
    expect(replayed[1]).toEqual(streamParts[1]);
  });

  it('failed turn: partial text renders as prose whatever role the server sent', () => {
    // A mid-stream provider failure (e.g. content inspection): savePartialTurn
    // wrote the streamed text as an annotation. Older servers mapped its role
    // to "system" — the display kind, not the role, decides it is prose; it
    // must never collapse into a single-line system chip.
    const rows: EntryView[] = [
      { id: "1", run_id: RUN, kind: 'annotation', role: 'system', content: '核实完毕 **增补**', display: { kind: 'message', text: '核实完毕 **增补**' } },
      { id: "2", run_id: RUN, kind: 'annotation', role: 'system', content: 'stream failed', display: { kind: 'error', text: 'stream failed' } },
    ];
    expect(partsOf(buildTimeline(rows))).toEqual([
      { type: 'text', content: '核实完毕 **增补**' },
      { type: 'error', content: 'stream failed', guardrail: undefined, stage: undefined },
    ]);
  });

  it('documented difference: handoff parts are live-only', () => {
    let live = ensureLiveTurn([], RUN)!;
    live = appendHandoffPart(live, { from: 'triage', to: 'coder', fromId: 'id-t', toId: 'id-c' })!;
    const streamParts = (live[live.length - 1] as TurnEntry).parts;
    expect(streamParts).toEqual([{ type: 'handoff', content: 'triage → coder', from: 'triage', to: 'coder', fromId: 'id-t', toId: 'id-c' }]);

    // The backend persists no handoff row — the transfer_to_* tool call is the
    // durable record. A replay therefore has no handoff part, by design.
    const rows: EntryView[] = [
      { id: "1", run_id: RUN, kind: 'item', role: 'assistant', content: 'transfer_to_coder({})', display: { kind: 'tool_call', call_id: 'h1', tool_name: 'transfer_to_coder', arguments: '{}' } },
      { id: "2", run_id: RUN, kind: 'item', role: 'tool', content: 'ok', display: { kind: 'tool_output', call_id: 'h1', output: 'ok' } },
    ];
    const replayParts = partsOf(buildTimeline(rows));
    expect(replayParts.some(p => p.type === 'handoff')).toBe(false);
    expect(replayParts).toEqual([
      { type: 'tools', toolCalls: [{ tool_call_id: 'h1', tool_name: 'transfer_to_coder', arguments: '{}', status: 'completed', output: 'ok' }] },
    ]);
  });

  it('documented difference: rejected status is live-only, replay shows completed', () => {
    let _live = ensureLiveTurn([], RUN)!;
    _live = appendToolCall(_live, {
      tool_call_id: 'c1', tool_name: 'rm', arguments: '{}',
      needs_approval: true, status: null, output: null,
    }, '')!;
    // User rejects (optimistic patch in app.tsx sets the status)…
    _live = applyToolResult(_live, 'c1', 'rejected by user')!;
    // …but here the status was still null when the result landed, so it
    // completes. Simulate the real order: status set BEFORE the result.
    let live2 = ensureLiveTurn([], RUN)!;
    live2 = appendToolCall(live2, {
      tool_call_id: 'c1', tool_name: 'rm', arguments: '{}',
      needs_approval: true, status: 'rejected', output: null,
    }, '')!;
    live2 = applyToolResult(live2, 'c1', 'rejected by user')!;
    const streamCall = ((live2[live2.length - 1] as TurnEntry).parts[0] as { toolCalls: Array<{ status: string | null }> }).toolCalls[0];
    expect(streamCall.status).toBe('rejected'); // the red badge survives the resume's tool_output

    // Replay: per-call status is not persisted, so the same call reads
    // 'completed' — the rejection is only visible in the output text.
    const rows: EntryView[] = [
      { id: "1", run_id: RUN, kind: 'item', role: 'assistant', content: 'rm({})', display: { kind: 'tool_call', call_id: 'c1', tool_name: 'rm', arguments: '{}' } },
      { id: "2", run_id: RUN, kind: 'item', role: 'tool', content: 'rejected by user', display: { kind: 'tool_output', call_id: 'c1', output: 'rejected by user' } },
    ];
    const replayCall = (partsOf(buildTimeline(rows))[0] as { toolCalls: Array<{ status: string | null }> }).toolCalls[0];
    expect(replayCall.status).toBe('completed');
  });

  it('broadcast prologue: a watching browser builds the user bubble from run.started input', () => {
    // Browser B never sent the prompt — the bubble comes from the event.
    const watcher = ensureLiveTurn([], RUN, 'hello')!;
    expect(watcher).toEqual([
      { role: 'user', content: 'hello', runId: RUN },
      { role: 'turn', parts: [], runId: RUN },
    ]);
    // The sender's optimistic bubble is already trailing — no duplicate.
    const sender = ensureLiveTurn([{ role: 'user', content: 'hello' }], RUN, 'hello')!;
    expect(sender.filter(m => m.role === 'user')).toHaveLength(1);
  });

  it('mergeLiveTail: history fetched after live events keeps both sides', () => {
    // Browser B joins mid-run: broadcast events land first (loaded=true),
    // then the history fetch resolves. The live tail (no messageId) must be
    // re-appended after the persisted rows.
    const persisted = buildTimeline([
      { id: "1", run_id: 'old', kind: 'item', role: 'user', content: 'earlier q' },
      { id: "2", run_id: 'old', kind: 'item', role: 'assistant', content: 'earlier a', display: { kind: 'message', text: 'earlier a' } },
    ]);
    let live = ensureLiveTurn([], RUN, 'new q')!;
    live = appendMessageItem(live, 'streaming…', false)!;
    const merged = mergeLiveTail(persisted, live, RUN);
    expect(merged.map(m => m.role)).toEqual(['user', 'turn', 'user', 'turn']);
    expect((merged[2] as { content?: string }).content).toBe('new q');

    // Entries the store already covers are deduped: the run's user prompt
    // persisted while the fetch was in flight must not double up.
    const persistedWithPrompt = buildTimeline([
      { id: "1", run_id: RUN, kind: 'item', role: 'user', content: 'new q' },
    ]);
    const merged2 = mergeLiveTail(persistedWithPrompt, live, RUN);
    expect(merged2.filter(m => m.role === 'user')).toHaveLength(1);
    expect(merged2.filter(m => m.role === 'turn')).toHaveLength(1);
  });

  it('mergeLiveTail: only the CURRENT live run\'s turn survives the merge', () => {
    // The tail holds a finished (or branched-away) turn that never got its
    // messageId stamped — e.g. a regenerate raced the terminal reload. The
    // fetched timeline already pruned it; the merge must not put it back.
    const persisted = buildTimeline([
      { id: "1", run_id: 'run-old', kind: 'item', role: 'user', content: 'hello', entry_id: 'u1' },
    ]);
    const stale: TimelineEntry[] = [
      { role: 'user', content: 'hello', clientMsgId: 'c1' },
      { role: 'turn', parts: [{ type: 'text', content: 'OLD ANSWER' }], runId: 'run-old' },
    ];
    // No live run: the stale turn is dropped, the bubble dedups onto its row.
    expect(mergeLiveTail(persisted, stale, null)).toEqual(persisted);
    // A different run is live: the stale turn still does not come back.
    const merged = mergeLiveTail(persisted, [...stale, { role: 'turn', parts: [], runId: RUN }], RUN);
    expect(merged.filter(m => m.role === 'turn').map(m => (m as TurnEntry).runId)).toEqual([RUN]);
  });

  it('mergeLiveTail: two identical optimistic sends both survive one persisted copy', () => {
    // First "x" is already persisted; two optimistic "x" bubbles (distinct
    // clientMsgIds, no messageId) sit in the live tail. Content-only dedup is
    // one-to-one: one bubble consumes the persisted copy, the second must NOT
    // also collapse onto it — that used to drop the genuine second send.
    const persisted = buildTimeline([
      { id: "1", run_id: 'r0', kind: 'item', role: 'user', content: 'x' },
    ]);
    const live: TimelineEntry[] = [
      { role: 'user', content: 'x', clientMsgId: 'c1' },
      { role: 'user', content: 'x', clientMsgId: 'c2' },
    ];
    const merged = mergeLiveTail(persisted, live);
    expect(merged.filter(m => m.role === 'user')).toHaveLength(2);
  });

  // An input the run reads from its queue (a steer, a follow-up) is a user
  // entry in the middle of the run: the store cuts the turn there, and the
  // live view must cut it at the same place — one run, two turns.
  it('a consumed injection splits the live turn exactly where reload puts it', () => {
    let live = ensureLiveTurn([], RUN, 'deploy')!;
    live = appendMessageItem(live, 'deploying to prod', false)!;
    live = appendInjected(live, RUN, 'use staging', 1)!;
    live = appendMessageItem(live, 'switched to staging', false)!;
    live = finalizeTurn(live, 'switched to staging', '') || live;

    const rows: EntryView[] = [
      { id: "1", run_id: RUN, kind: 'item', role: 'user', content: 'deploy' },
      { id: "2", run_id: RUN, kind: 'item', role: 'assistant', content: 'deploying to prod', display: { kind: 'message', text: 'deploying to prod' } },
      { id: "3", run_id: RUN, kind: 'item', role: 'user', content: 'use staging' },
      { id: "4", run_id: RUN, kind: 'item', role: 'assistant', content: 'switched to staging', display: { kind: 'message', text: 'switched to staging' } },
    ];
    // What a turn or a bubble shows, without the ids only one side has.
    const shape = (msgs: TimelineEntry[]) => msgs.map(m => m.role === 'turn'
      ? { role: m.role, runId: m.runId, parts: m.parts }
      : { role: m.role, runId: (m as { runId?: string }).runId, content: (m as { content?: string }).content });
    expect(shape(live)).toEqual(shape(buildTimeline(rows)));
    expect(shape(live).map(m => m.role)).toEqual(['user', 'turn', 'user', 'turn']);

    // A hub replay of the same injection adds nothing; a second one is new.
    expect(appendInjected(live, RUN, 'use staging', 1)).toBeNull();
    expect(appendInjected(live, RUN, 'use staging', 2)!.filter(m => m.role === 'user')).toHaveLength(3);
    // The store already holds the injection but not the turn after it (a
    // reload between the two): the replay adds the turn alone.
    const stored = buildTimeline(rows.slice(0, 3));
    const resumed = appendInjected(stored, RUN, 'use staging', 1)!;
    expect(resumed.map(m => m.role)).toEqual(['user', 'turn', 'user', 'turn']);
    expect(resumed.filter(m => m.role === 'user')).toHaveLength(2);
  });

  // The live rows of a split run share its id: two turns keyed alike made
  // React keep a ghost of one when the stored rows replaced them.
  it('a split run renders under distinct keys, live and stored', () => {
    let live = ensureLiveTurn([], RUN, 'deploy')!;
    live = appendMessageItem(live, 'deploying', false)!;
    live = appendInjected(live, RUN, 'use staging', 1)!;
    live = appendMessageItem(live, 'switching', false)!;
    live = appendInjected(live, RUN, 'and tag it', 2)!;
    const keys = rowKeys(live);
    expect(new Set(keys).size).toBe(keys.length);
    expect(keys).toEqual(['user-r' + RUN, 'turn-r' + RUN, 'user-r' + RUN + '~1', 'turn-r' + RUN + '~1', 'user-r' + RUN + '~2', 'turn-r' + RUN + '~2']);
    // Stored rows carry their own ids, an optimistic bubble its client id, and
    // a row with neither falls back to its position.
    expect(rowKeys([
      { role: 'user', content: 'q', messageId: '7', runId: RUN },
      { role: 'turn', parts: [], messageId: '8', runId: RUN },
      { role: 'user', content: 'next', clientMsgId: 'c3' },
      { role: 'system', content: 'note' },
    ] as TimelineEntry[])).toEqual(['user-m7', 'turn-m8', 'user-cc3', 'msg-i3']);
  });

  it('mergeLiveTail: a run an injection split keeps the turn the store does not have yet', () => {
    let live = ensureLiveTurn([], RUN, 'deploy')!;
    live = appendMessageItem(live, 'deploying to prod', false)!;
    live = appendInjected(live, RUN, 'use staging', 1)!;
    live = appendMessageItem(live, 'switching…', false)!;
    const rows: EntryView[] = [
      { id: "1", run_id: RUN, kind: 'item', role: 'user', content: 'deploy' },
      { id: "2", run_id: RUN, kind: 'item', role: 'assistant', content: 'deploying to prod', display: { kind: 'message', text: 'deploying to prod' } },
      { id: "3", run_id: RUN, kind: 'item', role: 'user', content: 'use staging' },
    ];
    // The store has the first turn and the injected message: its rows win for
    // those, and the live second turn follows them.
    const merged = mergeLiveTail(buildTimeline(rows), live, RUN);
    expect(merged.map(m => m.role)).toEqual(['user', 'turn', 'user', 'turn']);
    expect(merged.slice(0, 3).every(m => m.messageId !== undefined)).toBe(true);
    expect((merged[3] as TurnEntry).parts).toEqual([{ type: 'text', content: 'switching…' }]);
    // Fetched before the injection was stored: the live bubble stays, after
    // the stored first turn, with the live second turn behind it.
    const early = mergeLiveTail(buildTimeline(rows.slice(0, 2)), live, RUN);
    expect(early.map(m => m.role)).toEqual(['user', 'turn', 'user', 'turn']);
    expect((early[2] as { content?: string }).content).toBe('use staging');
    expect((early[3] as TurnEntry).parts).toEqual([{ type: 'text', content: 'switching…' }]);
  });

  // Two inputs the run read at one save point follow each other in the store
  // with nothing between them; the live view must not leave an empty turn there.
  it('two inputs read at one point sit side by side, live as on reload', () => {
    let live = ensureLiveTurn([], RUN, 'deploy')!;
    live = appendMessageItem(live, 'deploying', false)!;
    live = appendInjected(live, RUN, 'use staging', 1)!;
    live = appendInjected(live, RUN, 'and tag it', 2)!;
    live = appendMessageItem(live, 'staging, tagged', false)!;
    const rows: EntryView[] = [
      { id: "1", run_id: RUN, kind: 'item', role: 'user', content: 'deploy' },
      { id: "2", run_id: RUN, kind: 'item', role: 'assistant', content: 'deploying', display: { kind: 'message', text: 'deploying' } },
      { id: "3", run_id: RUN, kind: 'item', role: 'user', content: 'use staging' },
      { id: "4", run_id: RUN, kind: 'item', role: 'user', content: 'and tag it' },
      { id: "5", run_id: RUN, kind: 'item', role: 'assistant', content: 'staging, tagged', display: { kind: 'message', text: 'staging, tagged' } },
    ];
    expect(live.map(m => m.role)).toEqual(['user', 'turn', 'user', 'user', 'turn']);
    expect(live.map(m => m.role)).toEqual(buildTimeline(rows).map(m => m.role));
    // Once the store holds all of it, nothing of the live copy is left over.
    expect(mergeLiveTail(buildTimeline(rows), live, RUN)).toEqual(buildTimeline(rows));
    // A replay of either injection changes nothing.
    expect(appendInjected(live, RUN, 'use staging', 1)).toBeNull();
    expect(appendInjected(live, RUN, 'and tag it', 2)).toBeNull();
  });

  // A tab that loaded the store mid-run shows the run's first turn stamped;
  // the turn after an injection is live. A resync must not count the stamped
  // turn against the live one, or the view freezes until the run ends.
  it('mergeLiveTail: a turn the timeline already shows stamped does not cover the live one', () => {
    const rows: EntryView[] = [
      { id: "1", run_id: RUN, kind: 'item', role: 'user', content: 'deploy' },
      { id: "2", run_id: RUN, kind: 'item', role: 'assistant', content: 'deploying', display: { kind: 'message', text: 'deploying' } },
    ];
    let cur = buildTimeline(rows);
    cur = appendInjected(cur, RUN, 'use staging', 1)!;
    cur = appendMessageItem(cur, 'switching', false)!;
    const merged = mergeLiveTail(buildTimeline(rows), cur, RUN);
    expect(merged.map(m => m.role)).toEqual(['user', 'turn', 'user', 'turn']);
    expect((merged[3] as TurnEntry).parts).toEqual([{ type: 'text', content: 'switching' }]);
    // The run's next item still has a live turn to land in, and a second
    // resync changes nothing.
    expect(appendMessageItem(merged, 'done', false)).not.toBeNull();
    expect(mergeLiveTail(buildTimeline(rows), merged, RUN)).toEqual(merged);
  });

  // An injected message that repeats the prompt's text is not the prompt.
  it('mergeLiveTail: an injection that repeats the prompt is not covered by the stored prompt', () => {
    let live = ensureLiveTurn([], RUN, 'continue')!;
    live = appendMessageItem(live, 'step 1', false)!;
    live = appendInjected(live, RUN, 'continue', 1)!;
    live = appendMessageItem(live, 'step 2', false)!;
    const rows: EntryView[] = [
      { id: "1", run_id: RUN, kind: 'item', role: 'user', content: 'continue' },
      { id: "2", run_id: RUN, kind: 'item', role: 'assistant', content: 'step 1', display: { kind: 'message', text: 'step 1' } },
    ];
    const merged = mergeLiveTail(buildTimeline(rows), live, RUN);
    expect(merged.map(m => m.role)).toEqual(['user', 'turn', 'user', 'turn']);
    expect((merged[2] as { injected?: number }).injected).toBe(1);
    // Stored, it covers the bubble — one row, not two.
    const stored = [...rows, { id: "3", run_id: RUN, kind: 'item', role: 'user', content: 'continue' } as EntryView];
    expect(mergeLiveTail(buildTimeline(stored), live, RUN).filter(m => m.role === 'user')).toHaveLength(2);
  });

  // A hub replay re-delivers the run from its start: what an earlier turn of
  // the split run shows is not appended to the newest one.
  it('replay dedup reaches the turns before a split', () => {
    let live = ensureLiveTurn([], RUN, 'deploy')!;
    live = appendHandoffPart(live, { from: 'A', to: 'B' })!;
    live = appendMessageItem(live, 'deploying', true)!;
    live = appendReasoningItem(live, 'thinking it over', true)!;
    live = appendInjected(live, RUN, 'use staging', 1)!;
    expect(appendHandoffPart(live, { from: 'A', to: 'B' })).toBeNull();
    expect(appendMessageItem(live, 'deploying', true)).toBeNull();
    expect(appendReasoningItem(live, 'thinking it over', true)).toBeNull();
    // What the new turn has not shown still lands.
    expect(appendMessageItem(live, 'switching', true)).not.toBeNull();
  });

  it('replay dedup: re-delivered items and repeated run.started do not duplicate', () => {
    // Hub replays after a reconnect re-run the same events; the reducers must
    // be idempotent the same way the timeline rebuild inherently is.
    let live = ensureLiveTurn([], RUN)!;
    expect(ensureLiveTurn(live, RUN)).toBeNull(); // second run.started: no second turn
    live = appendMessageItem(live, 'hello', false)!;
    expect(appendMessageItem(live, 'hello', true)).toBeNull(); // no-item-id text replay
    live = appendHandoffPart(live, { from: 'a', to: 'b' })!;
    expect(appendHandoffPart(live, { from: 'a', to: 'b' })).toBeNull(); // handoff replay
    live = appendCancelledPart(live, '', '')!;
    const again = appendCancelledPart(live, '', '')!;
    const parts = (again[again.length - 1] as TurnEntry).parts;
    expect(parts.filter(p => p.type === 'cancelled')).toHaveLength(1); // marker idempotent
  });

  it('compaction: folded history renders in place, the checkpoint is an inline marker', () => {
    const timeline = buildTimeline([
      { id: "1", entry_id: 'e1', kind: 'item', role: 'user', content: 'old question' },
      { id: "2", entry_id: 'e2', kind: 'item', role: 'assistant', content: 'old answer', display: { kind: 'message', text: 'old answer' } },
      { id: "3", entry_id: 'e3', kind: 'compaction', role: 'compaction', content: 'summary of the above', compaction: { excluded_ids: ['e1', 'e2'], tokens_before: 12400, tokens_after: 3100 } },
      { id: "4", entry_id: 'e4', kind: 'item', role: 'user', content: 'new question' },
    ]);
    // The transcript is decoupled from the fold: history stays loose and in
    // full, the marker sits where the pass happened.
    expect(timeline.map(m => m.role)).toEqual(['user', 'turn', 'compaction', 'user']);
    const cp = timeline[2] as { tokensBefore?: number; tokensAfter?: number };
    expect(cp.tokensBefore).toBe(12400);
    expect(cp.tokensAfter).toBe(3100);
  });

  it('compaction: a reset checkpoint carries its flag to the marker', () => {
    const timeline = buildTimeline([
      { id: "1", entry_id: 'e1', kind: 'item', role: 'user', content: 'old question' },
      { id: "2", entry_id: 'e2', kind: 'compaction', role: 'compaction', content: 'the session memory', compaction: { excluded_ids: ['e1'], reset: true } },
      { id: "3", entry_id: 'e3', kind: 'item', role: 'user', content: 'new question' },
    ]);
    expect(timeline.map(m => m.role)).toEqual(['user', 'compaction', 'user']);
    expect((timeline[1] as { reset?: boolean }).reset).toBe(true);
  });

  it('compaction: a second pass leaves the first marker and the history in place', () => {
    const timeline = buildTimeline([
      { id: "1", entry_id: 'e1', kind: 'item', role: 'user', content: 'oldest' },
      { id: "2", entry_id: 'e2', kind: 'compaction', role: 'compaction', content: 'first summary', compaction: { excluded_ids: ['e1'] } },
      { id: "3", entry_id: 'e3', kind: 'item', role: 'user', content: 'middle' },
      { id: "4", entry_id: 'e4', kind: 'compaction', role: 'compaction', content: 'second summary', compaction: { excluded_ids: ['e1', 'e2', 'e3'] } },
    ]);
    expect(timeline.map(m => m.role)).toEqual(['user', 'compaction', 'user', 'compaction']);
    expect((timeline[3] as { content: string }).content).toBe('second summary');
  });

  it('compaction: an entry marked compacted stays in place', () => {
    // Soft-deleted from the model's context, not from what happened.
    const timeline = buildTimeline([
      { id: "1", entry_id: 'e1', kind: 'item', role: 'user', content: 'orphaned', compacted: true },
      { id: "2", entry_id: 'e2', kind: 'item', role: 'user', content: 'current' },
    ]);
    expect(timeline.map(m => (m as { content?: string }).content)).toEqual(['orphaned', 'current']);
  });

  it('branching: the abandoned attempt leaves the timeline but stays offerable', () => {
    // One question, answered twice. e2 was abandoned; e4 is current.
    const timeline = buildTimeline([
      { id: "1", entry_id: 'e1', kind: 'item', role: 'user', content: 'question', on_path: true },
      { id: "2", entry_id: 'e2', parent_id: 'e1', kind: 'item', role: 'assistant', content: 'first', display: { kind: 'message', text: 'first' }, on_path: false },
      { id: "3", entry_id: 'e3', parent_id: 'e2', kind: 'leaf', role: 'assistant', on_path: false },
      { id: "4", entry_id: 'e4', parent_id: 'e1', kind: 'item', role: 'assistant', content: 'second', display: { kind: 'message', text: 'second' }, on_path: true },
    ]);
    // Both answers inline would be a conversation that never happened.
    expect(timeline.map(m => m.role)).toEqual(['user', 'turn']);
    const turn = timeline[1] as TurnEntry;
    expect(turn.parts).toEqual([{ type: 'text', content: 'second' }]);
    // …but the switcher knows about both, and where to switch to.
    // The tip is the attempt's last CONTENT entry — e2, not the leaf marker
    // e3 that the switch away from it appended.
    expect(turn.branches).toEqual({ parentId: 'e1', tips: ['e2', 'e4'], active: 1 });
  });

  it('branching: an abandoned only-child is pruned before the new attempt exists', () => {
    // The regenerate window: branch switched back to the user message, the new
    // run has not persisted anything yet. The old answer is the user entry's
    // ONLY child (the switch's leaf is not one), so no fork exists — the
    // off-path filter must apply anyway, or the replaced answer stays on
    // screen for the whole regeneration.
    const timeline = buildTimeline([
      { id: "1", entry_id: 'e1', kind: 'item', role: 'user', content: 'question', on_path: true },
      { id: "2", entry_id: 'e2', parent_id: 'e1', kind: 'item', role: 'assistant', content: 'old answer', display: { kind: 'message', text: 'old answer' }, on_path: false },
      { id: "3", entry_id: 'e3', parent_id: 'e1', kind: 'leaf', role: 'user', on_path: true },
    ]);
    expect(timeline.map(m => m.role)).toEqual(['user']);
  });

  it('branching: a leaf entry is not an attempt', () => {
    // Every branch switch appends a leaf entry at whatever the tip was.
    // Counting those as children invents a fork at each switch.
    const timeline = buildTimeline([
      { id: "1", entry_id: 'e1', kind: 'item', role: 'user', content: 'q', on_path: true },
      { id: "2", entry_id: 'e2', parent_id: 'e1', kind: 'item', role: 'assistant', content: 'a', display: { kind: 'message', text: 'a' }, on_path: true },
      { id: "3", entry_id: 'e3', parent_id: 'e2', kind: 'leaf', role: 'assistant', on_path: true },
    ]);
    expect((timeline[1] as TurnEntry).branches).toBeUndefined();
  });

  it('task display projection: a patched spawn_task call rebuilds its task card', () => {
    // onTaskUpdate appends an update entry carrying task_* for the spawn
    // call; the server folds it into the call's display before the client
    // sees it. This is deliberately replay-only (no streamed counterpart): while
    // the task is live the chips row carries its status, so the isomorphism
    // contract does not extend to these fields.
    const timeline = buildTimeline([
      { id: "1", run_id: RUN, kind: 'item', role: 'user', content: 'spawn something' },
      { id: "2", run_id: RUN, kind: 'item', role: 'assistant', display: { kind: 'tool_call', call_id: 'c1', tool_name: 'spawn_task', arguments: '{}', title: 'audit', summary: 'all green', extra: { task_id: 't1', task_status: 'completed' } } },
      { id: "3", run_id: RUN, kind: 'item', role: 'tool', display: { kind: 'tool_output', call_id: 'c1', output: '{"task_id":"t1"}' } },
    ]);
    const turn = timeline[1] as TurnEntry;
    const tools = turn.parts.find(p => p.type === 'tools') as { toolCalls: Array<{ task?: { id?: string; status?: string; summary?: string } }> };
    expect(tools.toolCalls[0].task).toEqual({ id: 't1', label: 'audit', status: 'completed', summary: 'all green' });
  });
});

describe('workflow-started note', () => {
  it('replays as a system row carrying the note, its own turn boundary', () => {
    const rows: EntryView[] = [
      { id: "1", kind: 'annotation', role: 'system', content: 'Workflow "build" started by you', display: {
        kind: 'workflow_started', text: 'Workflow "build" started by you',
        extra: { task_id: 't1', workflow_id: 'w1', workflow_name: 'build', brief: 'ship it', origin: { kind: 'person' } },
      } },
      { id: "2", run_id: 'w-run', kind: 'item', role: 'user', content: '[task-notification] Task "build" (t1) completed. Result: done' },
      { id: "3", run_id: 'w-run', kind: 'item', role: 'assistant', content: 'It finished.', display: { kind: 'message', text: 'It finished.' } },
    ];
    const msgs = buildTimeline(rows);
    expect(msgs.map(m => m.role)).toEqual(['system', 'user', 'turn']);
    const note = (msgs[0] as { note?: unknown }).note;
    expect(note).toEqual({ taskId: 't1', workflowId: 'w1', workflowName: 'build', brief: 'ship it', origin: { kind: 'person' } });
    // A note with no extra still renders as the plain line, never crashes.
    const bare = buildTimeline([{ id: "1", kind: 'annotation', role: 'system', content: 'Workflow "x" started by you', display: { kind: 'workflow_started' } }]);
    expect((bare[0] as { note?: { taskId: string } }).note?.taskId).toBe('');
    expect((bare[0] as { content?: string }).content).toBe('Workflow "x" started by you');
  });
});

describe('host notes', () => {
  // A host annotation of a kind the client has no card for still shows: a
  // system row carrying the note's text (the container-rebuilt note is one).
  it('an unknown host note renders as a system row with its text', () => {
    const rows: EntryView[] = [
      { id: "1", kind: 'annotation', role: 'system', content: 'Container rebuilt for "p": processes and anything outside /workspace are gone', display: { kind: 'container_rebuilt', text: 'Container rebuilt for "p": processes and anything outside /workspace are gone', extra: { project_id: 'p1' } } },
    ];
    const out = buildTimeline(rows);
    expect(out).toHaveLength(1);
    expect(out[0].role).toBe('system');
    expect((out[0] as { content?: string }).content).toContain('Container rebuilt');
  });
});
