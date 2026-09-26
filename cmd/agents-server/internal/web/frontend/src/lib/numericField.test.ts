import { describe, expect, it } from 'vitest';
import { numberDraft, parseWholeNumber } from '@/lib/numericField';

describe('numericField', () => {
  it('a stored zero or nothing is an empty box; any other number shows as typed', () => {
    expect(numberDraft(0)).toBe('');
    expect(numberDraft(undefined)).toBe('');
    expect(numberDraft(null)).toBe('');
    expect(numberDraft(-1)).toBe('-1');
    expect(numberDraft(50000)).toBe('50000');
  });

  it('parses empty as 0, keeps a negative, and refuses what is not a whole number', () => {
    expect(parseWholeNumber('', 'Max turns')).toBe(0);
    expect(parseWholeNumber('   ', 'Max turns')).toBe(0);
    expect(parseWholeNumber('-1', 'Max retry attempts')).toBe(-1);
    expect(parseWholeNumber(' 42 ', 'Max turns')).toBe(42);
    expect(() => parseWholeNumber('abc', 'Max turns')).toThrow('Max turns is not a whole number');
    expect(() => parseWholeNumber('1.5', 'Window size')).toThrow('Window size is not a whole number');
  });
});
