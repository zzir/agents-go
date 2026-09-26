import { describe, expect, it } from 'vitest';
import { labelRuns } from '@/lib/runLabels';
import { buildTimeline, type EntryView } from '@/lib/timeline';

const asIs = (content: string) => content;
const image = (id: string) => ({ id, url: `https://cdn/${id}` });

// One run's exchange as the server stores it: the user row and the answer.
function exchange(run: string, question: string, attachments?: EntryView['attachments']): EntryView[] {
  return [
    { id: run + '-u', run_id: run, kind: 'item', role: 'user', content: question, attachments },
    { id: run + '-a', run_id: run, kind: 'item', role: 'assistant', content: 'ok', display: { kind: 'message', text: 'ok' } },
  ];
}

describe('labelRuns', () => {
  it('labels every run from its own user message, none from a turn count', () => {
    const rows = [...exchange('r1', 'first'), ...exchange('r2', 'second'), ...exchange('r3', 'third')];
    const { runLabels, userRunMap, turnRunMap, staleRuns } = labelRuns(buildTimeline(rows), rows, { r1: [], r2: [], r3: [] }, asIs);
    expect(runLabels).toEqual({ r1: 'first', r2: 'second', r3: 'third' });
    expect(Object.values(runLabels).some(l => l.startsWith('Turn '))).toBe(false);
    expect(userRunMap).toEqual({ 0: 'r1', 2: 'r2', 4: 'r3' });
    expect(turnRunMap).toEqual({ 1: 'r1', 3: 'r2', 5: 'r3' });
    expect(staleRuns.size).toBe(0);
  });

  it('labels an image-only message by its images', () => {
    const rows = [...exchange('r1', '', [image('p')]), ...exchange('r2', '', [image('p'), image('q')])];
    const { runLabels } = labelRuns(buildTimeline(rows), rows, { r1: [], r2: [] }, asIs);
    expect(runLabels).toEqual({ r1: 'Image', r2: '2 images' });
  });

  it('labels a run the session branched away from by the message it answered, and marks it stale', () => {
    // A regenerate answers the same message again: the first attempt's turn
    // leaves the timeline while its entries stay.
    const [user] = exchange('r0', 'question');
    const first: EntryView = { id: 'r1-a', run_id: 'r1', kind: 'item', role: 'assistant', content: 'one', display: { kind: 'message', text: 'one' }, on_path: false };
    const again: EntryView = { id: 'r2-a', run_id: 'r2', kind: 'item', role: 'assistant', content: 'two', display: { kind: 'message', text: 'two' } };
    const rows = [user, first, again];
    const { runLabels, staleRuns, turnRunMap } = labelRuns(buildTimeline(rows), rows, { r1: [], r2: [] }, asIs);
    expect(runLabels).toEqual({ r1: 'question', r2: 'question' });
    expect([...staleRuns]).toEqual(['r1']);
    expect(turnRunMap).toEqual({ 1: 'r2' });
  });

  it('phrases a task notification through labelFor and offers it no jump target', () => {
    const rows = [...exchange('r1', 'go'), ...exchange('r2', '[task-notification] Task "audit" (t1) completed. Result: fine')];
    const { runLabels, userRunMap } = labelRuns(buildTimeline(rows), rows, { r1: [], r2: [] }, c => (c.startsWith('[task-notification]') ? 'task result: audit' : c));
    expect(runLabels).toEqual({ r1: 'go', r2: 'task result: audit' });
    expect(userRunMap).toEqual({ 0: 'r1' });
  });
});
