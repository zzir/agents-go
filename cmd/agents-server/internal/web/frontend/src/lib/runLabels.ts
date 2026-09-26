import type { AttachmentMeta } from '@/lib/attachments';
import { parseTaskNotification } from '@/lib/protocol';
import { originText, type EntryView, type SystemEntry, type TimelineEntry } from '@/lib/timeline';

// RunLabels is what the trace panel reads off the timeline: which timeline
// index is which run (the turns, and the user messages a card can jump to),
// every traced run's label, and the runs the session has branched away from.
export interface RunLabels {
  turnRunMap: Record<number, string>;
  userRunMap: Record<number, string>;
  runLabels: Record<string, string>;
  staleRuns: Set<string>;
}

// questionLabel names an exchange by its user message: the text, or for an
// image-only message the images; null when the message carries neither.
function questionLabel(content: string | undefined, attachments: AttachmentMeta[] | undefined, labelFor: (content: string) => string): string | null {
  if (content) return labelFor(content);
  const n = attachments?.length || 0;
  if (n === 0) return null;
  return n === 1 ? 'Image' : `${n} images`;
}

// labelRuns labels every traced run (a key of traceRuns) by the user message
// its exchange started from, and maps the rendered timeline's turns and user
// messages to their runs. labelFor phrases a message's text (a task
// notification reads as its result).
export function labelRuns(messages: TimelineEntry[], entries: EntryView[], traceRuns: Record<string, unknown>, labelFor: (content: string) => string): RunLabels {
  const turnRunMap: Record<number, string> = {};
  const userRunMap: Record<number, string> = {};
  const runLabels: Record<string, string> = {};
  // A workflow-started note is the question of an exchange no run asked:
  // the wake-up run that later delivers that execution's result is labeled
  // by it and jumps to it. Notes precede their results in the timeline.
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
      // Label runs from the user message directly, so a run whose reply
      // produced no visible turn still shows its question in the trace panel.
      const notif = parseTaskNotification(entry.content);
      // Notifications don't render, so they anchor no jump target — label
      // the run but keep it out of userRunMap — unless the execution's start
      // left a note, which then IS the anchor.
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
          // The turn's run OVERWRITES the one the user message carries: a
          // message's own run_id is whichever run first produced it — after
          // a regenerate, an attempt the session has branched away from. On
          // the active branch a message is followed by exactly one turn, so
          // there is nothing to contend over.
          if (!parseTaskNotification(prev.content)) userRunMap[j] = rid;
          break;
        }
        if (!runLabels[rid]) runLabels[rid] = question ?? 'Turn ' + (turnIdx + 1);
      }
      turnIdx++;
    }
  }
  // Runs whose turn is NOT in the rendered timeline: a regenerated answer
  // the session has since branched away from. Their traces are still listed
  // — the work happened — but the timeline has no turn to label them from.
  // Label them from the entries instead, and mark them, so "5 traces, 3
  // exchanges" reads as what it is rather than as a mismatch.
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
