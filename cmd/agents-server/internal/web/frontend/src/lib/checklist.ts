import type { TimelineEntry } from '@/lib/timeline';

export const CHECKLIST_TOOL = 'todo_write';
// The session memory a run keeps its latest list under (store.ChecklistKey).
export const CHECKLIST_KEY = 'checklist.md';

export interface ChecklistItem { content: string; status: string }

// The list a session is working from: the newest accepted todo_write call.
export interface Checklist { callId: string; items: ChecklistItem[]; done: number }

// parseChecklist reads todo_write's arguments; null when they hold no list.
export function parseChecklist(args: string): ChecklistItem[] | null {
  try {
    const parsed = JSON.parse(args);
    if (!parsed || !Array.isArray(parsed.todos)) return null;
    return (parsed.todos as Array<{ content?: string; status?: string }>)
      .filter(td => td && typeof td.content === 'string')
      .map(td => ({ content: td.content as string, status: td.status || 'pending' }));
  } catch {
    return null;
  }
}

// latestChecklist is the newest todo_write on a timeline — the list the work
// is at. A call that was rejected or never ran changed nothing and is skipped.
export function latestChecklist(messages: TimelineEntry[]): Checklist | null {
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    if (m.role !== 'turn') continue;
    for (let j = m.parts.length - 1; j >= 0; j--) {
      const p = m.parts[j];
      if (p.type !== 'tools') continue;
      for (let k = p.toolCalls.length - 1; k >= 0; k--) {
        const tc = p.toolCalls[k];
        if (tc.tool_name !== CHECKLIST_TOOL || tc.status === 'rejected' || tc.not_run) continue;
        const items = parseChecklist(tc.arguments);
        if (items) return { callId: tc.tool_call_id, items, done: items.filter(it => it.status === 'completed').length };
      }
    }
  }
  return null;
}
