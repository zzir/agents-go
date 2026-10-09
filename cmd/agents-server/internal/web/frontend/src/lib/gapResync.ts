// The client's answer to a run.gap: re-subscribe from the last good cursor when
// the hub's ring may still hold the range; a gap at seq 0 or a cursor that gapped
// once already names an evicted range, and asking again would loop for the run's life.

export interface GapResync { at: number; cursor: number }

// resyncAfterGap says whether to re-subscribe from lastGood given the run's
// previous resync and the clock; minInterval caps a connection that keeps falling
// behind to one ask per interval.
export function resyncAfterGap(prev: GapResync | undefined, lastGood: number, now: number, minInterval = 5000): boolean {
  if (lastGood === 0) return false;
  if (prev && (prev.cursor === lastGood || prev.at > now - minInterval)) return false;
  return true;
}
