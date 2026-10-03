// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { consumeAuthFragment, readHash, restoreReturnHash, settingsHash, stashReturnHash, writeHash } from '@/lib/route';

beforeEach(() => {
  history.replaceState(null, '', '/');
  sessionStorage.clear();
});

describe('readHash', () => {
  it('names a conversation, its lens, or the hub', () => {
    window.location.hash = '#/session/abc-1/trace';
    expect(readHash()).toEqual({ sessionId: 'abc-1', panel: { kind: 'trace' }, hub: null, settings: null });
    window.location.hash = '#/session/abc-1/task/t9';
    expect(readHash()).toEqual({ sessionId: 'abc-1', panel: { kind: 'task', taskId: 't9' }, hub: null, settings: null });
    window.location.hash = '#/workflows';
    expect(readHash()).toEqual({ sessionId: null, panel: null, hub: 'definitions', settings: null });
    window.location.hash = '#/workflows/runs';
    expect(readHash().hub).toBe('runs');
  });
  it('reads Settings as a parameter over the view it covers', () => {
    window.location.hash = '#/session/abc-1/trace?settings=agents';
    expect(readHash()).toEqual({ sessionId: 'abc-1', panel: { kind: 'trace' }, hub: null, settings: 'agents' });
    window.location.hash = '#/workflows/runs?settings';
    expect(readHash()).toEqual({ sessionId: null, panel: null, hub: 'runs', settings: '' });
    window.location.hash = '#/?settings=general';
    expect(readHash()).toEqual({ sessionId: null, panel: null, hub: null, settings: 'general' });
  });
  it('still reads the older #/settings/:tab link, and rewrites it in place', () => {
    window.location.hash = '#/settings/agents';
    const entries = history.length;
    expect(readHash()).toEqual({ sessionId: null, panel: null, hub: null, settings: 'agents' });
    expect(window.location.hash).toBe('#/?settings=agents');
    expect(history.length).toBe(entries);
    window.location.hash = '#/settings';
    expect(readHash().settings).toBe('');
    expect(window.location.hash).toBe('#/?settings');
  });
  it('reads nothing from an unknown or empty fragment', () => {
    window.location.hash = '#auth_code=x';
    expect(readHash()).toEqual({ sessionId: null, panel: null, hub: null, settings: null });
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

describe('writeHash', () => {
  // A move between views is a history entry, so Back returns to the view
  // before; a lens or a Settings tab replaces the entry in place.
  it('pushes a view move and replaces a lens change', () => {
    const push = vi.spyOn(history, 'pushState');
    const replace = vi.spyOn(history, 'replaceState');
    writeHash('a', null, null, null, false);
    expect(replace).toHaveBeenCalledTimes(1);
    writeHash('b', null, null, null, true);
    expect(push).toHaveBeenCalledTimes(1);
    expect(window.location.hash).toBe('#/session/b');
    writeHash('b', { kind: 'trace' }, null, null, false);
    expect(push).toHaveBeenCalledTimes(1);
    expect(replace).toHaveBeenCalledTimes(2);
    expect(window.location.hash).toBe('#/session/b/trace');
    // What the URL already says is not written again.
    writeHash('b', { kind: 'trace' }, null, null, true);
    expect(push).toHaveBeenCalledTimes(1);
    push.mockRestore();
    replace.mockRestore();
  });
  it('writes Settings over the view, the empty view included', () => {
    writeHash('a', null, null, 'agents', true);
    expect(window.location.hash).toBe('#/session/a?settings=agents');
    writeHash(null, null, null, '', true);
    expect(window.location.hash).toBe('#/?settings');
    expect(settingsHash('#/workflows/runs', 'mcp-servers')).toBe('#/workflows/runs?settings=mcp-servers');
  });
});
