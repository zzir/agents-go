import { describe, expect, it } from 'vitest';
import { groupTraceRuns } from '@/lib/traceGroups';

describe('groupTraceRuns', () => {
  // A wake-up run joins the exchange that spawned the task, after it; a
  // wake-up of a wake-up follows the same chain to the root.
  it('chains wake-up runs under the run that started them', () => {
    const { groups, rootOf } = groupTraceRuns(['r1', 'r2', 'w1', 'w2'], { w1: 'r1', w2: 'w1' });
    expect(groups).toEqual([{ rootId: 'r1', runIds: ['r1', 'w1', 'w2'] }, { rootId: 'r2', runIds: ['r2'] }]);
    expect(rootOf).toEqual({ r1: 'r1', w1: 'r1', w2: 'r1', r2: 'r2' });
  });

  // Retention may have dropped the parent: the wake-up run is listed on its own.
  it('lists a run whose parent is gone as a root', () => {
    const { groups, rootOf } = groupTraceRuns(['w1'], { w1: 'gone' });
    expect(groups).toEqual([{ rootId: 'w1', runIds: ['w1'] }]);
    expect(rootOf.w1).toBe('w1');
  });

  it('keeps insertion order and ignores a run naming itself', () => {
    const { groups } = groupTraceRuns(['b', 'a'], { a: 'a' });
    expect(groups.map(g => g.rootId)).toEqual(['b', 'a']);
  });
});
