// Pure transforms that build a live turn's parts from streamed run events;
// useAgentSocket owns the buffers, dedup sets and frame batching — invariant 16.
// Each transform returns the new messages array, or null when nothing changed.

import { attachmentIdsEqual, type AttachmentMeta } from '@/lib/attachments';
import { patchToolCall, findToolCall } from '@/lib/timeline';
import type { TimelineEntry, TurnEntry, TurnPart, ToolCall, ToolCallPatch, DisplayExtra, ErrorPart, UserEntry } from '@/lib/timeline';

type Msgs = TimelineEntry[];

// lastTurn returns the trailing message when it is a turn, else null. Every transform
// anchors on the LAST message being the live turn (see run.handoff in useAgentSocket).
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

// runShows reports whether a turn of the live turn's run already holds a part pick
// admits (an injection splits one run into several turns; a replay re-delivers them).
function runShows(msgs: Msgs, turn: TurnEntry, pick: (p: TurnPart) => boolean): boolean {
  return msgs.some(m => m.role === 'turn'
    && (m === turn || (turn.runId !== undefined && m.runId === turn.runId))
    && (m.parts || []).some(pick));
}

// ensureLiveTurn appends the live turn for a starting run, with the user bubble
// from run.started's input unless already shown (invariant 14); null when the
// run's turn exists (a replay).
export function ensureLiveTurn(msgs: Msgs, runId: string, input?: string, attachments?: AttachmentMeta[]): Msgs | null {
  const hasTurn = msgs.some(m => m.role === 'turn' && (m as TurnEntry).runId === runId);
  if (hasTurn) return null;
  const out = [...msgs];
  if (input || attachments?.length) {
    const last = out[out.length - 1];
    // Text AND attachment set together are the message's identity.
    const dup = last?.role === 'user' && (last as UserEntry).content === (input || '')
      && attachmentIdsEqual((last as UserEntry).attachments, attachments);
    if (!dup) out.push({ role: 'user', content: input || '', runId, attachments } as UserEntry);
  }
  out.push({ role: 'turn', parts: [], runId } as TurnEntry);
  return out;
}

// appendInjected splits the live turn where the run read queued input `index`
// (from 1): a user bubble, then an empty turn of the same run; a bubble already
// shown adds only the turn.
export function appendInjected(msgs: Msgs, runId: string, input: string, index: number): Msgs | null {
  // The run's user entries after its first turn are its injections, in order.
  let shown = 0;
  let turnSeen = false;
  for (const m of msgs) {
    if (m.role === 'turn' && m.runId === runId) turnSeen = true;
    else if (m.role === 'user' && m.runId === runId && turnSeen) shown++;
  }
  const next = { role: 'turn', parts: [], runId } as TurnEntry;
  const last = msgs[msgs.length - 1];
  if (shown >= index) {
    return last?.role === 'user' && last.runId === runId ? [...msgs, next] : null;
  }
  const bubble = { role: 'user', content: input, runId, injected: index, createdAt: Date.now() } as UserEntry;
  // A second input read at the same point follows the first directly (no empty
  // turn between).
  const prev = msgs[msgs.length - 2];
  if (last?.role === 'turn' && last.runId === runId && last.messageId === undefined && (last.parts || []).length === 0
    && prev?.role === 'user' && prev.runId === runId) {
    return [...msgs.slice(0, -1), bubble, last];
  }
  return [...msgs, bubble, next];
}

// mergeLiveTail re-appends the unstamped tail of `current` (optimistic bubbles,
// the live turn) after a fetched timeline, deduping what the store covers; only
// liveRunId's turns survive — invariant 19.
export function mergeLiveTail(persisted: Msgs, current: Msgs, liveRunId?: string | null): Msgs {
  let i = current.length;
  while (i > 0 && current[i - 1].messageId === undefined) i--;
  const tail = current.slice(i);
  if (tail.length === 0) return persisted;
  const out = [...persisted];
  // A persisted bubble absorbs at most one content-only match, so two identical
  // sends do not collapse.
  const contentConsumed = new Set<number>();
  // The store's turns cover the live run's first turns in order, less those
  // `current` already shows stamped (not in the tail).
  const liveTurns = (msgs: Msgs) => msgs.filter(p => p.role === 'turn' && p.runId === liveRunId).length;
  let storedLiveTurns = Math.max(0, liveTurns(persisted) - liveTurns(current.slice(0, i)));
  for (const u of tail) {
    if (u.role === 'user' && u.injected !== undefined) {
      // An injected bubble shares the prompt's run id: covered only by the same
      // text under that run, past its first turn.
      const firstTurn = persisted.findIndex(p => p.role === 'turn' && p.runId === u.runId);
      const idx = firstTurn < 0 ? -1 : out.findIndex((p, k) => k > firstTurn && k < persisted.length && !contentConsumed.has(k)
        && p.role === 'user' && p.runId === u.runId && p.content === u.content);
      if (idx >= 0) contentConsumed.add(idx); else out.push(u);
    } else if (u.role === 'user') {
      // Identity keys win: a shared runId or clientMsgId is the same message;
      // distinct sends differ by clientMsgId.
      let dup = out.some(p => {
        if (p.role !== 'user') return false;
        if (p.runId && u.runId) return p.runId === u.runId;
        if (p.clientMsgId && u.clientMsgId) return p.clientMsgId === u.clientMsgId;
        return false;
      });
      // Content equality is the last resort, when neither side shares an id
      // kind; each persisted row is consumed once.
      if (!dup) {
        for (let idx = 0; idx < out.length; idx++) {
          if (contentConsumed.has(idx)) continue;
          const p = out[idx];
          if (p.role !== 'user') continue;
          if (p.runId && u.runId) continue;
          if (p.clientMsgId && u.clientMsgId) continue;
          if (p.content === u.content && attachmentIdsEqual(p.attachments, u.attachments)) { contentConsumed.add(idx); dup = true; break; }
        }
      }
      if (!dup) out.push(u);
    } else if (u.role === 'turn') {
      const rid = u.runId;
      if (!rid || rid !== liveRunId) continue;
      if (storedLiveTurns > 0) { storedLiveTurns--; continue; }
      out.push(u);
    } else {
      out.push(u);
    }
  }
  return out;
}

// appendMessageItem folds one completed assistant message into the live turn;
// dedupByText guards replays on backends that send no item id (id dedup is the
// caller's).
export function appendMessageItem(msgs: Msgs, text: string, dedupByText: boolean): Msgs | null {
  const turn = lastTurn(msgs);
  if (!turn) return null;
  const parts = [...(turn.parts || [])];
  if (dedupByText && runShows(msgs, turn, pt => pt.type === 'text' && pt.content === text)) return null;
  parts.push({ type: 'text', content: text });
  return withParts(msgs, turn, parts);
}

// appendReasoningItem folds one completed thinking block into the live turn,
// with the same dedup contract as appendMessageItem.
export function appendReasoningItem(msgs: Msgs, text: string, dedupByText: boolean): Msgs | null {
  const turn = lastTurn(msgs);
  if (!turn) return null;
  const parts = [...(turn.parts || [])];
  if (dedupByText && runShows(msgs, turn, pt => pt.type === 'thinking' && pt.content === text)) return null;
  parts.push({ type: 'thinking', content: text });
  return withParts(msgs, turn, parts);
}

// finalizeTurn flushes what run.output carries: leftover thinking, and the final
// text unless run.message already appended it.
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

// appendErrorPart attaches the error (plus un-flushed thinking/text) to the live
// turn, or as a turn of its own when none exists yet.
export function appendErrorPart(msgs: Msgs, err: ErrorPart, thinking: string, remaining: string): Msgs {
  const turn = lastTurn(msgs);
  if (!turn) return [...msgs, { role: 'turn', parts: [err] } as TurnEntry];
  const parts = [...(turn.parts || [])];
  if (thinking) parts.push({ type: 'thinking', content: thinking });
  if (remaining) parts.push({ type: 'text', content: remaining });
  parts.push(err);
  return withParts(msgs, turn, parts);
}

// markNotRun resolves every undecided approval card in the turns pick admits
// as not run for the reason; null when none was pending.
function markNotRun(msgs: Msgs, pick: (turn: TurnEntry) => boolean, reason: string): Msgs | null {
  let changed = false;
  const out = msgs.map(m => {
    if (m.role !== 'turn' || !pick(m as TurnEntry)) return m;
    const parts = (m as TurnEntry).parts.map(p => {
      if (p.type !== 'tools' || !p.toolCalls.some(tc => tc.needs_approval && !tc.status)) return p;
      changed = true;
      return { ...p, toolCalls: p.toolCalls.map(tc => tc.needs_approval && !tc.status ? { ...tc, status: 'not_run', not_run: reason } : tc) };
    });
    return { ...m, parts } as TurnEntry;
  });
  return changed ? out : null;
}

// resolvePendingApprovals marks the run's undecided approval cards not run:
// what run.cancelled with a reason means for a paused run.
export function resolvePendingApprovals(msgs: Msgs, runId: string, reason: string): Msgs | null {
  return markNotRun(msgs, t => t.runId === runId, reason);
}

// supersedePendingApprovals marks every other run's undecided approval cards
// superseded once a newer run starts on the session — invariant 19.
export function supersedePendingApprovals(msgs: Msgs, newRunId: string): Msgs | null {
  return markNotRun(msgs, t => t.runId !== newRunId, 'superseded');
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
// first so prose and calls interleave; a call already present (replay, rebuilt
// turn) is patched in place.
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

// ToolResultDisplay is the display portion of a run.tool_result event, mirroring
// the stored output entry's display fields.
export interface ToolResultDisplay {
  title?: string;
  summary?: string;
  renderer?: string;
  is_error?: boolean;
  extra?: DisplayExtra;
}

// applyToolResult records a call's output. A user-rejected call keeps its 'rejected'
// status: the resumed run's rejection-notice output must not clobber it.
export function applyToolResult(msgs: Msgs, toolCallId: string, output: string, display?: ToolResultDisplay): Msgs | null {
  const cur = findToolCall(msgs, toolCallId);
  const status = cur?.status === 'rejected' ? 'rejected' : 'completed';
  // The result replaces live progress; each key patches only with a value —
  // invariant 16.
  const patch: ToolCallPatch = { output, status };
  if (cur?.progress) patch.progress = '';
  if (display?.title) patch.title = display.title;
  if (display?.summary) patch.summary = display.summary;
  if (display?.renderer) patch.renderer = display.renderer;
  if (display?.is_error) patch.is_error = true;
  if (display?.extra && Object.keys(display.extra).length) patch.extra = display.extra;
  return patchToolCall(msgs, toolCallId, patch);
}

// TERMINAL_TASK_STATUSES mirrors the server's isTerminalTaskStatus.
export const TERMINAL_TASK_STATUSES = new Set(['completed', 'failed', 'cancelled']);

// applyTaskTerminal folds a task's terminal outcome into its spawn card —
// terminal only, never backwards (invariant 21). Pre-terminal states stay on
// the card's live-status props.
export function applyTaskTerminal(msgs: Msgs, toolCallId: string, task: { id: string; label?: string; status: string; summary?: string; attempt?: number }): Msgs | null {
  if (!TERMINAL_TASK_STATUSES.has(task.status)) return null;
  const cur = findToolCall(msgs, toolCallId);
  if (!cur || (cur.task?.status && TERMINAL_TASK_STATUSES.has(cur.task.status))) return null;
  // An outcome from an attempt the card has already moved past is a stale snapshot.
  if (task.attempt && cur.task?.attempt && task.attempt < cur.task.attempt) return null;
  const t: NonNullable<ToolCall['task']> = { id: task.id, status: task.status };
  if (task.label) t.label = task.label;
  if (task.summary) t.summary = task.summary;
  if (task.attempt) t.attempt = task.attempt;
  return patchToolCall(msgs, toolCallId, { task: t });
}

// startTaskAttempt re-arms a spawn card for a retry by clearing its terminal
// status. Only a run beyond the card's attempt counts, so a replayed
// run.started cannot wipe a real outcome.
export function startTaskAttempt(msgs: Msgs, toolCallId: string, attempt: number): Msgs | null {
  const cur = findToolCall(msgs, toolCallId);
  if (!cur?.task?.status || !TERMINAL_TASK_STATUSES.has(cur.task.status)) return null;
  if (!attempt || attempt <= (cur.task.attempt ?? 1)) return null;
  const task: NonNullable<ToolCall['task']> = { attempt };
  if (cur.task.label) task.label = cur.task.label;
  return patchToolCall(msgs, toolCallId, { task });
}

// syncTaskCard brings a spawn card in line with a task's state: a newer attempt
// re-arms it, a terminal outcome folds in. The one entry point for live events,
// REST and the reconnect sweep (invariant 21).
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

// appendToolProgress accumulates the live output a running tool pushed — the
// wire carries deltas, each the next piece.
export function appendToolProgress(msgs: Msgs, toolCallId: string, delta: string, renderer?: string): Msgs | null {
  if (!delta) return null;
  const cur = findToolCall(msgs, toolCallId);
  // A call that already has its result is finished; a late delta must not reopen it.
  if (!cur || cur.output !== null) return null;
  return patchToolCall(msgs, toolCallId, {
    progress: (cur.progress || '') + delta,
    renderer: renderer || cur.renderer,
  });
}

// appendHandoffPart records a completed agent switch inside the live turn. Live-only:
// a reload conveys it via the transfer_to_* tool-call card (see the isomorphism test).
export function appendHandoffPart(msgs: Msgs, handoff: { from: string; to: string; fromId?: string; toId?: string }): Msgs | null {
  const turn = lastTurn(msgs);
  if (!turn) return null;
  const content = handoff.from + ' → ' + handoff.to;
  if (runShows(msgs, turn, pt => pt.type === 'handoff' && pt.content === content)) return null;
  return withParts(msgs, turn, [...(turn.parts || []), { type: 'handoff', content, ...handoff }]);
}
