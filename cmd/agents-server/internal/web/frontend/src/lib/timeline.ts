// Structured display data the server derives from tool-call items, so the
// frontend never parses wire-format item JSON.
interface ToolCallDisplay {
  call_id?: string;
  name?: string;
  arguments?: string;
  output?: string;
  // Set on an error annotation that was a guardrail block (reuses the display
  // column) so a reloaded turn rebuilds the typed "Blocked by guardrail" card.
  guardrail?: string;
  stage?: string;
  // Patched onto a spawn_task call when its background task ends — the durable
  // truth the task card is rebuilt from on reload (the hub run is GC'd).
  task_id?: string;
  task_label?: string;
  task_status?: string;
  task_summary?: string;
}

interface Message {
  id?: number;
  run_id?: string;
  role: string;
  content?: string;
  display?: ToolCallDisplay;
  compacted?: boolean;
}

export function formatDuration(ms: number): string {
  if (ms < 1000) return '<1s';
  const s = ms / 1000;
  if (s < 60) return s.toFixed(1) + 's';
  const m = Math.floor(s / 60);
  const rs = Math.round(s % 60);
  if (m < 60) return m + 'm ' + rs + 's';
  const h = Math.floor(m / 60);
  const rm = m % 60;
  return h + 'h ' + rm + 'm';
}

// The ONE ToolCall / TurnPart definition — the streaming path (streamReducer),
// the replay path (buildTimeline) and every renderer (ChatView, ToolCallCard)
// import these. A second local copy is how the two paths drift apart.
interface ToolCall {
  tool_call_id: string;
  tool_name: string;
  arguments: string;
  output: string | null;
  status: string | null;
  needs_approval?: boolean;
  // Terminal task outcome for a spawn_task call, from the display projection.
  task?: { id?: string; label?: string; status?: string; summary?: string };
}

interface ToolsPart {
  type: 'tools';
  toolCalls: ToolCall[];
}

interface TextPart {
  type: 'text';
  content: string;
}

interface ErrorPart {
  type: 'error';
  content: string;
  guardrail?: string;
  stage?: string;
}

interface CancelledPart {
  type: 'cancelled';
  content?: string;
}

interface ThinkingPart {
  type: 'thinking';
  content: string;
}

// A live-only marker for an agent switch (run.handoff), rendered inside the
// turn's process timeline. Never persisted — on reload the transfer_to_* tool
// call card conveys the same information.
interface HandoffPart {
  type: 'handoff';
  content: string;
}

type TurnPart = ToolsPart | TextPart | ErrorPart | CancelledPart | ThinkingPart | HandoffPart;

interface UserEntry {
  role: 'user';
  content: string;
  // Absent on entries not yet persisted: the sender's optimistic bubble and
  // the bubble a watching browser builds from run.started's input.
  messageId?: number;
  runId?: string;
}

interface SystemEntry {
  role: 'system';
  content: string;
  messageId: number | undefined;
}

interface TurnEntry {
  role: 'turn';
  parts: TurnPart[];
  // Persisted turns carry the anchoring row id; a live turn assembled from
  // stream events has none until the post-run reload swaps it in.
  messageId?: number;
  runId?: string;
}

interface CompactionEntry {
  role: 'compaction';
  content: string;
  messageId: number | undefined;
}

type TimelineEntry = UserEntry | SystemEntry | TurnEntry | CompactionEntry;

interface HookEvent {
  agent_name?: string;
  tool_name?: string;
  from?: string;
  to?: string;
  detail?: string;
}

interface ToolCallPatch {
  output?: string;
  status?: string | null;
  tool_name?: string;
  arguments?: string;
  needs_approval?: boolean;
}

export type { Message, ToolCall, ToolsPart, TextPart, ErrorPart, CancelledPart, ThinkingPart, HandoffPart, TurnPart, TurnEntry, UserEntry, TimelineEntry, HookEvent, ToolCallPatch };

export function buildTimeline(msgs: Message[] | null | undefined): TimelineEntry[] {
  if (!msgs) return [];
  const timeline: TimelineEntry[] = [];
  const pendingTC: Record<string, ToolCall> = {};
  let turn: TurnEntry | null = null;
  const ensureTurn = (): void => {
    if (!turn) { turn = { role: 'turn', parts: [], messageId: 0 }; timeline.push(turn); }
  };
  const finishTurn = (): void => { turn = null; };
  for (const m of msgs) {
    if (m.role === 'compaction') {
      finishTurn();
      timeline.push({ role: 'compaction', content: m.content || '', messageId: m.id });
      continue;
    }
    // Compacted rows render exactly like live ones: compaction soft-deletes
    // only the model's replay context (GetItems), never the visible history.
    // Dropping them here made whole runs vanish after a compaction.
    if (m.role === 'user') {
      finishTurn();
      if (m.content) timeline.push({ role: 'user', content: m.content, messageId: m.id, runId: m.run_id });
    } else if (m.role === 'tool_call') {
      const d = m.display;
      if (d?.call_id) {
        ensureTurn();
        if (m.id) turn!.messageId = m.id;
        if (m.run_id) turn!.runId = m.run_id;
        const tc: ToolCall = { tool_call_id: d.call_id, tool_name: d.name || '', arguments: d.arguments || '', output: null, status: null };
        if (d.task_id || d.task_status) tc.task = { id: d.task_id, label: d.task_label, status: d.task_status, summary: d.task_summary };
        pendingTC[d.call_id] = tc;
        const last = turn!.parts[turn!.parts.length - 1];
        if (last && last.type === 'tools') { (last as ToolsPart).toolCalls.push(tc); }
        else { turn!.parts.push({ type: 'tools', toolCalls: [tc] }); }
      }
    } else if (m.role === 'tool_output') {
      const d = m.display;
      if (turn && m.id) (turn as TurnEntry).messageId = m.id;
      if (d?.call_id && pendingTC[d.call_id]) {
        pendingTC[d.call_id].output = d.output || m.content || '';
        pendingTC[d.call_id].status = 'completed';
      }
    } else if (m.role === 'error' && m.content) {
      ensureTurn();
      if (m.id) turn!.messageId = m.id;
      if (m.run_id) turn!.runId = m.run_id;
      const d = m.display;
      turn!.parts.push({ type: 'error', content: m.content, guardrail: d?.guardrail, stage: d?.stage });
    } else if (m.role === 'cancelled') {
      // A run stopped by the user (or a deadline). Content is optional — the
      // card renders a fixed label — so this branch does not gate on it.
      ensureTurn();
      if (m.id) turn!.messageId = m.id;
      if (m.run_id) turn!.runId = m.run_id;
      turn!.parts.push({ type: 'cancelled', content: m.content || '' });
    } else if (m.role === 'reasoning') {
      if (m.content) {
        ensureTurn();
        if (m.id) turn!.messageId = m.id;
        if (m.run_id) turn!.runId = m.run_id;
        turn!.parts.push({ type: 'thinking', content: m.content });
      }
    } else if (m.role === 'system' && m.content) {
      finishTurn();
      timeline.push({ role: 'system', content: m.content, messageId: m.id });
    } else if (m.content) {
      ensureTurn();
      if (m.id) turn!.messageId = m.id;
      if (m.run_id) turn!.runId = m.run_id;
      turn!.parts.push({ type: 'text', content: m.content });
    }
  }
  finishTurn();
  return timeline;
}

// findToolCall returns the tool call with the given id (searching newest-first),
// or null. Used to read a call's current state before patching it.
export function findToolCall(messages: TimelineEntry[], toolCallId: string): ToolCall | null {
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i].role !== 'turn') continue;
    for (const part of (messages[i] as TurnEntry).parts) {
      if (part.type !== 'tools') continue;
      const tc = (part as ToolsPart).toolCalls.find(t => t.tool_call_id === toolCallId);
      if (tc) return tc;
    }
  }
  return null;
}

export function patchToolCall(messages: TimelineEntry[], toolCallId: string, patch: ToolCallPatch): TimelineEntry[] | null {
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i].role !== 'turn') continue;
    const turnMsg = messages[i] as TurnEntry;
    const parts = [...turnMsg.parts];
    for (let j = parts.length - 1; j >= 0; j--) {
      if (parts[j].type !== 'tools') continue;
      const tcs = (parts[j] as ToolsPart).toolCalls;
      const idx = tcs.findIndex(tc => tc.tool_call_id === toolCallId);
      if (idx >= 0) {
        const newTcs = [...tcs];
        newTcs[idx] = { ...newTcs[idx], ...patch };
        parts[j] = { ...parts[j], toolCalls: newTcs } as ToolsPart;
        const newMsgs = [...messages];
        newMsgs[i] = { ...turnMsg, parts };
        return newMsgs;
      }
    }
  }
  return null;
}
