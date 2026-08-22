import { describe, expect, it } from 'vitest';
import { MAX_MESSAGE_BYTES, isTooLarge } from '@/lib/messageSize';

describe('isTooLarge', () => {
  it('measures UTF-8 bytes, not characters', () => {
    expect(isTooLarge('a'.repeat(MAX_MESSAGE_BYTES))).toBe(false);
    expect(isTooLarge('a'.repeat(MAX_MESSAGE_BYTES + 1))).toBe(true);
    // Three bytes each: a third of the limit in characters already overflows.
    expect(isTooLarge('中'.repeat(MAX_MESSAGE_BYTES / 3 + 1))).toBe(true);
  });
});
