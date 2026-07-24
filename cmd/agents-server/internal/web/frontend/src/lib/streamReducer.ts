// Pure transforms that build a live turn's parts from streamed run events.
// useAgentSocket owns the wiring (buffers, dedup sets, raf batching) and calls
// these for every messages mutation, so the streaming assembly is testable —
// timeline.test.ts locks the isomorphism contract: a turn assembled here from
// events must equal the same turn rebuilt by buildTimeline from its persisted
// rows (documented intentional differences aside). Change a shape on one side
// and that test is what fails, instead of a reload silently rendering
// differently than the stream did.
//
// Convention: each transform returns the new messages array, or null when it
// deliberately changed nothing (no live turn, replay dedup hit) — callers keep
// their existing state object in that case.

import { patchToolCall, findToolCall } from '@/lib/timeline';
import type { TimelineEntry, TurnEntry, TurnPart, ToolCall, ErrorPart, UserEntry } from '@/lib/timeline';

// Loose message shape: live state mixes TimelineEntry rows with optimistic
// entries, so transforms only assume `role` and (for turns) `parts`.
type Msgs = TimelineEntry[];

// lastTurn returns the trailing message when it is a turn, else null. Every
// stream transform anchors on the LAST message being the live turn — a message
// appended after it would freeze live rendering (see the run.handoff note in
// useAgentSocket).
function lastTurn(msgs: Msgs): TurnEntry | null {
  const last = msgs[msgs.length - 1];
  return last && last.role === 'turn' ? (last as TurnEntry) : null;
}

// withParts replaces the trailing turn's parts immutably.
function withParts(msgs: Msgs, turn: TurnEntry, parts: TurnPart[]): Msgs {
  const out = [...msgs];
  out[out.length - 1] = { ...turn, parts };
  return out;
}

// ensureLiveTurn appends the empty live turn for a starting run — preceded by
// the user bubble built from run.started's input when this browser doesn't
// already show it: run events broadcast to every connection, and an in-flight
// prompt is not persisted yet, so a watching browser has no other source for
// it. The sender's own optimistic bubble (same trailing content) wins the
// dedup. Returns null when a turn for this run already exists (hub replays
// re-deliver run.started).
export function ensureLiveTurn(msgs: Msgs, runId: string, input?: string): Msgs | null {
  const hasTurn = msgs.some(m => m.role === 'turn' && (m as TurnEntry).runId === runId);
  if (hasTurn) return null;
  const out = [...msgs];
  if (input) {
    const last = out[out.length - 1];
    const dup = last?.role === 'user' && (last as UserEntry).content === input;
    if (!dup) out.push({ role: 'user', content: input, runId } as UserEntry);
  }
  out.push({ role: 'turn', parts: [], runId } as TurnEntry);
  return out;
}

// mergeLiveTail reconciles a fetched persisted timeline with live entries that
// streamed in while the fetch was in flight: everything without a messageId at
// the tail of `current` (optimistic/broadcast user bubbles, the in-flight
// turn) is re-appended after the persisted rows, deduping entries the store
// already covers. Without this, a broadcast replay arriving before the fetch
// marked the session loaded and the fetch result was dropped — a second
// browser saw the live turn but no history.
export function mergeLiveTail(persisted: Msgs, current: Msgs): Msgs {
  let i = current.length;
  while (i > 0 && current[i - 1].messageId === undefined) i--;
  const tail = current.slice(i);
  if (tail.length === 0) return persisted;
  const out = [...persisted];
  // Content-only fallback matches must be one-to-one: a persisted bubble already
  // claimed by an earlier tail entry can't absorb a second identical one, so two
  // successive identical sends don't collapse into a single message.
  const contentConsumed = new Set<number>();
  for (const m of tail) {
    if (m.role === 'user') {
      const u = m as UserEntry & { clientMsgId?: string };
      // Identity keys win: a shared runId or clientMsgId means the same message.
      // Two DISTINCT optimistic sends of the same text carry different
      // clientMsgIds and must both survive.
      let dup = out.some(p => {
        if (p.role !== 'user') return false;
        const pu = p as UserEntry & { clientMsgId?: string };
        if (pu.runId && u.runId) return pu.runId === u.runId;
        if (pu.clientMsgId && u.clientMsgId) return pu.clientMsgId === u.clientMsgId;
        return false;
      });
      // Content equality is the last resort, only when neither side shares an id
      // kind (e.g. a persisted row vs. a broadcast bubble for the same prompt),
      // and each persisted row is consumed at most once.
      if (!dup) {
        for (let idx = 0; idx < out.length; idx++) {
          if (contentConsumed.has(idx)) continue;
          const p = out[idx];
          if (p.role !== 'user') continue;
          const pu = p as UserEntry & { clientMsgId?: string };
          if (pu.runId && u.runId) continue;
          if (pu.clientMsgId && u.clientMsgId) continue;
          if (pu.content === u.content) { contentConsumed.add(idx); dup = true; break; }
        }
      }
      if (!dup) out.push(m);
    } else if (m.role === 'turn') {
      const rid = (m as TurnEntry).runId;
      const dup = !!rid && out.some(p => p.role === 'turn' && (p as TurnEntry).runId === rid);
      if (!dup) out.push(m);
    } else {
      out.push(m);
    }
  }
  return out;
}

// appendMessageItem folds one completed assistant message into the live turn.
// dedupByText guards replays on backends that send no item id (an id-based
// dedup hit is decided by the caller, which owns the seen-ids set).
export function appendMessageItem(msgs: Msgs, text: string, dedupByText: boolean): Msgs | null {
  const turn = lastTurn(msgs);
  if (!turn) return null;
  const parts = [...(turn.parts || [])];
  if (dedupByText && parts.some(pt => pt.type === 'text' && pt.content === text)) return null;
  parts.push({ type: 'text', content: text });
  return withParts(msgs, turn, parts);
}

// appendReasoningItem folds one completed thinking block into the live turn,
// with the same dedup contract as appendMessageItem.
export function appendReasoningItem(msgs: Msgs, text: string, dedupByText: boolean): Msgs | null {
  const turn = lastTurn(msgs);
  if (!turn) return null;
  const parts = [...(turn.parts || [])];
  if (dedupByText && parts.some(pt => pt.type === 'thinking' && pt.content === text)) return null;
  parts.push({ type: 'thinking', content: text });
  return withParts(msgs, turn, parts);
}

// finalizeTurn flushes what run.output carries: leftover thinking, and the
// final text unless run.message already appended it (older server / no-item
// edge cases append it here).
export function finalizeTurn(msgs: Msgs, text: string, thinking: string): Msgs | null {
  const turn = lastTurn(msgs);
  if (!turn || (!text && !thinking)) return null;
  const parts = [...(turn.parts || [])];
  if (thinking) parts.push({ type: 'thinking', content: thinking });
  if (text && !parts.some(pt => pt.type === 'text' && pt.content === text)) {
    parts.push({ type: 'text', content: text });
  }
  return withParts(msgs, turn, parts);
}

// appendErrorPart attaches the error (plus any un-flushed thinking/text the
// buffers still held) to the live turn — or, when the failure arrived before
// any turn existed, as a turn of its own so the error is visible at all.
export function appendErrorPart(msgs: Msgs, err: ErrorPart, thinking: string, remaining: string): Msgs {
  const turn = lastTurn(msgs);
  if (!turn) return [...msgs, { role: 'turn', parts: [err] } as TurnEntry];
  const parts = [...(turn.parts || [])];
  if (thinking) parts.push({ type: 'thinking', content: thinking });
  if (remaining) parts.push({ type: 'text', content: remaining });
  parts.push(err);
  return withParts(msgs, turn, parts);
}

// appendCancelledPart marks the live turn cancelled, flushing leftover
// buffers first. No turn -> nothing to mark (null).
export function appendCancelledPart(msgs: Msgs, thinking: string, remaining: string): Msgs | null {
  const turn = lastTurn(msgs);
  if (!turn) return null;
  const parts = [...(turn.parts || [])];
  if (thinking) parts.push({ type: 'thinking', content: thinking });
  if (remaining) parts.push({ type: 'text', content: remaining });
  // The marker is idempotent: hub replays must not stack duplicates.
  if (parts[parts.length - 1]?.type !== 'cancelled') parts.push({ type: 'cancelled', content: '' });
  return withParts(msgs, turn, parts);
}

// appendToolCall adds a tool call to the live turn, flushing interim narration
// text first so prose and calls interleave chronologically. A call already
// present (replay, approval-rebuilt turn) is patched in place instead — a
// duplicate needs_approval card would pin the session red forever.
export function appendToolCall(msgs: Msgs, tc: ToolCall, flushed: string): Msgs | null {
  const patched = patchToolCall(msgs, tc.tool_call_id, {
    tool_name: tc.tool_name, arguments: tc.arguments, needs_approval: tc.needs_approval,
  });
  if (patched) return patched;
  const turn = lastTurn(msgs);
  if (!turn) return null;
  const parts = [...(turn.parts || [])];
  if (flushed) parts.push({ type: 'text', content: flushed });
  const lastPart = parts[parts.length - 1];
  if (lastPart?.type === 'tools') {
    parts[parts.length - 1] = { ...lastPart, toolCalls: [...lastPart.toolCalls, tc] };
  } else {
    parts.push({ type: 'tools', toolCalls: [tc] });
  }
  return withParts(msgs, turn, parts);
}

// applyToolResult records a call's output. A user-rejected call keeps its
// terminal 'rejected' status: the resumed run still emits a tool_output (the
// rejection notice) that would otherwise clobber the red badge to 'completed'.
export function applyToolResult(msgs: Msgs, toolCallId: string, output: string): Msgs | null {
  const cur = findToolCall(msgs, toolCallId);
  const status = cur?.status === 'rejected' ? 'rejected' : 'completed';
  return patchToolCall(msgs, toolCallId, { output, status });
}

// appendHandoffPart records a completed agent switch inside the live turn.
// Live-only by design: reloads convey the same transfer via the transfer_to_*
// tool-call card (see the isomorphism test's documented differences).
export function appendHandoffPart(msgs: Msgs, content: string): Msgs | null {
  const turn = lastTurn(msgs);
  if (!turn) return null;
  if ((turn.parts || []).some(pt => pt.type === 'handoff' && pt.content === content)) return null;
  return withParts(msgs, turn, [...(turn.parts || []), { type: 'handoff', content }]);
}
