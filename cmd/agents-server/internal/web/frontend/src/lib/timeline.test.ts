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
// with the README's design invariants):
//   1. handoff parts are live-only — a reload conveys the transfer via the
//      transfer_to_* tool-call card instead.
//   2. a user-rejected tool call keeps status 'rejected' live, but replays as
//      'completed' — per-call status is not persisted; the rejection notice
//      survives in the call's output text.
import { describe, it, expect } from 'vitest';
import { buildTimeline, type EntryView, type TurnEntry } from '@/lib/timeline';
import {
  ensureLiveTurn, mergeLiveTail, appendMessageItem, appendReasoningItem, finalizeTurn,
  appendErrorPart, appendCancelledPart, appendToolCall, applyToolResult, applyTaskTerminal, startTaskAttempt, appendHandoffPart,
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
    let live = ensureLiveTurn([{ role: 'user', content: 'q', messageId: 1 }], RUN)!;
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
      { id: 1, run_id: RUN, kind: 'item', role: 'user', content: 'q' },
      { id: 2, run_id: RUN, kind: 'item', role: 'assistant', content: 'pondering', display: { kind: 'reasoning', text: 'pondering' } },
      { id: 3, run_id: RUN, kind: 'item', role: 'assistant', content: 'search({"q":"x"})', display: { kind: 'tool_call', call_id: 'c1', tool_name: 'search', arguments: '{"q":"x"}' } },
      { id: 4, run_id: RUN, kind: 'item', role: 'tool', content: 'found it', display: { kind: 'tool_output', call_id: 'c1', output: 'found it' } },
      { id: 5, run_id: RUN, kind: 'item', role: 'assistant', content: 'let me check', display: { kind: 'message', text: 'let me check' } },
      { id: 6, run_id: RUN, kind: 'item', role: 'assistant', content: 'the answer', display: { kind: 'message', text: 'the answer' } },
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
      { id: 1, run_id: RUN, kind: 'item', role: 'assistant', content: 'apply_patch({})', display: { kind: 'tool_call', call_id: 'c1', tool_name: 'apply_patch', arguments: '{}' } },
      { id: 2, run_id: RUN, kind: 'item', role: 'tool', content: 'patch failed', display: { kind: 'tool_output', call_id: 'c1', output: 'patch failed', title: 'Apply patch', summary: '0 of 3 hunks applied', renderer: 'diff', is_error: true, extra: { command: 'apply', partial: true } } },
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
      { id: 1, run_id: RUN, kind: 'item', role: 'assistant', content: 'spawn_task({"agent":"researcher"})', display: { kind: 'tool_call', call_id: 'c1', tool_name: 'spawn_task', arguments: '{"agent":"researcher"}', title: 'Research topic', summary: 'Found 3 sources', extra: { task_id: 't1', task_status: 'completed' } } },
      { id: 2, run_id: RUN, kind: 'item', role: 'tool', content: 'task_id: t1\nstatus: working', display: { kind: 'tool_output', call_id: 'c1', output: 'task_id: t1\nstatus: working' } },
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
      { id: 1, run_id: RUN, kind: 'item', role: 'assistant', content: 'spawn_task({})', display: { kind: 'tool_call', call_id: 'c1', tool_name: 'spawn_task', arguments: '{}', title: 'Quick job', summary: 'boom', extra: { task_id: 't1', task_status: 'failed' } } },
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
      { id: 1, run_id: RUN, kind: 'item', role: 'assistant', content: 'spawn_task({})', display: { kind: 'tool_call', call_id: 'c1', tool_name: 'spawn_task', arguments: '{}', title: 'Flaky job', summary: 'done', extra: { task_id: 't1', task_status: 'completed', task_attempt: 2 } } },
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
      { id: 1, run_id: RUN, kind: 'annotation', role: 'assistant', content: 'was thinking', display: { kind: 'reasoning', text: 'was thinking' } },
      { id: 2, run_id: RUN, kind: 'annotation', role: 'system', content: 'blocked', display: { kind: 'error', text: 'blocked', extra: { guardrail: 'no_secrets', stage: 'input' } } },
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
      { id: 1, run_id: RUN, kind: 'annotation', role: 'assistant', content: 'partial answer', display: { kind: 'message', text: 'partial answer' } },
      { id: 2, run_id: RUN, kind: 'annotation', role: 'system', content: '', display: { kind: 'cancelled' } },
    ];

    const streamParts = (live[live.length - 1] as TurnEntry).parts;
    expect(streamParts).toEqual([
      { type: 'text', content: 'partial answer' },
      { type: 'cancelled', content: '' },
    ]);
    expect(partsOf(buildTimeline(rows))).toEqual(streamParts);
  });

  it('failed turn: partial text renders as prose whatever role the server sent', () => {
    // A mid-stream provider failure (e.g. content inspection): savePartialTurn
    // wrote the streamed text as an annotation. Older servers mapped its role
    // to "system" — the display kind, not the role, decides it is prose; it
    // must never collapse into a single-line system chip.
    const rows: EntryView[] = [
      { id: 1, run_id: RUN, kind: 'annotation', role: 'system', content: '核实完毕 **增补**', display: { kind: 'message', text: '核实完毕 **增补**' } },
      { id: 2, run_id: RUN, kind: 'annotation', role: 'system', content: 'stream failed', display: { kind: 'error', text: 'stream failed' } },
    ];
    expect(partsOf(buildTimeline(rows))).toEqual([
      { type: 'text', content: '核实完毕 **增补**' },
      { type: 'error', content: 'stream failed', guardrail: undefined, stage: undefined },
    ]);
  });

  it('documented difference: handoff parts are live-only', () => {
    let live = ensureLiveTurn([], RUN)!;
    live = appendHandoffPart(live, 'triage → coder')!;
    const streamParts = (live[live.length - 1] as TurnEntry).parts;
    expect(streamParts).toEqual([{ type: 'handoff', content: 'triage → coder' }]);

    // The backend persists no handoff row — the transfer_to_* tool call is the
    // durable record. A replay therefore has no handoff part, by design.
    const rows: EntryView[] = [
      { id: 1, run_id: RUN, kind: 'item', role: 'assistant', content: 'transfer_to_coder({})', display: { kind: 'tool_call', call_id: 'h1', tool_name: 'transfer_to_coder', arguments: '{}' } },
      { id: 2, run_id: RUN, kind: 'item', role: 'tool', content: 'ok', display: { kind: 'tool_output', call_id: 'h1', output: 'ok' } },
    ];
    const replayParts = partsOf(buildTimeline(rows));
    expect(replayParts.some(p => p.type === 'handoff')).toBe(false);
    expect(replayParts).toEqual([
      { type: 'tools', toolCalls: [{ tool_call_id: 'h1', tool_name: 'transfer_to_coder', arguments: '{}', status: 'completed', output: 'ok' }] },
    ]);
  });

  it('documented difference: rejected status is live-only, replay shows completed', () => {
    let live = ensureLiveTurn([], RUN)!;
    live = appendToolCall(live, {
      tool_call_id: 'c1', tool_name: 'rm', arguments: '{}',
      needs_approval: true, status: null, output: null,
    }, '')!;
    // User rejects (optimistic patch in app.tsx sets the status)…
    live = applyToolResult(live, 'c1', 'rejected by user')!;
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
      { id: 1, run_id: RUN, kind: 'item', role: 'assistant', content: 'rm({})', display: { kind: 'tool_call', call_id: 'c1', tool_name: 'rm', arguments: '{}' } },
      { id: 2, run_id: RUN, kind: 'item', role: 'tool', content: 'rejected by user', display: { kind: 'tool_output', call_id: 'c1', output: 'rejected by user' } },
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
      { id: 1, run_id: 'old', kind: 'item', role: 'user', content: 'earlier q' },
      { id: 2, run_id: 'old', kind: 'item', role: 'assistant', content: 'earlier a', display: { kind: 'message', text: 'earlier a' } },
    ]);
    let live = ensureLiveTurn([], RUN, 'new q')!;
    live = appendMessageItem(live, 'streaming…', false)!;
    const merged = mergeLiveTail(persisted, live, RUN);
    expect(merged.map(m => m.role)).toEqual(['user', 'turn', 'user', 'turn']);
    expect((merged[2] as { content?: string }).content).toBe('new q');

    // Entries the store already covers are deduped: the run's user prompt
    // persisted while the fetch was in flight must not double up.
    const persistedWithPrompt = buildTimeline([
      { id: 1, run_id: RUN, kind: 'item', role: 'user', content: 'new q' },
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
      { id: 1, run_id: 'run-old', kind: 'item', role: 'user', content: 'hello', entry_id: 'u1' },
    ]);
    const stale = [
      { role: 'user', content: 'hello', clientMsgId: 'c1' },
      { role: 'turn', parts: [{ type: 'text', content: 'OLD ANSWER' }], runId: 'run-old' },
    ] as unknown as ReturnType<typeof buildTimeline>;
    // No live run: the stale turn is dropped, the bubble dedups onto its row.
    expect(mergeLiveTail(persisted, stale, null)).toEqual(persisted);
    // A different run is live: the stale turn still does not come back.
    const merged = mergeLiveTail(persisted, [...stale, { role: 'turn', parts: [], runId: RUN }] as unknown as ReturnType<typeof buildTimeline>, RUN);
    expect(merged.filter(m => m.role === 'turn').map(m => (m as TurnEntry).runId)).toEqual([RUN]);
  });

  it('mergeLiveTail: two identical optimistic sends both survive one persisted copy', () => {
    // First "x" is already persisted; two optimistic "x" bubbles (distinct
    // clientMsgIds, no messageId) sit in the live tail. Content-only dedup is
    // one-to-one: one bubble consumes the persisted copy, the second must NOT
    // also collapse onto it — that used to drop the genuine second send.
    const persisted = buildTimeline([
      { id: 1, run_id: 'r0', kind: 'item', role: 'user', content: 'x' },
    ]);
    const live = [
      { role: 'user', content: 'x', clientMsgId: 'c1' },
      { role: 'user', content: 'x', clientMsgId: 'c2' },
    ] as unknown as ReturnType<typeof buildTimeline>;
    const merged = mergeLiveTail(persisted, live);
    expect(merged.filter(m => m.role === 'user')).toHaveLength(2);
  });

  it('replay dedup: re-delivered items and repeated run.started do not duplicate', () => {
    // Hub replays after a reconnect re-run the same events; the reducers must
    // be idempotent the same way the timeline rebuild inherently is.
    let live = ensureLiveTurn([], RUN)!;
    expect(ensureLiveTurn(live, RUN)).toBeNull(); // second run.started: no second turn
    live = appendMessageItem(live, 'hello', false)!;
    expect(appendMessageItem(live, 'hello', true)).toBeNull(); // no-item-id text replay
    live = appendHandoffPart(live, 'a → b')!;
    expect(appendHandoffPart(live, 'a → b')).toBeNull(); // handoff replay
    live = appendCancelledPart(live, '', '')!;
    const again = appendCancelledPart(live, '', '')!;
    const parts = (again[again.length - 1] as TurnEntry).parts;
    expect(parts.filter(p => p.type === 'cancelled')).toHaveLength(1); // marker idempotent
  });

  it('compaction: the checkpoint swallows what it folded, and keeps it expandable', () => {
    const timeline = buildTimeline([
      { id: 1, entry_id: 'e1', kind: 'item', role: 'user', content: 'old question' },
      { id: 2, entry_id: 'e2', kind: 'item', role: 'assistant', content: 'old answer', display: { kind: 'message', text: 'old answer' } },
      { id: 3, entry_id: 'e3', kind: 'compaction', role: 'compaction', content: 'summary of the above', compaction: { excluded_ids: ['e1', 'e2'], tokens_before: 12400, tokens_after: 3100 } },
      { id: 4, entry_id: 'e4', kind: 'item', role: 'user', content: 'new question' },
    ]);
    // The folded pair is NOT loose in the history — the checkpoint stands for it.
    expect(timeline.map(m => m.role)).toEqual(['compaction', 'user']);
    const cp = timeline[0] as { folded?: Array<{ role: string }>; tokensBefore?: number; tokensAfter?: number };
    expect(cp.tokensBefore).toBe(12400);
    expect(cp.tokensAfter).toBe(3100);
    // …but it is still there, in full, one expand away.
    expect(cp.folded?.map(m => m.role)).toEqual(['user', 'turn']);
  });

  it('compaction: a second pass folds the first checkpoint under it', () => {
    const timeline = buildTimeline([
      { id: 1, entry_id: 'e1', kind: 'item', role: 'user', content: 'oldest' },
      { id: 2, entry_id: 'e2', kind: 'compaction', role: 'compaction', content: 'first summary', compaction: { excluded_ids: ['e1'] } },
      { id: 3, entry_id: 'e3', kind: 'item', role: 'user', content: 'middle' },
      { id: 4, entry_id: 'e4', kind: 'compaction', role: 'compaction', content: 'second summary', compaction: { excluded_ids: ['e1', 'e2', 'e3'] } },
    ]);
    expect(timeline.map(m => m.role)).toEqual(['compaction']);
    const outer = timeline[0] as { content: string; folded?: Array<{ role: string; folded?: Array<{ role: string }> }> };
    expect(outer.content).toBe('second summary');
    // e1 is named by BOTH checkpoints; the later one wins, so the inner
    // checkpoint keeps its marker but no longer owns the entry.
    expect(outer.folded?.map(m => m.role)).toEqual(['user', 'compaction', 'user']);
    expect(outer.folded?.[1].folded).toBeUndefined();
  });

  it('compaction: an entry marked compacted but named by no checkpoint stays in place', () => {
    // Soft-deleted by an older compaction whose checkpoint predates ExcludedIDs.
    // Hiding it would make a whole run vanish from the history.
    const timeline = buildTimeline([
      { id: 1, entry_id: 'e1', kind: 'item', role: 'user', content: 'orphaned', compacted: true },
      { id: 2, entry_id: 'e2', kind: 'item', role: 'user', content: 'current' },
    ]);
    expect(timeline.map(m => (m as { content?: string }).content)).toEqual(['orphaned', 'current']);
  });

  it('branching: the abandoned attempt leaves the timeline but stays offerable', () => {
    // One question, answered twice. e2 was abandoned; e4 is current.
    const timeline = buildTimeline([
      { id: 1, entry_id: 'e1', kind: 'item', role: 'user', content: 'question', on_path: true },
      { id: 2, entry_id: 'e2', parent_id: 'e1', kind: 'item', role: 'assistant', content: 'first', display: { kind: 'message', text: 'first' }, on_path: false },
      { id: 3, entry_id: 'e3', parent_id: 'e2', kind: 'leaf', role: 'assistant', on_path: false },
      { id: 4, entry_id: 'e4', parent_id: 'e1', kind: 'item', role: 'assistant', content: 'second', display: { kind: 'message', text: 'second' }, on_path: true },
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
      { id: 1, entry_id: 'e1', kind: 'item', role: 'user', content: 'question', on_path: true },
      { id: 2, entry_id: 'e2', parent_id: 'e1', kind: 'item', role: 'assistant', content: 'old answer', display: { kind: 'message', text: 'old answer' }, on_path: false },
      { id: 3, entry_id: 'e3', parent_id: 'e1', kind: 'leaf', role: 'user', on_path: true },
    ]);
    expect(timeline.map(m => m.role)).toEqual(['user']);
  });

  it('branching: a leaf entry is not an attempt', () => {
    // Every branch switch appends a leaf entry at whatever the tip was.
    // Counting those as children invents a fork at each switch.
    const timeline = buildTimeline([
      { id: 1, entry_id: 'e1', kind: 'item', role: 'user', content: 'q', on_path: true },
      { id: 2, entry_id: 'e2', parent_id: 'e1', kind: 'item', role: 'assistant', content: 'a', display: { kind: 'message', text: 'a' }, on_path: true },
      { id: 3, entry_id: 'e3', parent_id: 'e2', kind: 'leaf', role: 'assistant', on_path: true },
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
      { id: 1, run_id: RUN, kind: 'item', role: 'user', content: 'spawn something' },
      { id: 2, run_id: RUN, kind: 'item', role: 'assistant', display: { kind: 'tool_call', call_id: 'c1', tool_name: 'spawn_task', arguments: '{}', title: 'audit', summary: 'all green', extra: { task_id: 't1', task_status: 'completed' } } },
      { id: 3, run_id: RUN, kind: 'item', role: 'tool', display: { kind: 'tool_output', call_id: 'c1', output: '{"task_id":"t1"}' } },
    ]);
    const turn = timeline[1] as TurnEntry;
    const tools = turn.parts.find(p => p.type === 'tools') as { toolCalls: Array<{ task?: { id?: string; status?: string; summary?: string } }> };
    expect(tools.toolCalls[0].task).toEqual({ id: 't1', label: 'audit', status: 'completed', summary: 'all green' });
  });
});
