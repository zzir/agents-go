import { describe, it, expect } from 'vitest';
import { providerMeta } from '@/lib/providers';

describe('provider effort options', () => {
  // The adaptive path takes every effort up to max; "none" is offered nowhere,
  // since the Anthropic adapter refuses it by name.
  it('anthropic offers the efforts adaptive thinking takes', () => {
    const efforts = providerMeta('anthropic').effortOptions.map(([v]) => v);
    expect(efforts).toEqual(['', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max']);
    expect(efforts).not.toContain('none');
  });
});
