import type { AttachmentMeta } from '@/lib/attachments';
import { parseTaskNotification } from '@/lib/protocol';
import { originText, type EntryView, type SystemEntry, type TimelineEntry } from '@/lib/timeline';

// RunLabels is what the trace panel reads off the timeline: which index is
// which run (turns, and the user messages a card can jump to), each run's
// label, and the branched-away runs.
export interface RunLabels {
  turnRunMap: Record<number, string>;
  userRunMap: Record<number, string>;
  runLabels: Record<string, string>;
  staleRuns: Set<string>;
}

// questionLabel names an exchange by its user message: the text, or the images;
// null for neither.
function questionLabel(content: string | undefined, attachments: AttachmentMeta[] | undefined, labelFor: (content: string) => string): string | null {
  if (content) return labelFor(content);
  const n = attachments?.length || 0;
  if (n === 0) return null;
  return n === 1 ? 'Image' : `${n} images`;
}

// labelRuns labels every traced run by the user message its exchange started
// from, and maps the rendered timeline's turns and user messages to their runs.
// labelFor phrases a message's text.
export function labelRuns(messages: TimelineEntry[], entries: EntryView[], traceRuns: Record<string, unknown>, labelFor: (content: string) => string): RunLabels {
  const turnRunMap: Record<number, string> = {};
  const userRunMap: Record<number, string> = {};
  const runLabels: Record<string, string> = {};
  // A workflow-started note is the question of an exchange no run asked: the wake-up
  // run that delivers that execution's result is labeled by it and jumps to it.
  const noteIdxByTask: Record<string, number> = {};
  let turnIdx = 0;
  for (let i = 0; i < messages.length; i++) {
    const entry = messages[i];
    if (entry.role === 'system') {
      if (entry.note?.taskId) noteIdxByTask[entry.note.taskId] = i;
      continue;
    }
    if (entry.role === 'user') {
      const rid = entry.runId;
      if (!rid || !traceRuns[rid]) continue;
      // Labeled from the user message, so a run whose reply produced no turn
      // still shows its question.
      const notif = parseTaskNotification(entry.content);
      // A notification does not render, so it anchors no jump — unless the
      // execution's start left a note, which then is the anchor.
      if (!notif) userRunMap[i] = rid;
      if (!runLabels[rid]) {
        const label = questionLabel(entry.content, entry.attachments, labelFor);
        if (label) runLabels[rid] = label;
      }
      if (notif) {
        const noted = notif.items.find(it => it.taskId && noteIdxByTask[it.taskId] !== undefined);
        if (noted?.taskId) {
          const idx = noteIdxByTask[noted.taskId];
          const note = (messages[idx] as SystemEntry).note!;
          userRunMap[idx] = rid;
          runLabels[rid] = '▶ ' + (note.workflowName || noted.label) + ' (' + originText(note.origin) + ')';
        }
      }
    } else if (entry.role === 'turn') {
      const rid = entry.runId;
      if (rid && traceRuns[rid]) {
        turnRunMap[i] = rid;
        let question: string | null = null;
        for (let j = i - 1; j >= 0; j--) {
          const prev = messages[j];
          if (prev.role !== 'user') continue;
          question = questionLabel(prev.content, prev.attachments, labelFor);
          // The turn's run overwrites the message's own run_id (whichever run
          // first produced it — after a regenerate, a branched-away attempt);
          // on the active branch a message has one turn.
          if (!parseTaskNotification(prev.content)) userRunMap[j] = rid;
          break;
        }
        if (!runLabels[rid]) runLabels[rid] = question ?? 'Turn ' + (turnIdx + 1);
      }
      turnIdx++;
    }
  }
  // Runs whose turn is not in the rendered timeline (a regenerated answer
  // branched away) are labeled from the entries and marked stale, so "5 traces,
  // 3 exchanges" reads as what it is.
  const staleRuns = new Set<string>();
  let lastUser: EntryView | null = null;
  for (const e of entries) {
    if (e.role === 'user' && (e.content || e.attachments?.length)) lastUser = e;
    const rid = e.run_id;
    if (!rid || !traceRuns[rid]) continue;
    if (e.on_path === false) staleRuns.add(rid);
    if (!runLabels[rid] && lastUser) {
      const label = questionLabel(lastUser.content, lastUser.attachments, labelFor);
      if (label) runLabels[rid] = label;
    }
  }
  return { turnRunMap, userRunMap, runLabels, staleRuns };
}
