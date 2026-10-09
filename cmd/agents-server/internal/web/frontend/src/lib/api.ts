import type { components } from '@/lib/apiTypes.gen';
import type { InjectQueue } from '@/lib/protocol';

// The generated OpenAPI schemas — swagger.yaml is CI-checked fresh, and
// `npm run gen:api` keeps apiTypes.gen.ts matching it (CI checks that too).
type S = components['schemas'];
export type ApiSchemas = S;

const BASE = '/api/v1';

// TOKEN_KEY names the credential in both storages: localStorage for an OAuth
// session (tabs share it), sessionStorage for a token-mode login (gone with the tab).
export const TOKEN_KEY = 'auth_token';

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) || sessionStorage.getItem(TOKEN_KEY) || '';
}

export function setToken(t: string, opts: { persist: boolean }): void {
  clearToken();
  (opts.persist ? localStorage : sessionStorage).setItem(TOKEN_KEY, t);
}

export function clearToken(): void {
  localStorage.removeItem(TOKEN_KEY);
  sessionStorage.removeItem(TOKEN_KEY);
}

async function request<T = unknown>(path: string, opts: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json', ...(opts.headers as Record<string, string>) };
  const t = getToken();
  if (t) headers['Authorization'] = `Bearer ${t}`;
  const res = await fetch(`${BASE}${path}`, { ...opts, headers });
  if (res.status === 401) {
    clearToken();
    window.dispatchEvent(new Event('auth:logout'));
    throw new Error('unauthorized');
  }
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    // Errors arrive as {"error": {"code", "message"}}.
    const message = body.error?.message || (typeof body.error === 'string' ? body.error : '') || res.statusText;
    const err = new Error(message) as Error & { status?: number };
    err.status = res.status;
    throw err;
  }
  if (res.status === 204) return null as T;
  return res.json();
}

export async function login(token: string): Promise<boolean> {
  const res = await fetch(`${BASE}/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ token }),
  });
  if (!res.ok) throw new Error('invalid token');
  setToken(token, { persist: false });
  return true;
}

// Probes the stored credential. Only a refusal (401/403) clears the token; any
// other failure rejects with its status so the caller can retry rather than sign out.
export async function checkAuth(): Promise<boolean> {
  const t = getToken();
  if (!t) return false;
  const res = await fetch(`${BASE}/auth/check`, {
    headers: { 'Authorization': `Bearer ${t}` },
  });
  if (res.status === 401 || res.status === 403) { clearToken(); return false; }
  if (!res.ok) throw httpError(res);
  return true;
}

function httpError(res: Response): Error & { status: number } {
  const err = new Error(`HTTP ${res.status}`) as Error & { status: number };
  err.status = res.status;
  return err;
}

export type AuthConfig = S['protocol.AuthConfig'];
export type AuthUser = S['protocol.UserInfo'];

// How to authenticate — auth-exempt, called by the login page before any
// credential exists.
export async function authConfig(): Promise<AuthConfig> {
  const res = await fetch(`${BASE}/auth/config`);
  if (!res.ok) throw httpError(res);
  return res.json();
}

// Trade the OAuth callback's one-time #auth_code for the session token and
// store it. The token plaintext exists only in this response.
export async function exchangeCode(code: string): Promise<AuthUser> {
  const res = await fetch(`${BASE}/auth/exchange`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ code }),
  });
  if (!res.ok) throw httpError(res);
  const body: S['protocol.AuthSession'] = await res.json();
  setToken(body.token || '', { persist: true });
  return body.user || {};
}

// Revokes the session server-side (no-op in token mode), forgets the local copy
// and reloads so no in-memory state survives; a dead server never blocks sign-out.
export async function logout(): Promise<void> {
  try {
    await request('/auth/logout', { method: 'POST' });
  } catch { /* the local clear below is the part that must happen */ }
  clearToken();
  window.location.reload();
}

interface CrudMethods<T> {
  list: () => Promise<T[]>;
  create: (data: unknown) => Promise<T>;
  get: (id: string | number) => Promise<T>;
  update: (id: string | number, data: unknown) => Promise<T>;
  delete: (id: string | number) => Promise<null>;
}

function crud<T>(base: string): CrudMethods<T> {
  return {
    list: () => request<T[]>(base),
    create: (data: unknown) => request<T>(base, { method: 'POST', body: JSON.stringify(data) }),
    get: (id: string | number) => request<T>(`${base}/${id}`),
    update: (id: string | number, data: unknown) => request<T>(`${base}/${id}`, { method: 'PUT', body: JSON.stringify(data) }),
    delete: (id: string | number) => request<null>(`${base}/${id}`, { method: 'DELETE' }),
  };
}

// Moves a scoped row between private and global: promote is the admin's act,
// demote the admin's or the owner's; 400/409 report non-global references or a
// name collision.
function setScope(base: string) {
  return (id: string | number, scope: 'global' | 'private') =>
    request<null>(`${base}/${id}/scope`, { method: 'POST', body: JSON.stringify({ scope }) });
}

// Admin: transfers a scoped row to another account; scope stays put. 409 on a
// name collision in the target owner's namespace.
function setOwner(base: string) {
  return (id: string | number, userId: string) =>
    request<null>(`${base}/${id}/owner`, { method: 'PUT', body: JSON.stringify({ user_id: userId }) });
}

export const api = {
  auth: {
    me: () => request<AuthUser>('/auth/me'),
    // Admin: every account; role changes and disabling (never one's own
    // account, never the last enabled admin); signing one out everywhere.
    users: {
      list: () => request<S['store.User'][]>('/auth/users'),
      patch: (id: string, patch: { role?: 'admin' | 'member'; disabled?: boolean }) =>
        request<null>(`/auth/users/${encodeURIComponent(id)}`, { method: 'PATCH', body: JSON.stringify(patch) }),
      revokeTokens: (id: string) =>
        request<null>(`/auth/users/${encodeURIComponent(id)}/tokens`, { method: 'DELETE' }),
    },
    // The id→person directory any member reads to label row owners.
    userLabels: () => request<{ id: string; name?: string; email: string }[]>('/auth/user-labels'),
    // Admin: the audit log, newest first; `before` (an event id) pages older.
    audit: (limit = 50, before?: string) =>
      request<S['store.AuditEvent'][]>(`/auth/audit?limit=${limit}${before ? `&before=${encodeURIComponent(before)}` : ''}`),
    // Personal access tokens (OAuth mode): the create response is the only
    // place a token's plaintext appears.
    pats: {
      list: () => request<S['protocol.PatView'][]>('/auth/tokens'),
      create: (name: string, expiresInDays: number) =>
        request<S['protocol.PatCreated']>('/auth/tokens', { method: 'POST', body: JSON.stringify({ name, expires_in_days: expiresInDays }) }),
      delete: (id: string) => request<null>(`/auth/tokens/${encodeURIComponent(id)}`, { method: 'DELETE' }),
    },
  },
  sessions: {
    ...crud<S['store.Session']>('/sessions'),
    // One page of the caller's sessions (lib/sessionPages.ts): limit unpinned
    // rows after `before`, matching q; no options is the whole list.
    list: (opts: { limit?: number; before?: string; q?: string } = {}) => {
      const p = new URLSearchParams();
      if (opts.limit) p.set('limit', String(opts.limit));
      if (opts.before) p.set('before', opts.before);
      if (opts.q) p.set('q', opts.q);
      const qs = p.toString();
      return request<S['store.Session'][]>('/sessions' + (qs ? '?' + qs : ''));
    },
    // Admin: every owner's sessions — existence and recency, never content —
    // and reassigning one (its task sessions follow) to another account.
    listAll: () => request<S['store.Session'][]>('/sessions?all=true'),
    setOwner: (id: string, userId: string) =>
      request<S['store.Session']>(`/sessions/${id}/owner`, { method: 'PUT', body: JSON.stringify({ user_id: userId }) }),
    // Both optional: an unnamed conversation is "New Session" until its first
    // message titles it.
    create: (body: { name?: string; agent_config_id?: string } = {}) => request('/sessions', { method: 'POST', body: JSON.stringify(body) }),
    update: (id: string | number, name: string) => request(`/sessions/${id}`, { method: 'PATCH', body: JSON.stringify({ name }) }),
    // The session's entries, all of them, oldest first.
    messages: (id: string | number) => request(`/sessions/${id}/messages`),
    // The session's background work — tasks and workflow executions — newest first.
    tasks: (id: string | number) => request(`/sessions/${id}/tasks`),
    // summary leaves the payload fields (model request and reply, tool arguments
    // and result) out of each row, marked payload_omitted; traceSpan fetches one
    // whole — invariant 22.
    traces: (id: string | number, opts?: { summary?: boolean }) =>
      request(`/sessions/${id}/traces` + (opts?.summary ? '?summary=true' : '')),
    traceSpan: (id: string | number, spanId: string) => request(`/sessions/${id}/traces/${encodeURIComponent(spanId)}`),
    // What the active branch occupies of the context window, recomputed per
    // call from the entries; no live event carries it, so the panel refetches
    // when a run ends.
    context: (id: string | number) => request(`/sessions/${id}/context`),
    // Forces one compaction pass now; {compacted:false} means nothing to fold.
    compact: (id: string | number) => request(`/sessions/${id}/compact`, { method: 'POST' }),
    // The session's own memory: what the model wrote for itself, keys and
    // sizes, then one key in full.
    memory: (id: string | number) => request<S['handler.SessionMemoryInfo'][]>(`/sessions/${id}/memory`),
    memoryKey: (id: string | number, key: string) => request<S['store.Memory']>(`/sessions/${id}/memory/${key.split('/').map(encodeURIComponent).join('/')}`),
    approvals: (id: string | number) => request(`/sessions/${id}/approvals`),
    // Approves every call of the session's own pause at once (the per-call
    // kinds excepted, see PER_CALL_APPROVALS) and resumes the run once.
    approveAll: (id: string | number) => request<{ run_id: string; approved: number }>(`/sessions/${id}/approvals/approve-all`, { method: 'POST' }),
    // Moves the session's active branch to an entry. Append-only: the
    // abandoned attempt stays recorded and can be switched back to.
    branch: (id: string | number, entryId: string) => request<{ leaf: string; previous_leaf: string }>(`/sessions/${id}/branch`, { method: 'POST', body: JSON.stringify({ entry_id: entryId }) }),
    fork: (id: string | number, messageId?: string, opts?: { exclusive?: boolean; label?: string }) => request<S['store.Session']>(`/sessions/${id}/fork`, { method: 'POST', body: JSON.stringify({ ...(messageId ? { message_id: messageId } : {}), ...opts }) }),
    pin: (id: string | number, pinned: boolean) => request(`/sessions/${id}`, { method: 'PATCH', body: JSON.stringify({ pinned }) }),
  },
  attachments: {
    config: () => request('/attachments/config'),
    // Multipart, not JSON: hand-rolled fetch because request() stamps a JSON
    // Content-Type the multipart boundary must replace.
    upload: async (blob: Blob, name: string) => {
      const form = new FormData();
      form.append('file', blob, name);
      const headers: Record<string, string> = {};
      const t = getToken();
      if (t) headers['Authorization'] = `Bearer ${t}`;
      const res = await fetch(`${BASE}/attachments`, { method: 'POST', body: form, headers });
      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        throw new Error(body.error?.message || res.statusText);
      }
      return res.json();
    },
    remove: (id: string) => request(`/attachments/${id}`, { method: 'DELETE' }),
    // The storage section saves/tests/clears as ONE group: per-key writes are
    // refused server-side, a value validating only with the siblings it is stored with.
    storageSave: (body: Record<string, unknown>) => request('/attachments/storage', { method: 'PUT', body: JSON.stringify(body) }),
    storageTest: (body: Record<string, unknown>) => request('/attachments/storage/test', { method: 'POST', body: JSON.stringify(body) }),
  },
  agents: {
    ...crud<S['store.AgentConfig']>('/agents'),
    setScope: setScope('/agents'),
    setOwner: setOwner('/agents'),
    // The agent's CURRENT tool surface as schema-only definitions, each with
    // its source and read-only flag: the Replay dialog's tool picker and the
    // editor's approval list.
    tools: (id: string | number) => request(`/agents/${id}/tools`),
  },
  mcpServers: {
    ...crud<S['handler.mcpServerListItem']>('/mcp-servers'),
    setScope: setScope('/mcp-servers'),
    setOwner: setOwner('/mcp-servers'),
    connect: (id: string | number) => request(`/mcp-servers/${id}/connect`, { method: 'POST' }),
    clearOAuth: (id: string | number) => request(`/mcp-servers/${id}/oauth-token`, { method: 'DELETE' }),
    tools: (id: string | number) => request(`/mcp-servers/${id}/tools`),
  },
  memories: crud<S['store.Memory']>('/memories'),
  playground: {
    generate: (body: {
      agent_config_id: string;
      model?: string;
      system_instructions?: string;
      input_items: unknown[];
      model_settings?: Record<string, unknown>;
      tools?: unknown[];
      output_schema?: { name?: string; schema: Record<string, unknown>; strict?: boolean };
    }) =>
      request('/playground/generate', { method: 'POST', body: JSON.stringify(body) }),
    // The SSE variant: onDelta/onReasoning fire per chunk, the promise resolves
    // with the terminal `done` payload; `signal` aborts the model call server-side.
    generateStream: async (
      body: Record<string, unknown>,
      handlers: { onDelta?: (text: string) => void; onReasoning?: (text: string) => void },
      signal?: AbortSignal,
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    ): Promise<any> => {
      const headers: Record<string, string> = { 'Content-Type': 'application/json' };
      const t = getToken();
      if (t) headers['Authorization'] = `Bearer ${t}`;
      const res = await fetch(`${BASE}/playground/generate`, {
        method: 'POST', headers, body: JSON.stringify({ ...body, stream: true }), signal,
      });
      if (res.status === 401) {
        clearToken();
        window.dispatchEvent(new Event('auth:logout'));
        throw new Error('unauthorized');
      }
      if (!res.ok || !res.body) {
        const b = await res.json().catch(() => ({} as { error?: { message?: string } }));
        throw new Error(b.error?.message || res.statusText);
      }
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buf = '';
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      let done: any = null;
      for (;;) {
        const { value, done: eof } = await reader.read();
        if (eof) break;
        buf += decoder.decode(value, { stream: true });
        for (;;) {
          const sep = buf.indexOf('\n\n');
          if (sep < 0) break;
          const frame = buf.slice(0, sep);
          buf = buf.slice(sep + 2);
          let event = '';
          let data = '';
          for (const line of frame.split('\n')) {
            if (line.startsWith('event: ')) event = line.slice(7).trim();
            else if (line.startsWith('data: ')) data += line.slice(6);
          }
          if (!event || !data) continue;
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          let parsed: any;
          try { parsed = JSON.parse(data); } catch { continue; }
          if (event === 'delta') handlers.onDelta?.(String(parsed.text ?? ''));
          else if (event === 'reasoning') handlers.onReasoning?.(String(parsed.text ?? ''));
          else if (event === 'done') done = parsed;
          else if (event === 'error') throw new Error(String(parsed.message || 'model call failed'));
        }
      }
      if (!done) throw new Error('stream ended unexpectedly');
      return done;
    },
  },
  // What the command line decided: read-only, fixed for the process.
  server: () => request('/server'),
  settings: {
    list: () => request('/settings'),
    // The registry the panel renders from — kinds, defaults, labels. A new
    // global setting is a Go entry; nothing here enumerates keys.
    defs: () => request('/setting-defs'),
    get: (key: string) => request(`/settings/${key}`),
    set: (key: string, value: unknown) => request(`/settings/${key}`, { method: 'PUT', body: JSON.stringify({ value }) }),
    delete: (key: string) => request(`/settings/${key}`, { method: 'DELETE' }),
  },
  skills: {
    ...crud<S['store.Skill']>('/skills'),
    // Per-row scope flips are for workbench-authored skills only; an imported
    // repo flips as one group via setRepoScope.
    setScope: setScope('/skills'),
    setRepoScope: (repo: string, scope: 'global' | 'private', ownerId?: string) =>
      request<null>('/skill-repos/scope', { method: 'POST', body: JSON.stringify({ repo, scope, ...(ownerId ? { owner_id: ownerId } : {}) }) }),
    setOwner: setOwner('/skills'),
    // Import walks a GitHub repo (or one raw SKILL.md) and upserts. ownerId
    // names WHICH group a sync refreshes (an admin syncing another's repo);
    // omitted, the caller's own.
    import: (url: string, ownerId?: string) =>
      request('/skill-imports', { method: 'POST', body: JSON.stringify({ url, ...(ownerId ? { owner_id: ownerId } : {}) }) }),
  },
  guardrails: crud<S['store.Guardrail']>('/guardrails'),
  providers: {
    ...crud<S['store.Provider']>('/providers'),
    setScope: setScope('/providers'),
    setOwner: setOwner('/providers'),
    // The provider's live model list (cached ten minutes server-side).
    models: (id: string) => request<S['providers.ModelInfo'][]>(`/providers/${id}/models`),
    // Lists the models with the stored key; a model name is looked up in the list.
    test: (id: string, model?: string) => request<S['handler.providerTestResp']>(`/providers/${id}/test`, { method: 'POST', body: JSON.stringify(model ? { model } : {}) }),
  },
  // Projects carry a name, a template and an environment; the target is fixed at
  // creation. A delete refuses (409) while sessions bind one, else destroys the tree.
  projects: {
    list: () => request<S['store.Project'][]>('/projects'),
    // Admin: every owner's projects, storage hints included.
    listAll: () => request<S['store.Project'][]>('/projects?all=true'),
    create: (data: unknown) => request<S['handler.projectDetail']>('/projects', { method: 'POST', body: JSON.stringify(data) }),
    // The one call that returns an environment, and only to the owner.
    get: (id: string) => request<S['handler.projectDetail']>(`/projects/${id}`),
    update: (id: string, data: unknown) =>
      request<S['handler.projectDetail']>(`/projects/${id}`, { method: 'PUT', body: JSON.stringify(data) }),
    // Answers 200 whenever the row is gone; storage_error names storage that
    // could not be reclaimed with it — a warning, not a failed delete.
    delete: (id: string) => request<{ deleted: boolean; storage_error?: string }>(`/projects/${id}`, { method: 'DELETE' }),
    // Creates the container up front, or discards and recreates it, synchronously
    // (an image pull's worth of time); sessionId is where the rebuilt note is left.
    rebuildContainer: (id: string, sessionId?: string) => request<null>(`/projects/${id}/sandbox/rebuild`, { method: 'POST', body: sessionId ? JSON.stringify({ session_id: sessionId }) : undefined }),
    // The project's compute: what it is doing, and starting/stopping it by
    // hand rather than leaving both to the next run and the idle timer.
    sandboxStatus: (id: string) => request<{ state: string }>(`/projects/${id}/sandbox`),
    sandboxHost: (id: string) => request<{ sandbox_id: string; domain: string }>(`/projects/${id}/host`),
    sandboxStart: (id: string) => request<null>(`/projects/${id}/sandbox/start`, { method: 'POST' }),
    sandboxStop: (id: string) => request<{ stopped: boolean }>(`/projects/${id}/sandbox/stop`, { method: 'POST' }),
    // The working tree as a tar: a DOWNLOAD, not JSON, so it bypasses request()
    // — the bearer token must ride on the fetch, and the body is a stream.
    exportTar: async (id: string, name: string): Promise<void> => {
      const headers: Record<string, string> = {};
      const t = getToken();
      if (t) headers['Authorization'] = `Bearer ${t}`;
      const res = await fetch(`${BASE}/projects/${id}/export`, { headers });
      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        throw new Error(body.error?.message || res.statusText);
      }
      const url = URL.createObjectURL(await res.blob());
      const a = document.createElement('a');
      a.href = url;
      a.download = `${name || 'project'}.tar`;
      // Appended to the DOM (older Firefox ignores a detached anchor); the URL
      // is revoked on a delay, since revoking right after click() can cancel
      // Safari's download.
      a.style.display = 'none';
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 60_000);
    },
  },
  workflows: {
    ...crud<S['store.Workflow']>('/workflows'),
    setScope: setScope('/workflows'),
    setOwner: setOwner('/workflows'),
    // A person's own run of a workflow: the brief, for the session the result
    // comes back to; project_id binds a still-unbound session first — invariant 27.
    run: (id: string | number, body: { session_id: string; input: string; project_id?: string }) =>
      request(`/workflows/${id}/runs`, { method: 'POST', body: JSON.stringify(body) }),
  },
  // Triggers start a workflow without a conversation asking: on a cron
  // schedule, or on a signed webhook call. fire runs one by hand.
  triggers: {
    ...crud<S['handler.TriggerView']>('/triggers'),
    listFor: (workflowId: string) => request(`/triggers?workflow_id=${encodeURIComponent(workflowId)}`),
    fire: (id: string | number, payload = '') => request(`/triggers/${id}/fire`, { method: 'POST', body: JSON.stringify({ payload }) }),
    rotateSecret: (id: string | number) => request(`/triggers/${id}/rotate-secret`, { method: 'POST' }),
  },
  providerTypes: {
    list: () => request('/provider-types'),
  },
  runs: {
    // Queues input on a live run; 409 when the run is paused, starting or over.
    inject: (runId: string, body: { queue: InjectQueue; input: string }) =>
      request<{ run_id: string; queue: string }>(`/runs/${runId}/inject`, { method: 'POST', body: JSON.stringify(body) }),
  },
  tasks: {
    // One page across every session, newest first ({items, total}), for the
    // hub's Runs view: kind narrows, live keeps working / input_required rows,
    // limit/offset page.
    list: (q: { kind?: string; live?: boolean; limit?: number; offset?: number } = {}) => {
      const p = new URLSearchParams();
      if (q.kind) p.set('kind', q.kind);
      if (q.live) p.set('live', 'true');
      if (q.limit) p.set('limit', String(q.limit));
      if (q.offset) p.set('offset', String(q.offset));
      const qs = p.toString();
      return request(`/tasks${qs ? '?' + qs : ''}`);
    },
    stop: (id: string | number, graceful = false) => request(`/tasks/${id}/stop`, { method: 'POST', body: JSON.stringify({ graceful }) }),
    retry: (id: string | number) => request(`/tasks/${id}/retry`, { method: 'POST' }),
    // Hides a finished task from the chat strip; the panel keeps it, a retry
    // brings it back.
    dismiss: (id: string | number) => request(`/tasks/${id}/dismiss`, { method: 'POST' }),
  },
  sandboxes: {
    ...crud<S['store.Sandbox']>('/sandboxes'),
    test: (id: string | number) => request(`/sandboxes/${id}/test`, { method: 'POST' }),
  },
  // The OAuth flow belongs to the endpoint: the token is the provider's
  // credential, shared by every agent pointed at it.
  chatgpt: {
    login: (providerId: string | number) => request(`/providers/${providerId}/chatgpt/login`, { method: 'POST' }),
    // Redeem the callback URL the user pastes after authorizing; the loopback
    // redirect has no listener, so this is how a login completes.
    complete: (providerId: string | number, redirectUrl: string) =>
      request(`/providers/${providerId}/chatgpt/complete`, { method: 'POST', body: JSON.stringify({ redirect_url: redirectUrl }) }),
    logout: (providerId: string | number) => request(`/providers/${providerId}/chatgpt/logout`, { method: 'POST' }),
  },
};
