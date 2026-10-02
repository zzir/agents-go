// What tells a person a conversation needs them when they are not looking at
// it: the page title's count, a spoken line, an opt-in desktop notification.
// All of it renders the status the server derived (session.status, the list's
// rows) — nothing here works a status out.

import type { SessionStatus } from '@/lib/protocol';

const APP_TITLE = 'agents-go';

/** The page title for n conversations waiting on a decision. */
export function waitingTitle(n: number): string {
  return n > 0 ? `(${n}) waiting · ${APP_TITLE}` : APP_TITLE;
}

/** How many conversations wait on a decision: each row's status, under the one
 *  session.status announced since the list was read. */
export function countWaiting(rows: Array<{ id: string; status?: SessionStatus }> | null, announced: Record<string, SessionStatus>): number {
  return (rows || []).filter(r => (announced[r.id] ?? r.status) === 'requires_action').length;
}

/** What to say when a conversation's status becomes `next`, or null when the
 *  change asks nothing of the person: only entering requires_action or failed
 *  does, and a repeat of the status it already had is not entering it. */
export function attentionMessage(name: string, prev: SessionStatus | undefined, next: SessionStatus): string | null {
  if (prev === next) return null;
  const who = name || 'A session';
  if (next === 'requires_action') return `${who} needs your approval`;
  if (next === 'failed') return `${who} failed`;
  return null;
}

const NOTIFY_KEY = 'attention.notify';

/** Whether the person asked for desktop notifications in this browser. */
export function loadNotifyPref(): boolean {
  try { return localStorage.getItem(NOTIFY_KEY) === '1'; } catch { return false; }
}

export function saveNotifyPref(on: boolean): void {
  try {
    if (on) localStorage.setItem(NOTIFY_KEY, '1');
    else localStorage.removeItem(NOTIFY_KEY);
  } catch { /* a browser that refuses storage keeps the preference for the page */ }
}

/** Why notifications cannot be offered here, or '' when they can: browsers
 *  give them to secure contexts only. */
export function notifyUnavailable(): string {
  if (typeof Notification === 'undefined') return 'This browser has no notifications';
  if (!window.isSecureContext) return 'Notifications need https or localhost';
  return '';
}

/** Shows a desktop notification, only when the person asked for them, the
 *  browser granted them, and the page is not the one being looked at. One per
 *  conversation: a newer one for the same session replaces the older. */
export function notifyAttention(message: string, sessionId: string): boolean {
  if (!loadNotifyPref() || notifyUnavailable() || Notification.permission !== 'granted' || !document.hidden) return false;
  new Notification(APP_TITLE, { body: message, tag: 'session-' + sessionId });
  return true;
}
