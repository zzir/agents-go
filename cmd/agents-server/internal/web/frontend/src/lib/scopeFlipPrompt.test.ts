import { describe, expect, it } from 'vitest';
import { scopeFlipPrompt } from '@/lib/scopeFlipPrompt';

describe('scopeFlipPrompt', () => {
  it('publishing names the row and what every member gains', () => {
    const p = scopeFlipPrompt('reviewer', 'global', { id: 'u1', label: 'you' });
    expect(p.title).toBe('Publish “reviewer”?');
    expect(p.content).toBe('Every member will see it. Its author keeps it and can still edit it.');
    expect(p.confirmButtonContent).toBe('Publish');
    expect(p.confirmButtonType).toBe('primary');
  });

  it('unpublishing names who it returns to, or that nobody is there', () => {
    const named = scopeFlipPrompt('reviewer', 'private', { id: 'u1', label: 'Ada' });
    expect(named.title).toBe('Unpublish “reviewer”?');
    expect(named.content).toBe('It returns to Ada alone; members using it lose access.');
    expect(named.confirmButtonContent).toBe('Unpublish');
    expect(named.confirmButtonType).toBe('danger');
    expect(scopeFlipPrompt('reviewer', 'private', { id: 'u1' }).content).toBe('It returns to its author alone; members using it lose access.');
    expect(scopeFlipPrompt('reviewer', 'private').content).toBe('Members using it lose access. It has no author to return to — transfer it first if someone should keep it.');
  });
});
