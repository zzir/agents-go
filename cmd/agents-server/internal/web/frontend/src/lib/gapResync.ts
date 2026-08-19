// The client's answer to a run.gap: re-subscribe from the last good cursor
// when the hub's ring may still hold the range — this connection fell behind
// for a moment — and let it go when the ring cannot: a gap opening at seq 0
// came from a subscribe from 0 (the ring had moved past the run's start before
// this connection attached), and a cursor asked for once that gaps again names
// a range the ring has evicted, not one it is slow to send. Either would loop
// for the run's life, replaying the whole ring each time.

export interface GapResync { at: number; cursor: number }

// resyncAfterGap says whether to re-subscribe from lastGood, given the run's
// previous resync (if any) and the clock; minInterval keeps a connection that
// keeps falling behind to one ask per interval.
export function resyncAfterGap(prev: GapResync | undefined, lastGood: number, now: number, minInterval = 5000): boolean {
  if (lastGood === 0) return false;
  if (prev && (prev.cursor === lastGood || prev.at > now - minInterval)) return false;
  return true;
}
