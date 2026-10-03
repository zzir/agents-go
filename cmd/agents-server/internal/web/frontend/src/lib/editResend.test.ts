import { describe, expect, it, vi } from 'vitest';
import { canEditMessage, resendEdited } from '@/lib/editResend';

describe('editResend', () => {
  it('offers Edit on a stored message with a parent, at rest', () => {
    const idle = { running: false, pendingDecision: false };
    expect(canEditMessage({ entryId: 'e5', parentId: 'e2', content: 'x' }, idle)).toBe(true);
    // The first message has no parent to branch at.
    expect(canEditMessage({ entryId: 'e1', content: 'x' }, idle)).toBe(false);
    // Not yet stored: nothing to branch from.
    expect(canEditMessage({ content: 'x' }, idle)).toBe(false);
    expect(canEditMessage({ entryId: 'e5', parentId: 'e2', content: 'x' }, { running: true, pendingDecision: false })).toBe(false);
    expect(canEditMessage({ entryId: 'e5', parentId: 'e2', content: 'x' }, { running: false, pendingDecision: true })).toBe(false);
  });

  // The branch moves when the edit is SENT, to the message's parent, and
  // before the run starts; a send that fails puts the branch back.
  it('branches at the parent on send, then runs the text; a failed send rolls back', async () => {
    const calls: string[] = [];
    const deps = {
      branch: vi.fn(async (id: string) => { calls.push('branch:' + id); return { previous_leaf: 'leaf-old' }; }),
      reload: vi.fn(async () => { calls.push('reload'); }),
      send: vi.fn((text: string) => { calls.push('send:' + text); return true; }),
    };
    expect(await resendEdited(deps, 'e2', 'do Y')).toBe('sent');
    expect(calls).toEqual(['branch:e2', 'reload', 'send:do Y']);

    calls.length = 0;
    deps.send.mockImplementation((text: string) => { calls.push('send:' + text); return false; });
    expect(await resendEdited(deps, 'e2', 'do Y')).toBe('rolled_back');
    expect(calls).toEqual(['branch:e2', 'reload', 'send:do Y', 'branch:leaf-old', 'reload']);

    deps.branch.mockImplementation(async (id: string) => { if (id === 'leaf-old') throw new Error('down'); return { previous_leaf: 'leaf-old' }; });
    expect(await resendEdited(deps, 'e2', 'do Y')).toBe('stranded');
  });
});
