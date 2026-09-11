import { describe, expect, it } from 'vitest';
import { MAX_FRAME_BYTES, frameTooLarge } from '@/lib/messageSize';

describe('frameTooLarge', () => {
  const overhead = JSON.stringify({ type: 'run.create', payload: { session_id: 's', input: '' } }).length;

  it('measures the envelope as sent, in UTF-8 bytes', () => {
    const fits = 'a'.repeat(MAX_FRAME_BYTES - overhead);
    expect(frameTooLarge('run.create', { session_id: 's', input: fits })).toBe(false);
    expect(frameTooLarge('run.create', { session_id: 's', input: fits + 'a' })).toBe(true);
    // Three bytes each: a third of the limit in characters already overflows.
    expect(frameTooLarge('run.create', { session_id: 's', input: '中'.repeat(MAX_FRAME_BYTES / 3 + 1) })).toBe(true);
  });

  it('counts JSON escaping: a text under the limit can still overflow the frame', () => {
    const quotes = '"'.repeat(MAX_FRAME_BYTES / 2 + 1);
    expect(new TextEncoder().encode(quotes).length).toBeLessThan(MAX_FRAME_BYTES);
    expect(frameTooLarge('run.create', { session_id: 's', input: quotes })).toBe(true);
  });
});
