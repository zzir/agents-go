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
import type { TimelineEntry, TurnEntry, TurnPart, ToolCall, ToolCallPatch, DisplayExtra, ErrorPart, UserEntry } from '@/lib/timeline';

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
//
// liveRunId scopes which TURN is genuinely in flight: only the current live
// run's turn survives the merge. A finished turn whose terminal reload hasn't
// landed yet, or one paused on an approval, also sits unstamped in the tail —
// re-appending those after a regenerate's branch switch is how the replaced
// answer used to linger on screen.
export function mergeLiveTail(persisted: Msgs, current: Msgs, liveRunId?: string | null): Msgs {
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
      if (!rid || rid !== liveRunId) continue;
      const dup = out.some(p => p.role === 'turn' && (p as TurnEntry).runId === rid);
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

// ToolResultDisplay is the display portion of a run.tool_result event: the
// tool's own word on how to present the result, mirroring the stored output
// entry's display fields.
export interface ToolResultDisplay {
  title?: string;
  summary?: string;
  renderer?: string;
  is_error?: boolean;
  extra?: DisplayExtra;
}

// applyToolResult records a call's output. A user-rejected call keeps its
// terminal 'rejected' status: the resumed run still emits a tool_output (the
// rejection notice) that would otherwise clobber the red badge to 'completed'.
export function applyToolResult(msgs: Msgs, toolCallId: string, output: string, display?: ToolResultDisplay): Msgs | null {
  const cur = findToolCall(msgs, toolCallId);
  const status = cur?.status === 'rejected' ? 'rejected' : 'completed';
  // The result REPLACES any live progress: progress was how the tool got here,
  // and leaving both would show the same work twice.
  //
  // Only when there IS progress, though. Patching the key unconditionally would
  // add it to every streamed call and none of the replayed ones, and the two
  // paths must produce identical timelines — the isomorphism test is what says
  // so, and it caught exactly this.
  const patch: ToolCallPatch = { output, status };
  if (cur?.progress) patch.progress = '';
  // Display fields patch conditionally for the same reason: buildTimeline sets
  // them only when the stored display carries them, and the two paths must
  // produce identical timelines.
  if (display?.title) patch.title = display.title;
  if (display?.summary) patch.summary = display.summary;
  if (display?.renderer) patch.renderer = display.renderer;
  if (display?.is_error) patch.is_error = true;
  if (display?.extra && Object.keys(display.extra).length) patch.extra = display.extra;
  return patchToolCall(msgs, toolCallId, patch);
}

// TERMINAL_TASK_STATUSES mirrors the server's isTerminalTaskStatus — the three
// states a task cannot leave.
export const TERMINAL_TASK_STATUSES = new Set(['completed', 'failed', 'cancelled']);

// applyTaskTerminal folds a task's terminal outcome into its spawn card — the
// live counterpart of the call-display UPDATE the server appends for reload
// (bridge onTaskUpdate), same shape as buildTimeline's fold, so the card
// renders identically before and after a refresh.
//
// Terminal only: pre-terminal states stay on the card's liveTaskStatus props —
// an early `task` object would take over the badge path and bypass ChatView's
// identity-stable memo. And a card already terminal keeps its outcome: a late
// or replayed event must not move it, mirroring the server's own
// no-move-backwards guard.
export function applyTaskTerminal(msgs: Msgs, toolCallId: string, task: { id: string; label?: string; status: string; summary?: string; attempt?: number }): Msgs | null {
  if (!TERMINAL_TASK_STATUSES.has(task.status)) return null;
  const cur = findToolCall(msgs, toolCallId);
  if (!cur || (cur.task?.status && TERMINAL_TASK_STATUSES.has(cur.task.status))) return null;
  // An outcome from an attempt the card has already moved past: a stale
  // snapshot, resolving after the events that overtook it.
  if (task.attempt && cur.task?.attempt && task.attempt < cur.task.attempt) return null;
  const t: NonNullable<ToolCall['task']> = { id: task.id, status: task.status };
  if (task.label) t.label = task.label;
  if (task.summary) t.summary = task.summary;
  if (task.attempt) t.attempt = task.attempt;
  return patchToolCall(msgs, toolCallId, { task: t });
}

// startTaskAttempt re-arms a spawn card for a retry: the outcome it is showing
// belongs to an attempt that is over, and the task is running again.
//
// Clearing `task` rather than overwriting it with a working state is what makes
// applyTaskTerminal accept the NEW outcome — its no-move-backwards guard reads
// the card's own terminal status — and it puts the badge back on the live
// status path, which is where a running task belongs. The label survives,
// because the card is still that task's.
//
// Guarded by attempt so a replayed run.started cannot wipe a real outcome: only
// a run BEYOND the one the card describes is a new attempt.
export function startTaskAttempt(msgs: Msgs, toolCallId: string, attempt: number): Msgs | null {
  const cur = findToolCall(msgs, toolCallId);
  if (!cur?.task?.status || !TERMINAL_TASK_STATUSES.has(cur.task.status)) return null;
  if (!attempt || attempt <= (cur.task.attempt ?? 1)) return null;
  // The attempt stays on the card. Dropping it left the card unable to say
  // which attempt it was on, so a stale snapshot resolving later — a reconnect
  // fetch issued before the retry — could fold the PREVIOUS attempt's outcome
  // onto a task that is running again. The status is what the badge reads, and
  // that is what re-arming clears.
  const task: NonNullable<ToolCall['task']> = { attempt };
  if (cur.task.label) task.label = cur.task.label;
  return patchToolCall(msgs, toolCallId, { task });
}

// syncTaskCard brings a spawn card in line with a task's current state: a newer
// attempt re-arms it, a terminal outcome folds in, and both together are a
// retry that has already finished.
//
// It exists because three callers need it — the live task events, a REST
// response the caller already has in hand, and the reconnect sweep — and each
// remembering the parts separately is how the attempt stopped being recorded
// on the live path, which quietly disarmed the guard that keeps a replayed
// run.started from wiping a real outcome.
export function syncTaskCard(msgs: Msgs, toolCallId: string, task: { id: string; label?: string; status?: string; summary?: string; attempt?: number }): Msgs | null {
  let out: Msgs | null = null;
  if (task.attempt) {
    const rearmed = startTaskAttempt(msgs, toolCallId, task.attempt);
    if (rearmed) { out = rearmed; msgs = rearmed; }
  }
  if (task.status) {
    const folded = applyTaskTerminal(msgs, toolCallId, {
      id: task.id, label: task.label, status: task.status, summary: task.summary, attempt: task.attempt,
    });
    if (folded) out = folded;
  }
  return out;
}

// appendToolProgress accumulates the live output a running tool pushed.
//
// It appends rather than replaces because the wire carries deltas: a command
// producing output over two minutes sends many, and each is the next piece, not
// the whole picture.
export function appendToolProgress(msgs: Msgs, toolCallId: string, delta: string, renderer?: string): Msgs | null {
  if (!delta) return null;
  const cur = findToolCall(msgs, toolCallId);
  // A call that already has its result is finished; a late delta from a
  // goroutine the tool left behind must not reopen it.
  if (!cur || cur.output !== null) return null;
  return patchToolCall(msgs, toolCallId, {
    progress: (cur.progress || '') + delta,
    renderer: renderer || cur.renderer,
  });
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
