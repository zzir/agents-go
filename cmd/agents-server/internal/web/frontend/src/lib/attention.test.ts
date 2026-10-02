// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { attentionMessage, countWaiting, loadNotifyPref, notifyAttention, notifyUnavailable, saveNotifyPref, waitingTitle } from '@/lib/attention';

const g = globalThis as Record<string, unknown>;

// A Notification that records what was shown; `permission` is what the
// browser granted.
function stubNotification(permission: NotificationPermission) {
  const shown: Array<{ title: string; body?: string; tag?: string }> = [];
  class FakeNotification {
    static permission = permission;
    constructor(title: string, opts?: NotificationOptions) { shown.push({ title, body: opts?.body, tag: opts?.tag }); }
  }
  g.Notification = FakeNotification;
  return shown;
}

let hidden = true;
let secure = true;
let savedNotification: unknown;
beforeEach(() => {
  savedNotification = g.Notification;
  localStorage.clear();
  hidden = true;
  secure = true;
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden });
  Object.defineProperty(window, 'isSecureContext', { configurable: true, get: () => secure });
});
afterEach(() => {
  if (savedNotification === undefined) delete g.Notification; else g.Notification = savedNotification;
  vi.restoreAllMocks();
});

describe('waiting count', () => {
  it('puts the count in the title, and takes it out at zero', () => {
    expect(waitingTitle(0)).toBe('agents-go');
    expect(waitingTitle(3)).toBe('(3) waiting · agents-go');
  });

  // The count is the server's statuses, read where the sidebar reads them:
  // the list's rows under what session.status announced since.
  it('counts the rows waiting on a decision, the announced status over the row', () => {
    const rows = [
      { id: 'a', status: 'requires_action' as const },
      { id: 'b', status: 'running' as const },
      { id: 'c', status: 'requires_action' as const },
      { id: 'd' },
    ];
    expect(countWaiting(rows, {})).toBe(2);
    expect(countWaiting(rows, { a: 'idle', b: 'requires_action', d: 'requires_action' })).toBe(3);
    expect(countWaiting(null, { z: 'requires_action' })).toBe(0);
  });
});

describe('attentionMessage', () => {
  it('speaks only when a conversation starts to wait or fails', () => {
    expect(attentionMessage('Deploy', 'running', 'requires_action')).toBe('Deploy needs your approval');
    expect(attentionMessage('Deploy', 'running', 'failed')).toBe('Deploy failed');
    expect(attentionMessage('Deploy', undefined, 'requires_action')).toBe('Deploy needs your approval');
    expect(attentionMessage('', 'idle', 'failed')).toBe('A session failed');
    // Not news: the status it already had, and the ones that ask nothing.
    expect(attentionMessage('Deploy', 'requires_action', 'requires_action')).toBeNull();
    expect(attentionMessage('Deploy', 'requires_action', 'running')).toBeNull();
    expect(attentionMessage('Deploy', 'running', 'idle')).toBeNull();
  });
});

describe('desktop notifications', () => {
  it('shows one only when asked for, granted, and the page is hidden', () => {
    const shown = stubNotification('granted');
    // Not asked for.
    expect(notifyAttention('Deploy needs your approval', 's1')).toBe(false);
    saveNotifyPref(true);
    expect(loadNotifyPref()).toBe(true);
    // The page is the one being looked at.
    hidden = false;
    expect(notifyAttention('Deploy needs your approval', 's1')).toBe(false);
    hidden = true;
    expect(notifyAttention('Deploy needs your approval', 's1')).toBe(true);
    expect(shown).toEqual([{ title: 'agents-go', body: 'Deploy needs your approval', tag: 'session-s1' }]);
    // Turned off again.
    saveNotifyPref(false);
    expect(notifyAttention('Deploy failed', 's1')).toBe(false);
    expect(shown).toHaveLength(1);
  });

  // Chrome for Android has the API and throws from the constructor: that is
  // no notification, not a crash.
  it('is quiet where the platform refuses the constructor', () => {
    class Refusing {
      static permission: NotificationPermission = 'granted';
      constructor() { throw new TypeError('Illegal constructor'); }
    }
    g.Notification = Refusing;
    saveNotifyPref(true);
    expect(notifyAttention('Deploy failed', 's1')).toBe(false);
  });

  it('shows none the browser did not grant', () => {
    const shown = stubNotification('denied');
    saveNotifyPref(true);
    expect(notifyAttention('Deploy failed', 's1')).toBe(false);
    expect(shown).toEqual([]);
  });

  // Browsers hand notifications to secure contexts only: the toggle says so
  // instead of offering something that cannot work.
  it('cannot be offered outside a secure context, or without the API', () => {
    stubNotification('default');
    expect(notifyUnavailable()).toBe('');
    secure = false;
    expect(notifyUnavailable()).toBe('Notifications need https or localhost');
    saveNotifyPref(true);
    expect(notifyAttention('Deploy failed', 's1')).toBe(false);
    secure = true;
    delete g.Notification;
    expect(notifyUnavailable()).toBe('This browser has no notifications');
  });

  it('keeps working when the browser refuses storage', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('denied'); });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('denied'); });
    expect(loadNotifyPref()).toBe(false);
    expect(() => saveNotifyPref(true)).not.toThrow();
  });
});
