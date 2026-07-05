// Structured display data the server derives from tool-call items, so the
// frontend never parses wire-format item JSON.
interface ToolCallDisplay {
  call_id?: string;
  name?: string;
  arguments?: string;
  output?: string;
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

interface ToolCall {
  tool_call_id: string;
  tool_name: string;
  arguments: string;
  output: string | null;
  status: string | null;
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
}

interface ThinkingPart {
  type: 'thinking';
  content: string;
}

type TurnPart = ToolsPart | TextPart | ErrorPart | ThinkingPart;

interface UserEntry {
  role: 'user';
  content: string;
  messageId: number | undefined;
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
  messageId: number;
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
  status?: string;
}

export type { Message, ToolCall, ToolsPart, TextPart, TurnPart, TimelineEntry, HookEvent, ToolCallPatch };

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
      turn!.parts.push({ type: 'error', content: m.content });
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
