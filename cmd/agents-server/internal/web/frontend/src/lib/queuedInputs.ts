import type { TimelineEntry } from '@/lib/timeline';

// A queued input a run read mid-way, placed on its trace.
export interface TraceMarker {
  // When the run recorded it (ms); a marker without a time sits at the end.
  at?: number;
  // The input, shortened for a title, and whole for the opened row.
  label: string;
  text: string;
}

const LABEL_CHARS = 60;

// queuedInputMarkers finds, per run, the inputs the run read from its queue:
// a run has one prompt, so every later user message under the same run id is
// one (live, the bubble also says so). The first message is never a marker.
export function queuedInputMarkers(messages: TimelineEntry[]): Record<string, TraceMarker[]> {
  const seen = new Set<string>();
  const out: Record<string, TraceMarker[]> = {};
  for (const m of messages) {
    if (m.role !== 'user' || !m.runId) continue;
    if (!seen.has(m.runId) && m.injected === undefined) {
      seen.add(m.runId);
      continue;
    }
    seen.add(m.runId);
    const text = m.content.replace(/\s+/g, ' ').trim();
    const label = text.length > LABEL_CHARS ? text.slice(0, LABEL_CHARS - 1) + '…' : text || 'image';
    (out[m.runId] ||= []).push({ at: m.createdAt, label, text });
  }
  return out;
}
