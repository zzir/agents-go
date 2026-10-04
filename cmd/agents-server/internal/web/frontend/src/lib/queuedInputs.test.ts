import { describe, it, expect } from 'vitest';
import { queuedInputMarkers } from '@/lib/queuedInputs';
import type { TimelineEntry } from '@/lib/timeline';

const user = (runId: string, content: string, extra: Record<string, unknown> = {}): TimelineEntry =>
  ({ role: 'user', content, runId, ...extra } as TimelineEntry);
const turn = (runId: string): TimelineEntry => ({ role: 'turn', runId, parts: [], messageId: '' } as unknown as TimelineEntry);

describe('queuedInputMarkers', () => {
  // The prompt is never a marker; the inputs read after it are, in order.
  it('marks every user message of a run after its prompt', () => {
    const got = queuedInputMarkers([
      user('r1', 'fix the bug', { createdAt: 1000 }),
      turn('r1'),
      user('r1', 'also add a test', { createdAt: 2000 }),
      turn('r1'),
      user('r2', 'next question', { createdAt: 3000 }),
      turn('r2'),
    ]);
    expect(got).toEqual({ r1: [{ at: 2000, label: 'also add a test' }] });
  });

  // Live, the bubble says it was injected before any reload marks it by order.
  it('takes a live injected bubble at its word and shortens the text', () => {
    const long = 'x'.repeat(80);
    const got = queuedInputMarkers([user('r1', 'go', { createdAt: 1 }), user('r1', long, { injected: 1, createdAt: 2 })]);
    expect(got.r1).toHaveLength(1);
    expect(got.r1[0].label).toHaveLength(60);
    expect(got.r1[0].label.endsWith('…')).toBe(true);
  });

  it('ignores messages with no run', () => {
    expect(queuedInputMarkers([user('', 'draft'), { role: 'user', content: 'x' } as TimelineEntry])).toEqual({});
  });
});
