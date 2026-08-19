import { describe, expect, it } from 'vitest';
import { traceEventFromRow, withSpanPayload } from '@/lib/useAgentSocket';

describe('traceEventFromRow', () => {
  it('parses a summary row and keeps its omitted mark', () => {
    const ev = traceEventFromRow({
      run_id: 'r1', kind: 'span', name: 'generation', detail: 'generation', span_id: 'sp1',
      data: '{"model":"m","input_tokens":5}', payload_omitted: true,
      started_at: '2026-08-19T00:00:00.000Z', ended_at: '2026-08-19T00:00:01.500Z',
    });
    expect(ev).toMatchObject({ kind: 'span', type: 'generation', span_id: 'sp1', payloadOmitted: true, duration: '1.5s' });
    expect(ev.data).toEqual({ model: 'm', input_tokens: 5 });
    // A pre-JSON row reads as no data, and a whole row is not marked.
    expect(traceEventFromRow({ name: 'old', data: 'not json' })).toMatchObject({ data: null, payloadOmitted: false });
  });
});

describe('withSpanPayload', () => {
  const runs = {
    r1: [
      { kind: 'span', name: 'agent', span_id: 'a' },
      { kind: 'span', name: 'generation', span_id: 'g', data: { model: 'm', input_tokens: 5 }, payloadOmitted: true },
    ],
    r2: [{ kind: 'span', name: 'generation', span_id: 'g2', payloadOmitted: true }],
  };
  const full = traceEventFromRow({ name: 'generation', span_id: 'g', data: '{"model":"m","input_tokens":5,"input":[{"role":"user"}]}' });

  it('swaps the whole span in where the run holds it, and only there', () => {
    const next = withSpanPayload(runs, 'r1', 'g', full);
    expect(next.r1[1]).toMatchObject({ payloadOmitted: false, data: { model: 'm', input_tokens: 5, input: [{ role: 'user' }] } });
    expect(next.r1[0]).toBe(runs.r1[0]);
    expect(next.r2).toBe(runs.r2);
  });

  it('is the same object when the run or the span is not there', () => {
    expect(withSpanPayload(runs, 'r9', 'g', full)).toBe(runs);
    expect(withSpanPayload(runs, 'r1', 'nope', full)).toBe(runs);
  });
});
