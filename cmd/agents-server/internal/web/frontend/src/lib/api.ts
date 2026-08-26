import type { components } from '@/lib/apiTypes.gen';

// The generated OpenAPI schemas — swagger.yaml is CI-checked fresh, and
// `npm run gen:api` keeps apiTypes.gen.ts matching it (CI checks that too).
type S = components['schemas'];
export type ApiSchemas = S;

const BASE = '/api/v1';

// TOKEN_KEY names the credential in both storages: localStorage for an
// OAuth session (new tabs share the 30-day session), sessionStorage for the
// token-mode login (gone with the tab).
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

// Probe the stored credential. Only a refusal (401/403) means the token is
// bad and gets it cleared; any other failure (429, a proxy's 502 mid-restart)
// rejects with the status so the caller can retry rather than sign out.
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

// Revoke the current session server-side (no-op in token mode), forget the
// local copy, then reload: the socket and every piece of in-memory state
// belong to the signed-out user, and a fresh document is the one sure way
// to own none of it when the next account signs in. Always resolves — a
// dead server must not block sign-out.
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
// demote the admin's or the owner's (the row returns to its author); 400/409
// report non-global references or a name collision in the target scope.
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
    // Admin: every owner's sessions — existence and recency, never content —
    // and reassigning one (its task sessions follow) to another account.
    listAll: () => request<S['store.Session'][]>('/sessions?all=true'),
    setOwner: (id: string, userId: string) =>
      request<S['store.Session']>(`/sessions/${id}/owner`, { method: 'PUT', body: JSON.stringify({ user_id: userId }) }),
    create: (name: string, agentConfigId?: string) => request('/sessions', { method: 'POST', body: JSON.stringify({ name, ...(agentConfigId ? { agent_config_id: agentConfigId } : {}) }) }),
    update: (id: string | number, name: string) => request(`/sessions/${id}`, { method: 'PATCH', body: JSON.stringify({ name }) }),
    // limit/beforeId page BACKWARDS: the newest `limit` entries first, then
    // older pages keyed on the smallest id received. A cursor rather than an
    // offset because entries keep arriving — an offset shifts under a
    // concurrent append and silently repeats or skips a row.
    messages: (id: string | number, opts?: { limit?: number; beforeId?: string }) => {
      const q = new URLSearchParams();
      if (opts?.limit) q.set('limit', String(opts.limit));
      if (opts?.beforeId) q.set('before_id', opts.beforeId);
      const qs = q.toString();
      return request(`/sessions/${id}/messages` + (qs ? '?' + qs : ''));
    },
    // The session's background work — tasks and workflow executions — newest first.
    tasks: (id: string | number) => request(`/sessions/${id}/tasks`),
    // summary leaves the payload fields (the model request and reply, a
    // tool's arguments and result — nearly all of a session's trace bytes)
    // out of each row, marking it payload_omitted; traceSpan fetches one span
    // whole when a row is opened.
    traces: (id: string | number, opts?: { summary?: boolean }) =>
      request(`/sessions/${id}/traces` + (opts?.summary ? '?summary=true' : '')),
    traceSpan: (id: string | number, spanId: string) => request(`/sessions/${id}/traces/${encodeURIComponent(spanId)}`),
    // Every run that left entries, oldest first, with the user text it started
    // from and whether it is on the active branch — the trace panel's labels
    // for runs whose exchange the paged timeline has not loaded.
    runs: (id: string | number) => request(`/sessions/${id}/runs`),
    // What the session's active branch occupies of the model's context window.
    // Recomputed per call from the entries — there is no live event for it, so
    // the panel refetches when a run ends.
    context: (id: string | number) => request(`/sessions/${id}/context`),
    // Forces one compaction pass now; {compacted:false} means nothing to fold.
    compact: (id: string | number) => request(`/sessions/${id}/compact`, { method: 'POST' }),
    approvals: (id: string | number) => request(`/sessions/${id}/approvals`),
    // Moves the session's active branch to an entry. Append-only: the
    // abandoned attempt stays recorded and can be switched back to.
    branch: (id: string | number, entryId: string) => request(`/sessions/${id}/branch`, { method: 'POST', body: JSON.stringify({ entry_id: entryId }) }),
    fork: (id: string | number, messageId?: string, opts?: { exclusive?: boolean; label?: string }) => request<S['store.Session']>(`/sessions/${id}/fork`, { method: 'POST', body: JSON.stringify({ ...(messageId ? { message_id: messageId } : {}), ...opts }) }),
    pin: (id: string | number, pinned: boolean) => request(`/sessions/${id}`, { method: 'PATCH', body: JSON.stringify({ pinned }) }),
  },
  agents: {
    ...crud<S['store.AgentConfig']>('/agents'),
    setScope: setScope('/agents'),
    setOwner: setOwner('/agents'),
    // The agent's CURRENT tool surface as schema-only definitions — what the
    // bridge would hand the model right now (sandbox tools excluded). Backs
    // the Replay dialog's tool picker.
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
    // generateStream is the SSE variant: onDelta/onReasoning fire per text
    // chunk, the returned promise resolves with the terminal `done` payload
    // (output, usage, duration_ms, ttft_ms). Abort via `signal` cancels the
    // model call server-side (the request context tears it down).
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
    // Import walks a GitHub repo (or fetches one raw SKILL.md) and upserts.
    // ownerId names WHICH group a sync refreshes — an admin syncing somebody
    // else's published repository; omitted, it is the caller's own group.
    import: (url: string, ownerId?: string) =>
      request('/skill-imports', { method: 'POST', body: JSON.stringify({ url, ...(ownerId ? { owner_id: ownerId } : {}) }) }),
  },
  guardrails: {
    ...crud<S['store.Guardrail']>('/guardrails'),
    list: () => request('/guardrails'),
  },
  providers: {
    ...crud<S['store.Provider']>('/providers'),
    setScope: setScope('/providers'),
    setOwner: setOwner('/providers'),
  },
  // Projects have no get/update routes: a row is (name, sandbox), immutable
  // once created — delete refuses (409) while sessions still bind it.
  projects: {
    list: () => request<S['store.Project'][]>('/projects'),
    // Admin: every owner's projects, storage hints included.
    listAll: () => request<S['store.Project'][]>('/projects?all=true'),
    create: (data: unknown) => request<S['handler.projectDetail']>('/projects', { method: 'POST', body: JSON.stringify(data) }),
    // The one call that returns an environment, and only to the owner.
    get: (id: string) => request<S['handler.projectDetail']>(`/projects/${id}`),
    update: (id: string, data: unknown) =>
      request<S['handler.projectDetail']>(`/projects/${id}`, { method: 'PUT', body: JSON.stringify(data) }),
    delete: (id: string) => request<null>(`/projects/${id}`, { method: 'DELETE' }),
    // Container calls: create it up front, or discard and recreate it. Both
    // are synchronous and can take an image pull's worth of time.
    prepareContainer: (id: string) => request<null>(`/projects/${id}/container/prepare`, { method: 'POST' }),
    rebuildContainer: (id: string) => request<null>(`/projects/${id}/container/rebuild`, { method: 'POST' }),
  },
  workflows: {
    ...crud<S['store.Workflow']>('/workflows'),
    setScope: setScope('/workflows'),
    setOwner: setOwner('/workflows'),
    // A person's own run of a workflow: the brief they wrote, for the session
    // the result comes back to.
    // sandbox_id/project_id bind a still-unbound session first, so the
    // execution has its file and command tools; a bound session ignores them.
    run: (id: string | number, body: { session_id: string; input: string; sandbox_id?: string; project_id?: string }) =>
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
  tasks: {
    // One page across every conversation, newest first ({items, total}): the
    // hub's Runs view. kind narrows ("workflow"), live keeps only working /
    // input_required rows, limit/offset cut the page.
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
    ...crud<S['store.SandboxConfig']>('/sandboxes'),
    test: (id: string | number) => request(`/sandboxes/${id}/test`, { method: 'POST' }),
  },
  // The OAuth flow belongs to the endpoint: the token is the provider's
  // credential, shared by every agent pointed at it.
  chatgpt: {
    login: (providerId: string | number) => request(`/providers/${providerId}/chatgpt/login`, { method: 'POST' }),
    logout: (providerId: string | number) => request(`/providers/${providerId}/chatgpt/logout`, { method: 'POST' }),
    status: (providerId: string | number) => request(`/providers/${providerId}/chatgpt/status`),
  },
};
