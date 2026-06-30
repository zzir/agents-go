const BASE = '/api';

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
    throw new Error(body.error || res.statusText);
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
    create: (name: string) => request('/sessions', { method: 'POST', body: JSON.stringify({ name }) }),
    update: (id: string | number, name: string) => request(`/sessions/${id}`, { method: 'PUT', body: JSON.stringify({ name }) }),
    messages: (id: string | number) => request(`/sessions/${id}/messages`),
    traces: (id: string | number) => request(`/sessions/${id}/traces`),
    fork: (id: string | number, messageId?: number, opts?: { exclusive?: boolean; label?: string }) => request(`/sessions/${id}/fork`, { method: 'POST', body: JSON.stringify({ message_id: messageId || 0, ...opts }) }),
    pin: (id: string | number, pinned: boolean) => request(`/sessions/${id}/pin`, { method: 'PATCH', body: JSON.stringify({ pinned }) }),
  },
  agents: crud('/agents'),
  mcpServers: {
    ...crud('/mcp-servers'),
    connect: (id: string | number) => request(`/mcp-servers/${id}/connect`, { method: 'POST' }),
    disconnect: (id: string | number) => request(`/mcp-servers/${id}/disconnect`, { method: 'POST' }),
    tools: (id: string | number) => request(`/mcp-servers/${id}/tools`),
  },
  memories: crud('/memories'),
  settings: {
    list: () => request('/settings'),
    get: (key: string) => request(`/settings/${key}`),
    set: (key: string, value: unknown) => request(`/settings/${key}`, { method: 'PUT', body: JSON.stringify({ value }) }),
    delete: (key: string) => request(`/settings/${key}`, { method: 'DELETE' }),
  },
  skills: {
    list: () => request('/skills'),
    get: (path: string) => request(`/skills/${path}`),
    clone: (url: string) => request('/skills/clone', { method: 'POST', body: JSON.stringify({ url }) }),
    update: (name: string) => request(`/skills/${name}`, { method: 'PUT' }),
    delete: (name: string) => request(`/skills/${name}`, { method: 'DELETE' }),
  },
  guardrails: {
    ...crud('/guardrails'),
    list: () => request('/guardrails'),
  },
  providerRoutes: crud('/provider-routes'),
  sandboxes: {
    ...crud('/sandboxes'),
    test: (id: string | number) => request(`/sandboxes/${id}/test`, { method: 'POST' }),
  },
  chatgpt: {
    login: (agentConfigId: string | number) => request(`/chatgpt/login?agent_config_id=${agentConfigId}`, { method: 'POST' }),
    logout: (agentConfigId: string | number) => request(`/chatgpt/logout?agent_config_id=${agentConfigId}`, { method: 'POST' }),
    status: (agentConfigId: string | number) => request(`/chatgpt/status?agent_config_id=${agentConfigId}`),
  },
};
