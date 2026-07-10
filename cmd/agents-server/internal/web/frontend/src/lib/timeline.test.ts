// The isomorphism contract between the two ways a turn is built:
//
//   streaming — useAgentSocket applies run events via the streamReducer
//               transforms as they arrive;
//   replay    — buildTimeline rebuilds the same turn from the rows the
//               backend persisted (message_store.deriveDisplay projections
//               and runner.savePartialTurn annotations).
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
import { buildTimeline, type Message, type TurnEntry } from '@/lib/timeline';
import {
  ensureLiveTurn, mergeLiveTail, appendMessageItem, appendReasoningItem, finalizeTurn,
  appendErrorPart, appendCancelledPart, appendToolCall, applyToolResult, appendHandoffPart,
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
    const rows: Message[] = [
      { id: 1, run_id: RUN, role: 'user', content: 'q' },
      { id: 2, run_id: RUN, role: 'reasoning', content: 'pondering' },
      { id: 3, run_id: RUN, role: 'tool_call', content: 'search({"q":"x"})', display: { call_id: 'c1', name: 'search', arguments: '{"q":"x"}' } },
      { id: 4, run_id: RUN, role: 'tool_output', content: 'found it', display: { call_id: 'c1', output: 'found it' } },
      { id: 5, run_id: RUN, role: 'assistant', content: 'let me check' },
      { id: 6, run_id: RUN, role: 'assistant', content: 'the answer' },
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

  it('guardrail-blocked turn: thinking → typed error card', () => {
    let live = ensureLiveTurn([], RUN)!;
    // run.error flushes the reasoning buffer into a part, then the typed card.
    live = appendErrorPart(live, { type: 'error', content: 'blocked', guardrail: 'no_secrets', stage: 'input' }, 'was thinking', '');

    const rows: Message[] = [
      { id: 1, run_id: RUN, role: 'reasoning', content: 'was thinking' },
      { id: 2, run_id: RUN, role: 'error', content: 'blocked', display: { guardrail: 'no_secrets', stage: 'input' } },
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

    const rows: Message[] = [
      { id: 1, run_id: RUN, role: 'assistant', content: 'partial answer' },
      { id: 2, run_id: RUN, role: 'cancelled', content: '' },
    ];

    const streamParts = (live[live.length - 1] as TurnEntry).parts;
    expect(streamParts).toEqual([
      { type: 'text', content: 'partial answer' },
      { type: 'cancelled', content: '' },
    ]);
    expect(partsOf(buildTimeline(rows))).toEqual(streamParts);
  });

  it('documented difference: handoff parts are live-only', () => {
    let live = ensureLiveTurn([], RUN)!;
    live = appendHandoffPart(live, 'triage → coder')!;
    const streamParts = (live[live.length - 1] as TurnEntry).parts;
    expect(streamParts).toEqual([{ type: 'handoff', content: 'triage → coder' }]);

    // The backend persists no handoff row — the transfer_to_* tool call is the
    // durable record. A replay therefore has no handoff part, by design.
    const rows: Message[] = [
      { id: 1, run_id: RUN, role: 'tool_call', content: 'transfer_to_coder({})', display: { call_id: 'h1', name: 'transfer_to_coder', arguments: '{}' } },
      { id: 2, run_id: RUN, role: 'tool_output', content: 'ok', display: { call_id: 'h1', output: 'ok' } },
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
    const rows: Message[] = [
      { id: 1, run_id: RUN, role: 'tool_call', content: 'rm({})', display: { call_id: 'c1', name: 'rm', arguments: '{}' } },
      { id: 2, run_id: RUN, role: 'tool_output', content: 'rejected by user', display: { call_id: 'c1', output: 'rejected by user' } },
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
      { id: 1, run_id: 'old', role: 'user', content: 'earlier q' },
      { id: 2, run_id: 'old', role: 'assistant', content: 'earlier a' },
    ]);
    let live = ensureLiveTurn([], RUN, 'new q')!;
    live = appendMessageItem(live, 'streaming…', false)!;
    const merged = mergeLiveTail(persisted, live);
    expect(merged.map(m => m.role)).toEqual(['user', 'turn', 'user', 'turn']);
    expect((merged[2] as { content?: string }).content).toBe('new q');

    // Entries the store already covers are deduped: the run's user prompt
    // persisted while the fetch was in flight must not double up.
    const persistedWithPrompt = buildTimeline([
      { id: 1, run_id: RUN, role: 'user', content: 'new q' },
    ]);
    const merged2 = mergeLiveTail(persistedWithPrompt, live);
    expect(merged2.filter(m => m.role === 'user')).toHaveLength(1);
    expect(merged2.filter(m => m.role === 'turn')).toHaveLength(1);
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
});
