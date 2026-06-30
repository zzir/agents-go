interface Message {
  id?: number;
  run_id?: string;
  role: string;
  content?: string;
  item?: string;
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

type TurnPart = ToolsPart | TextPart;

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
    if (m.compacted && (m.role === 'user' || m.role === 'assistant')) {
      finishTurn();
      if (m.role === 'user' && m.content) {
        timeline.push({ role: 'user', content: m.content, messageId: m.id, runId: m.run_id });
      } else if (m.content) {
        ensureTurn();
        if (m.id) turn!.messageId = m.id;
        if (m.run_id) turn!.runId = m.run_id;
        turn!.parts.push({ type: 'text', content: m.content });
        finishTurn();
      }
      continue;
    }
    if (m.compacted) continue;
    if (m.role === 'user') {
      finishTurn();
      if (m.content) timeline.push({ role: 'user', content: m.content, messageId: m.id, runId: m.run_id });
    } else if (m.role === 'tool_call') {
      try {
        const item = JSON.parse(m.item!);
        ensureTurn();
        if (m.id) turn!.messageId = m.id;
        if (m.run_id) turn!.runId = m.run_id;
        const tc: ToolCall = { tool_call_id: item.call_id, tool_name: item.name, arguments: item.arguments || '', output: null, status: null };
        pendingTC[item.call_id] = tc;
        const last = turn!.parts[turn!.parts.length - 1];
        if (last && last.type === 'tools') { (last as ToolsPart).toolCalls.push(tc); }
        else { turn!.parts.push({ type: 'tools', toolCalls: [tc] }); }
      } catch (_) {}
    } else if (m.role === 'tool_output') {
      try {
        const item = JSON.parse(m.item!);
        if (turn && m.id) (turn as TurnEntry).messageId = m.id;
        if (pendingTC[item.call_id]) {
          pendingTC[item.call_id].output = item.output || m.content;
          pendingTC[item.call_id].status = 'completed';
        }
      } catch (_) {}
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

export function formatHookDetail(ev: HookEvent): string {
  const parts: string[] = [];
  if (ev.agent_name) parts.push(ev.agent_name);
  if (ev.tool_name) parts.push('→ ' + ev.tool_name);
  if (ev.from && ev.to) parts.push(ev.from + ' → ' + ev.to);
  if (ev.detail) parts.push(ev.detail);
  return parts.join(' ');
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
