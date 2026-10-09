import type { InspectorPanel } from '@/features/chat/ChatView';
import type { HubTab } from '@/features/workflows/WorkflowsHub';

// The URL names the view: a session (with its Inspector lens) or the Workflows
// hub (with its tab; the last session stays in state beside it). settings is the
// dialog over the view (`?settings=<tab>`, invariant 61): '' first tab, null closed.
export interface HashState { sessionId: string | null; panel: InspectorPanel; hub: HubTab | null; settings: string | null }

// splitSettings takes the settings parameter off a fragment. A `#/settings/:tab`
// bookmark reads as the root view with that tab and is rewritten in the current form.
function splitSettings(h: string): { view: string; settings: string | null } {
  const legacy = /^#\/settings(?:\/([a-zA-Z0-9_-]+))?$/.exec(h);
  if (legacy) {
    const settings = legacy[1] || '';
    window.history.replaceState(null, '', settingsHash('', settings));
    return { view: '', settings };
  }
  const m = /^(.*?)\?settings(?:=([a-zA-Z0-9_-]*))?$/.exec(h);
  if (!m) return { view: h, settings: null };
  return { view: m[1] === '#/' ? '' : m[1], settings: m[2] || '' };
}

export function readHash(): HashState {
  const { view: h, settings } = splitSettings(window.location.hash);
  const hub = /^#\/workflows(?:\/(definitions|triggers|runs))?$/.exec(h);
  if (hub) return { sessionId: null, panel: null, hub: (hub[1] as HubTab) || 'definitions', settings };
  const m = /^#\/session\/([a-zA-Z0-9_-]+)(?:\/(trace|tasks|context|task\/([a-zA-Z0-9_-]+)))?$/.exec(h);
  if (!m) return { sessionId: null, panel: null, hub: null, settings };
  let panel: InspectorPanel = null;
  if (m[2] === 'trace') panel = { kind: 'trace' };
  else if (m[2] === 'tasks') panel = { kind: 'tasks' };
  else if (m[2] === 'context') panel = { kind: 'context' };
  else if (m[3]) panel = { kind: 'task', taskId: m[3] };
  return { sessionId: m[1], panel, hub: null, settings };
}

// viewHash is the fragment naming a view, '' for the empty one.
export function viewHash(sessionId: string | null, panel: InspectorPanel, hub: HubTab | null): string {
  if (hub) return `#/workflows/${hub}`;
  if (!sessionId) return '';
  let next = `#/session/${sessionId}`;
  if (panel?.kind === 'trace') next += '/trace';
  else if (panel?.kind === 'tasks') next += '/tasks';
  else if (panel?.kind === 'context') next += '/context';
  else if (panel?.kind === 'task') next += `/task/${panel.taskId}`;
  return next;
}

// settingsHash is a view's fragment with the Settings dialog open on tab.
export function settingsHash(view: string, tab: string): string {
  return (view || '#/') + '?settings' + (tab ? '=' + tab : '');
}

// currentViewHash is the view part of the fragment on screen, settings aside.
export function currentViewHash(): string {
  return splitSettings(window.location.hash).view;
}

// writeHash puts the state in the URL: a new history entry when push (a view
// move, Settings opening), else the entry in place; nothing when the URL
// already says it.
export function writeHash(sessionId: string | null, panel: InspectorPanel, hub: HubTab | null, settings: string | null, push: boolean) {
  const view = viewHash(sessionId, panel, hub);
  const next = settings == null ? view : settingsHash(view, settings);
  if (window.location.hash === next) return;
  const url = next || window.location.pathname;
  if (push) window.history.pushState(null, '', url);
  else window.history.replaceState(null, '', url);
}

// consumeAuthFragment strips a login-callback fragment (#auth_code= / #auth_error=)
// before the hash router parses it and returns what it carried; stripping at once
// keeps the one-time code out of history.
export function consumeAuthFragment(): { code?: string; error?: string } {
  const h = window.location.hash;
  if (h.startsWith('#auth_code=')) {
    history.replaceState(null, '', window.location.pathname);
    return { code: decodeURIComponent(h.slice('#auth_code='.length)) };
  }
  if (h.startsWith('#auth_error=')) {
    history.replaceState(null, '', window.location.pathname);
    return { error: decodeURIComponent(h.slice('#auth_error='.length)) };
  }
  return {};
}

// The deep link a sign-in started from: the OAuth round trip replaces the
// fragment, so the view is stashed before leaving and put back after the exchange.
const AUTH_RETURN_KEY = 'auth_return_hash';

export function stashReturnHash(): void {
  const h = window.location.hash;
  if (h) sessionStorage.setItem(AUTH_RETURN_KEY, h);
  else sessionStorage.removeItem(AUTH_RETURN_KEY);
}

export function restoreReturnHash(): void {
  const h = sessionStorage.getItem(AUTH_RETURN_KEY);
  sessionStorage.removeItem(AUTH_RETURN_KEY);
  if (h) window.location.hash = h;
}
