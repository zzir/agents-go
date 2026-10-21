import type { TraceSpanLike } from '@/lib/background';

// turnSpeed is a turn's output throughput as "233 tok/s": the generation
// spans' output tokens over their summed wall time, model time only (tool
// runs and approval waits are left out). '' until a finished span carries tokens.
export function turnSpeed(events: TraceSpanLike[]): string {
  let tokens = 0, ms = 0;
  for (const ev of events) {
    if (ev.kind !== 'span' || ev.type !== 'generation' || !ev.started_at || !ev.ended_at) continue;
    const n = Number(ev.data?.output_tokens) || 0;
    const dt = new Date(ev.ended_at).getTime() - new Date(ev.started_at).getTime();
    if (n <= 0 || !(dt > 0)) continue;
    tokens += n;
    ms += dt;
  }
  if (tokens === 0) return '';
  const rate = tokens / (ms / 1000);
  return (rate >= 10 ? Math.round(rate) : rate.toFixed(1)) + ' tok/s';
}

const SPEED_KEY = 'chat.turnSpeed';

/** Whether the turn footer shows the speed in the duration's place, in this browser. */
export function loadSpeedPref(): boolean {
  try { return localStorage.getItem(SPEED_KEY) === '1'; } catch { return false; }
}

export function saveSpeedPref(on: boolean): void {
  try {
    if (on) localStorage.setItem(SPEED_KEY, '1');
    else localStorage.removeItem(SPEED_KEY);
  } catch { /* a browser that refuses storage keeps the preference for the page */ }
}
