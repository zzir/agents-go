import { describe, expect, it } from 'vitest';
import { numberDraft, parseOptionalPositive, parseOptionalPositiveDecimal, parseWholeNumber } from '@/lib/numericField';

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

  it('an optional count is 0 only when left empty; zero, negatives, fractions and words are refused', () => {
    expect(parseOptionalPositive('', 'Expires in days')).toBe(0);
    expect(parseOptionalPositive(' ', 'Expires in days')).toBe(0);
    expect(parseOptionalPositive('30', 'Expires in days')).toBe(30);
    for (const bad of ['0', '-1', '1.5', 'abc']) {
      expect(() => parseOptionalPositive(bad, 'Expires in days')).toThrow('Expires in days must be a whole number of at least 1');
    }
  });

  it('a decimal quantity takes a fraction, and refuses zero, negatives and words', () => {
    expect(parseOptionalPositiveDecimal('', 'CPUs')).toBe(0);
    expect(parseOptionalPositiveDecimal('1.5', 'CPUs')).toBe(1.5);
    expect(parseOptionalPositiveDecimal(' 2 ', 'CPUs')).toBe(2);
    for (const bad of ['0', '0.0', '-1', '1.', '.5', 'abc', '1e3']) {
      expect(() => parseOptionalPositiveDecimal(bad, 'CPUs')).toThrow('CPUs must be a number above 0');
    }
  });
});
