const BASE = '/api/v1';

export function getToken(): string {
  return sessionStorage.getItem('auth_token') || '';
}

export function setToken(t: string): void {
  sessionStorage.setItem('auth_token', t);
}

export function clearToken(): void {
  sessionStorage.removeItem('auth_token');
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function request(path: string, opts: RequestInit = {}): Promise<any> {
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
  if (res.status === 204) return null;
  return res.json();
}

export async function login(token: string): Promise<boolean> {
  const res = await fetch(`${BASE}/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ token }),
  });
  if (!res.ok) throw new Error('invalid token');
  setToken(token);
  return true;
}

export async function checkAuth(): Promise<boolean> {
  const t = getToken();
  if (!t) return false;
  const res = await fetch(`${BASE}/auth/check`, {
    headers: { 'Authorization': `Bearer ${t}` },
  });
  if (!res.ok) { clearToken(); return false; }
  return true;
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
interface CrudMethods {
  list: () => Promise<any>;
  create: (data: unknown) => Promise<any>;
  get: (id: string | number) => Promise<any>;
  update: (id: string | number, data: unknown) => Promise<any>;
  delete: (id: string | number) => Promise<any>;
}

function crud(base: string): CrudMethods {
  return {
    list: () => request(base),
    create: (data: unknown) => request(base, { method: 'POST', body: JSON.stringify(data) }),
    get: (id: string | number) => request(`${base}/${id}`),
    update: (id: string | number, data: unknown) => request(`${base}/${id}`, { method: 'PUT', body: JSON.stringify(data) }),
    delete: (id: string | number) => request(`${base}/${id}`, { method: 'DELETE' }),
  };
}

export const api = {
  sessions: {
    ...crud('/sessions'),
    create: (name: string, agentConfigId?: string) => request('/sessions', { method: 'POST', body: JSON.stringify({ name, ...(agentConfigId ? { agent_config_id: agentConfigId } : {}) }) }),
    update: (id: string | number, name: string) => request(`/sessions/${id}`, { method: 'PATCH', body: JSON.stringify({ name }) }),
    // limit/beforeId page BACKWARDS: the newest `limit` entries first, then
    // older pages keyed on the smallest id received. A cursor rather than an
    // offset because entries keep arriving — an offset shifts under a
    // concurrent append and silently repeats or skips a row.
    messages: (id: string | number, opts?: { limit?: number; beforeId?: number }) => {
      const q = new URLSearchParams();
      if (opts?.limit) q.set('limit', String(opts.limit));
      if (opts?.beforeId) q.set('before_id', String(opts.beforeId));
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
    fork: (id: string | number, messageId?: number, opts?: { exclusive?: boolean; label?: string }) => request(`/sessions/${id}/fork`, { method: 'POST', body: JSON.stringify({ ...(messageId ? { message_id: messageId } : {}), ...opts }) }),
    pin: (id: string | number, pinned: boolean) => request(`/sessions/${id}`, { method: 'PATCH', body: JSON.stringify({ pinned }) }),
  },
  agents: {
    ...crud('/agents'),
    // The agent's CURRENT tool surface as schema-only definitions — what the
    // bridge would hand the model right now (sandbox tools excluded). Backs
    // the Replay dialog's tool picker.
    tools: (id: string | number) => request(`/agents/${id}/tools`),
  },
  mcpServers: {
    ...crud('/mcp-servers'),
    connect: (id: string | number) => request(`/mcp-servers/${id}/connect`, { method: 'POST' }),
    clearOAuth: (id: string | number) => request(`/mcp-servers/${id}/oauth-token`, { method: 'DELETE' }),
    tools: (id: string | number) => request(`/mcp-servers/${id}/tools`),
  },
  memories: crud('/memories'),
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
    list: () => request('/skills'),
    get: (path: string) => request(`/skills/${path}`),
    // Management operates on whole repos under /skill-repos.
    clone: (url: string) => request('/skill-repos', { method: 'POST', body: JSON.stringify({ url }) }),
    update: (name: string) => request(`/skill-repos/${name}/sync`, { method: 'POST' }),
    delete: (name: string) => request(`/skill-repos/${name}`, { method: 'DELETE' }),
  },
  guardrails: {
    ...crud('/guardrails'),
    list: () => request('/guardrails'),
  },
  providers: crud('/providers'),
  workflows: {
    ...crud('/workflows'),
    // A person's own run of a workflow: the brief they wrote, for the session
    // the result comes back to.
    // sandbox_id/work_dir bind a still-unbound session first, so the
    // execution has its file and command tools; a bound session ignores them.
    run: (id: string | number, body: { session_id: string; input: string; sandbox_id?: string; work_dir?: string }) =>
      request(`/workflows/${id}/runs`, { method: 'POST', body: JSON.stringify(body) }),
  },
  // Triggers start a workflow without a conversation asking: on a cron
  // schedule, or on a signed webhook call. fire runs one by hand.
  triggers: {
    ...crud('/triggers'),
    listFor: (workflowId: string) => request(`/triggers?workflow_id=${encodeURIComponent(workflowId)}`),
    fire: (id: string | number, payload = '') => request(`/triggers/${id}/fire`, { method: 'POST', body: JSON.stringify({ payload }) }),
    rotateSecret: (id: string | number) => request(`/triggers/${id}/rotate-secret`, { method: 'POST' }),
  },
  providerRoutes: crud('/provider-routes'),
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
    ...crud('/sandboxes'),
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
