import type { InspectorPanel } from '@/features/chat/ChatView';
import type { HubTab } from '@/features/workflows/WorkflowsHub';

// The URL names the view: a conversation (with the open Inspector lens), or
// the Workflows hub (with its tab). The hub is a place of its own, so the
// conversation last open is kept beside it in state, not in the URL.
// settings is a one-shot deep link to the Settings dialog, consumed on open:
// '' names its first tab, a name that tab, null means no such fragment.
export interface HashState { sessionId: string | null; panel: InspectorPanel; hub: HubTab | null; settings: string | null }

export function readHash(): HashState {
  const h = window.location.hash;
  const set = /^#\/settings(?:\/([a-zA-Z0-9_-]+))?$/.exec(h);
  if (set) return { sessionId: null, panel: null, hub: null, settings: set[1] || '' };
  const hub = /^#\/workflows(?:\/(definitions|triggers|runs))?$/.exec(h);
  if (hub) return { sessionId: null, panel: null, hub: (hub[1] as HubTab) || 'definitions', settings: null };
  const m = /^#\/session\/([a-zA-Z0-9_-]+)(?:\/(trace|tasks|context|task\/([a-zA-Z0-9_-]+)))?$/.exec(h);
  if (!m) return { sessionId: null, panel: null, hub: null, settings: null };
  let panel: InspectorPanel = null;
  if (m[2] === 'trace') panel = { kind: 'trace' };
  else if (m[2] === 'tasks') panel = { kind: 'tasks' };
  else if (m[2] === 'context') panel = { kind: 'context' };
  else if (m[3]) panel = { kind: 'task', taskId: m[3] };
  return { sessionId: m[1], panel, hub: null, settings: null };
}

export function writeHash(sessionId: string | null, panel: InspectorPanel, hub: HubTab | null) {
  let next = '';
  if (hub) {
    next = `#/workflows/${hub}`;
  } else if (sessionId) {
    next = `#/session/${sessionId}`;
    if (panel?.kind === 'trace') next += '/trace';
    else if (panel?.kind === 'tasks') next += '/tasks';
    else if (panel?.kind === 'context') next += '/context';
    else if (panel?.kind === 'task') next += `/task/${panel.taskId}`;
  }
  if (window.location.hash !== next) {
    window.history.replaceState(null, '', next || window.location.pathname);
  }
}

// consumeAuthFragment strips a login-callback fragment (#auth_code= /
// #auth_error=) from the URL before the hash router ever parses it, and
// returns what it carried. Stripping immediately keeps the one-time code out
// of the session history the user can arrow back through.
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
// fragment with the callback's, so the view is stashed before leaving and
// put back once the code has been exchanged.
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
