import { describe, expect, it } from 'vitest';
import { activates } from '@/lib/activation';

describe('activates', () => {
  const row = {} as EventTarget;
  const inner = {} as EventTarget;

  it('is the row\'s own Enter or Space', () => {
    expect(activates({ key: 'Enter', target: row, currentTarget: row })).toBe(true);
    expect(activates({ key: ' ', target: row, currentTarget: row })).toBe(true);
    expect(activates({ key: 'Escape', target: row, currentTarget: row })).toBe(false);
  });

  // A button inside the row, or a menu item its menu rendered elsewhere:
  // the key is theirs, and taking it would open the row instead.
  it('is not a key pressed on a control inside the row', () => {
    expect(activates({ key: 'Enter', target: inner, currentTarget: row })).toBe(false);
    expect(activates({ key: ' ', target: inner, currentTarget: row })).toBe(false);
  });
});
