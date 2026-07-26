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
    tasks: (id: string | number) => request(`/sessions/${id}/tasks`),
    traces: (id: string | number) => request(`/sessions/${id}/traces`),
    approvals: (id: string | number) => request(`/sessions/${id}/approvals`),
    fork: (id: string | number, messageId?: number, opts?: { exclusive?: boolean; label?: string }) => request(`/sessions/${id}/fork`, { method: 'POST', body: JSON.stringify({ ...(messageId ? { message_id: messageId } : {}), ...opts }) }),
    pin: (id: string | number, pinned: boolean) => request(`/sessions/${id}`, { method: 'PATCH', body: JSON.stringify({ pinned }) }),
  },
  agents: crud('/agents'),
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
    }) =>
      request('/playground/generate', { method: 'POST', body: JSON.stringify(body) }),
  },
  settings: {
    list: () => request('/settings'),
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
  providerRoutes: crud('/provider-routes'),
  tasks: {
    stop: (id: string | number, graceful = false) => request(`/tasks/${id}/stop`, { method: 'POST', body: JSON.stringify({ graceful }) }),
  },
  sandboxes: {
    ...crud('/sandboxes'),
    test: (id: string | number) => request(`/sandboxes/${id}/test`, { method: 'POST' }),
  },
  chatgpt: {
    login: (agentConfigId: string | number) => request(`/agents/${agentConfigId}/chatgpt/login`, { method: 'POST' }),
    logout: (agentConfigId: string | number) => request(`/agents/${agentConfigId}/chatgpt/logout`, { method: 'POST' }),
    status: (agentConfigId: string | number) => request(`/agents/${agentConfigId}/chatgpt/status`),
  },
};
