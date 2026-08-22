// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from 'vitest';
import { consumeAuthFragment, readHash, restoreReturnHash, stashReturnHash } from '@/lib/route';

beforeEach(() => {
  history.replaceState(null, '', '/');
  sessionStorage.clear();
});

describe('readHash', () => {
  it('names a conversation, its lens, or the hub', () => {
    window.location.hash = '#/session/abc-1/trace';
    expect(readHash()).toEqual({ sessionId: 'abc-1', panel: { kind: 'trace' }, hub: null });
    window.location.hash = '#/session/abc-1/task/t9';
    expect(readHash()).toEqual({ sessionId: 'abc-1', panel: { kind: 'task', taskId: 't9' }, hub: null });
    window.location.hash = '#/workflows';
    expect(readHash()).toEqual({ sessionId: null, panel: null, hub: 'definitions' });
    window.location.hash = '#/workflows/runs';
    expect(readHash().hub).toBe('runs');
  });
  it('reads nothing from an unknown or empty fragment', () => {
    window.location.hash = '#auth_code=x';
    expect(readHash()).toEqual({ sessionId: null, panel: null, hub: null });
  });
});

describe('consumeAuthFragment', () => {
  it('takes the code off the URL, once', () => {
    window.location.hash = '#auth_code=abc%2Fdef';
    expect(consumeAuthFragment()).toEqual({ code: 'abc/def' });
    expect(window.location.hash).toBe('');
    expect(consumeAuthFragment()).toEqual({});
  });
  it('takes an error tag the same way', () => {
    window.location.hash = '#auth_error=not_allowed';
    expect(consumeAuthFragment()).toEqual({ error: 'not_allowed' });
    expect(window.location.hash).toBe('');
  });
  it('leaves a view fragment alone', () => {
    window.location.hash = '#/session/abc';
    expect(consumeAuthFragment()).toEqual({});
    expect(window.location.hash).toBe('#/session/abc');
  });
});

describe('return hash', () => {
  it('stashes the view a sign-in started from and restores it once', () => {
    window.location.hash = '#/session/abc/trace';
    stashReturnHash();
    window.location.hash = '';
    restoreReturnHash();
    expect(window.location.hash).toBe('#/session/abc/trace');
    window.location.hash = '';
    restoreReturnHash();
    expect(window.location.hash).toBe('');
  });
  it('stashes nothing for the empty view', () => {
    stashReturnHash();
    expect(sessionStorage.getItem('auth_return_hash')).toBeNull();
  });
});
