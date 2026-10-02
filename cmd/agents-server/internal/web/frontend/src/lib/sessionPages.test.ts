import { describe, expect, it } from 'vitest';
import { SESSION_LISTS, SESSION_PAGE, hasMoreSessions, sessionListKey } from '@/lib/sessionPages';

describe('sessionPages', () => {
  it('a page is "more" only while its unpinned rows fill the limit', () => {
    const pinned = { pinned: true };
    const plain = { pinned: false };
    expect(hasMoreSessions([pinned, pinned, plain, plain], 2)).toBe(true);
    expect(hasMoreSessions([pinned, pinned, plain], 2)).toBe(false);
    expect(hasMoreSessions([], SESSION_PAGE)).toBe(false);
  });

  it('one invalidation reaches the whole list and every page', () => {
    expect(SESSION_LISTS.test('sessions')).toBe(true);
    expect(SESSION_LISTS.test(sessionListKey(100, ''))).toBe(true);
    expect(SESSION_LISTS.test(sessionListKey(200, 'deploy'))).toBe(true);
    expect(SESSION_LISTS.test('sessions-admin')).toBe(false);
    expect(SESSION_LISTS.test('mcp-servers')).toBe(false);
  });
});
