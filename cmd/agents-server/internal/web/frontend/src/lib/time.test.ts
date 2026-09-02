import { describe, expect, it } from 'vitest';
import { formatTime, shortDate, shortTime } from '@/lib/time';


describe('formatTime', () => {
  it('renders month, day and a 24h clock; seconds only in the long style', () => {
    const short = formatTime('2026-09-01T07:19:42Z');
    const long = formatTime('2026-09-01T07:19:42Z', 'long');
    expect(short).toMatch(/\d{2}:\d{2}/);
    expect(short).not.toMatch(/:\d{2}:\d{2}/);
    expect(long).toMatch(/\d{2}:\d{2}:\d{2}/);
    expect(short).not.toMatch(/AM|PM/);
  });

  it('adds the year only when it is not the current one', () => {
    const thisYear = new Date().getFullYear();
    expect(formatTime(`${thisYear}-03-04T12:00:00Z`)).not.toContain(String(thisYear));
    expect(formatTime('2001-03-04T12:00:00Z')).toContain('2001');
  });

  it('is empty for nothing or garbage', () => {
    expect(formatTime('')).toBe('');
    expect(formatTime('yesterday')).toBe('');
    expect(shortTime(undefined)).toBe('');
    expect(shortDate('nope')).toBe('');
  });
});
