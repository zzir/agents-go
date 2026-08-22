// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from 'vitest';
import { TOKEN_KEY, clearToken, getToken, setToken } from '@/lib/api';

beforeEach(() => { localStorage.clear(); sessionStorage.clear(); });

describe('token storage', () => {
  it('persists an OAuth session across tabs and a token-mode login within one', () => {
    setToken('t1', { persist: true });
    expect(localStorage.getItem(TOKEN_KEY)).toBe('t1');
    expect(sessionStorage.getItem(TOKEN_KEY)).toBeNull();
    expect(getToken()).toBe('t1');

    setToken('t2', { persist: false });
    expect(sessionStorage.getItem(TOKEN_KEY)).toBe('t2');
    // One credential at a time: the other storage is emptied.
    expect(localStorage.getItem(TOKEN_KEY)).toBeNull();
    expect(getToken()).toBe('t2');
  });
  it('clears both', () => {
    setToken('t1', { persist: true });
    clearToken();
    expect(getToken()).toBe('');
    expect(localStorage.getItem(TOKEN_KEY)).toBeNull();
  });
});
