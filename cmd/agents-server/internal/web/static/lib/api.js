const BASE = '/api';

async function request(path, opts = {}) {
  const res = await fetch(`${BASE}${path}`, {
    headers: { 'Content-Type': 'application/json', ...opts.headers },
    ...opts,
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || res.statusText);
  }
  return res.json();
}

function crud(base) {
  return {
    list: () => request(base),
    create: (data) => request(base, { method: 'POST', body: JSON.stringify(data) }),
    get: (id) => request(`${base}/${id}`),
    update: (id, data) => request(`${base}/${id}`, { method: 'PUT', body: JSON.stringify(data) }),
    delete: (id) => request(`${base}/${id}`, { method: 'DELETE' }),
  };
}

export const api = {
  sessions: {
    ...crud('/sessions'),
    create: (name) => request('/sessions', { method: 'POST', body: JSON.stringify({ name }) }),
    update: (id, name) => request(`/sessions/${id}`, { method: 'PUT', body: JSON.stringify({ name }) }),
    messages: (id) => request(`/sessions/${id}/messages`),
    traces: (id) => request(`/sessions/${id}/traces`),
  },
  agents: crud('/agents'),
  mcpServers: {
    ...crud('/mcp-servers'),
    connect: (id) => request(`/mcp-servers/${id}/connect`, { method: 'POST' }),
    disconnect: (id) => request(`/mcp-servers/${id}/disconnect`, { method: 'POST' }),
    tools: (id) => request(`/mcp-servers/${id}/tools`),
  },
  memories: crud('/memories'),
  settings: {
    list: () => request('/settings'),
    get: (key) => request(`/settings/${key}`),
    set: (key, value) => request(`/settings/${key}`, { method: 'PUT', body: JSON.stringify({ value }) }),
    delete: (key) => request(`/settings/${key}`, { method: 'DELETE' }),
  },
  skills: {
    list: () => request('/skills'),
    get: (path) => request(`/skill/${path}`),
    clone: (url) => request('/skills/clone', { method: 'POST', body: JSON.stringify({ url }) }),
    update: (name) => request(`/skills/${name}/update`, { method: 'POST' }),
    delete: (name) => request(`/skills/${name}`, { method: 'DELETE' }),
  },
  files: {
    list: (path) => request(`/files?path=${encodeURIComponent(path || '')}`),
    read: (path) => request(`/files/${encodeURIComponent(path)}`),
  },
  guardrails: {
    list: () => request('/guardrails'),
  },
  providerRoutes: crud('/provider-routes'),
  sandboxes: {
    ...crud('/sandboxes'),
    exec: (id, code) => request(`/sandboxes/${id}/exec`, { method: 'POST', body: JSON.stringify({ code }) }),
  },
};
