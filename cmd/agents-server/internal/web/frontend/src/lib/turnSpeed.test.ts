import { describe, it, expect } from 'vitest';
import { turnSpeed } from '@/lib/turnSpeed';

const t0 = Date.UTC(2026, 9, 14, 0, 0, 0);
const at = (ms: number) => new Date(t0 + ms).toISOString();
const span = (type: string, from: number, to: number | null, data?: Record<string, unknown>) =>
  ({ kind: 'span', type, started_at: at(from), ended_at: to === null ? undefined : at(to), data: data ?? null });

describe('turnSpeed', () => {
  // Model time only: the tool call between the two generations and the agent
  // span around them do not slow the figure down.
  it("divides the generations' output tokens by their own time", () => {
    const events = [
      span('agent', 0, 20000),
      span('generation', 0, 2000, { input_tokens: 900, output_tokens: 400 }),
      span('function', 2000, 15000),
      span('generation', 15000, 18000, { input_tokens: 1300, output_tokens: 600 }),
    ];
    expect(turnSpeed(events)).toBe('200 tok/s');
  });

  // A generation still open, one that reported nothing (a call that failed
  // before usage came back) and one with no tokens are not a speed.
  it('is empty until a finished generation reports output tokens', () => {
    expect(turnSpeed([])).toBe('');
    expect(turnSpeed([span('generation', 0, null, { output_tokens: 50 })])).toBe('');
    expect(turnSpeed([span('generation', 0, 1000)])).toBe('');
    expect(turnSpeed([span('generation', 0, 1000, { output_tokens: 0 })])).toBe('');
    expect(turnSpeed([span('generation', 0, 1000, { output_tokens: 0 }), span('generation', 1000, 3000, { output_tokens: 500 })])).toBe('250 tok/s');
  });

  it('keeps a decimal below ten tokens a second', () => {
    expect(turnSpeed([span('generation', 0, 4000, { output_tokens: 30 })])).toBe('7.5 tok/s');
  });
});
