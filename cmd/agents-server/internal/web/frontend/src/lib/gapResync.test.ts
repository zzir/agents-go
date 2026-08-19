import { describe, expect, it } from 'vitest';
import { resyncAfterGap } from '@/lib/gapResync';

describe('resyncAfterGap', () => {
  it('re-subscribes once from a cursor the ring may still hold', () => {
    expect(resyncAfterGap(undefined, 120, 10_000)).toBe(true);
    // The same cursor gapping again: the range is evicted, not slow.
    expect(resyncAfterGap({ at: 4_000, cursor: 120 }, 120, 10_000)).toBe(false);
    // A later cursor is a new gap — after the throttle.
    expect(resyncAfterGap({ at: 4_000, cursor: 120 }, 300, 10_000)).toBe(true);
    expect(resyncAfterGap({ at: 8_000, cursor: 120 }, 300, 10_000)).toBe(false);
  });

  it('never re-subscribes for a gap that opened at seq 0', () => {
    // A subscribe from 0 on a run past its ring produced it: nothing to regain.
    expect(resyncAfterGap(undefined, 0, 10_000)).toBe(false);
    expect(resyncAfterGap({ at: 0, cursor: 300 }, 0, 10_000)).toBe(false);
  });
});
