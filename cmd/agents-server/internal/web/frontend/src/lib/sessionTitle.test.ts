import { describe, expect, it } from 'vitest';
import { sessionTitle } from '@/lib/sessionTitle';

describe('sessionTitle', () => {
  it('joins the target and the brief, the brief on one line', () => {
    expect(sessionTitle('ship', ' fix  the\nboard ')).toBe('ship: fix the board');
  });

  it('is the target alone without a brief', () => {
    expect(sessionTitle('ship', '   ')).toBe('ship');
  });

  it('caps at 40 characters, counted as characters, with an ellipsis', () => {
    const t = sessionTitle('工作流', 'x'.repeat(50));
    expect([...t].length).toBe(40);
    expect(t.endsWith('…')).toBe(true);
    expect(sessionTitle('ship', 'x'.repeat(34))).toHaveLength(40);
  });
});
