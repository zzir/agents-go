import React from 'react';
import { api } from '/lib/api.js';
import { useApi } from '/lib/hooks.js';
import { fc } from '/lib/form.js';

const { useState, useEffect, useCallback } = React;
const h = React.createElement;
const TRANSPORTS = ['stdio', 'streamable_http'];
const AUTH_MODES = [
  { value: '', label: 'None' },
  { value: 'header', label: 'Static headers' },
  { value: 'oauth', label: 'OAuth' },
];

function flatten(s) {
  const c = s.config || {};
  return {
    name: s.name || '', transport_type: s.transport_type || 'stdio', auto_connect: !!s.auto_connect,
    command: c.command || '',
    args: Array.isArray(c.args) ? JSON.stringify(c.args) : '',
    endpoint: c.endpoint || '',
    headers: c.headers ? JSON.stringify(c.headers) : '',
    auth_mode: c.auth_mode || '',
    oauth_client_id: c.oauth_client_id || '',
    oauth_client_secret: c.oauth_client_secret || '',
    oauth_scopes: c.oauth_scopes || '',
  };
}

function pack(form) {
  const base = { name: form.name, transport_type: form.transport_type, auto_connect: form.auto_connect };
  let config;
  if (form.transport_type === 'stdio') {
    let args = [];
    try { args = form.args ? JSON.parse(form.args) : []; } catch (e) { args = []; }
    config = { command: form.command, args };
  } else {
    config = { endpoint: form.endpoint };
    if (form.auth_mode === 'header' || !form.auth_mode) {
      let headers = null;
      try { headers = form.headers ? JSON.parse(form.headers) : null; } catch (e) { headers = null; }
      if (headers && typeof headers === 'object' && Object.keys(headers).length > 0) config.headers = headers;
    }
    if (form.auth_mode === 'oauth') {
      config.auth_mode = 'oauth';
      if (form.oauth_client_id) config.oauth_client_id = form.oauth_client_id;
      if (form.oauth_client_secret) config.oauth_client_secret = form.oauth_client_secret;
      if (form.oauth_scopes) config.oauth_scopes = form.oauth_scopes;
    } else if (form.auth_mode === 'header') {
      config.auth_mode = 'header';
    }
  }
  return { ...base, config };
}

function McpForm({ initial, onSave, onCancel }) {
  const [form, setForm] = useState(flatten(initial || {}));
  const set = (k, v) => setForm(prev => ({ ...prev, [k]: v }));
  const isStdio = form.transport_type === 'stdio';
  const isOAuth = form.auth_mode === 'oauth';
  const isHeader = form.auth_mode === 'header';

  return h('div', { className: 'form-box' },
    fc('Name', h('input', { value: form.name, onChange: e => set('name', e.target.value), className: 'form-control' })),
    fc('Transport', h('select', { value: form.transport_type, onChange: e => set('transport_type', e.target.value), className: 'form-select' },
      TRANSPORTS.map(t => h('option', { key: t, value: t }, t)),
    )),
    isStdio && fc('Command', h('input', { value: form.command, onChange: e => set('command', e.target.value), placeholder: 'npx -y @modelcontextprotocol/server-filesystem', className: 'form-control' })),
    isStdio && fc('Args (JSON array)', h('input', { value: form.args, onChange: e => set('args', e.target.value), placeholder: '["/path/to/dir"]', className: 'form-control form-control-mono' })),
    !isStdio && fc('Endpoint', h('input', { value: form.endpoint, onChange: e => set('endpoint', e.target.value), placeholder: 'http://localhost:3000/mcp', className: 'form-control' })),
    !isStdio && fc('Authentication', h('select', { value: form.auth_mode, onChange: e => set('auth_mode', e.target.value), className: 'form-select' },
      AUTH_MODES.map(m => h('option', { key: m.value, value: m.value }, m.label)),
    )),
    !isStdio && isHeader && fc('Headers (JSON object)',
      h('div', null,
        h('input', { value: form.headers, onChange: e => set('headers', e.target.value), placeholder: '{"Authorization": "Bearer <token>"}', className: 'form-control form-control-mono' }),
        h('span', { className: 'FormControl-caption' }, 'Sent with every request, e.g. an auth or API-key header. Leave empty for none.'),
      ),
    ),
    !isStdio && isOAuth && fc('Client ID',
      h('div', null,
        h('input', { value: form.oauth_client_id, onChange: e => set('oauth_client_id', e.target.value), placeholder: 'Leave empty for dynamic registration', className: 'form-control form-control-mono' }),
        h('span', { className: 'FormControl-caption' }, 'Pre-registered OAuth client ID. Leave empty to use dynamic client registration (DCR).'),
      ),
    ),
    !isStdio && isOAuth && form.oauth_client_id && fc('Client Secret',
      h('input', { value: form.oauth_client_secret, onChange: e => set('oauth_client_secret', e.target.value), type: 'password', className: 'form-control form-control-mono' }),
    ),
    !isStdio && isOAuth && fc('Scopes',
      h('div', null,
        h('input', { value: form.oauth_scopes, onChange: e => set('oauth_scopes', e.target.value), placeholder: 'read write', className: 'form-control form-control-mono' }),
        h('span', { className: 'FormControl-caption' }, 'Space-separated OAuth scopes to request.'),
      ),
    ),
    h('label', { className: 'form-checkbox' },
      h('input', { type: 'checkbox', checked: form.auto_connect, onChange: e => set('auto_connect', e.target.checked) }),
      'Auto-connect on server start',
    ),
    h('div', { className: 'form-actions' },
      h('button', { onClick: () => onSave(pack(form)), className: 'btn btn-primary' }, 'Save'),
      onCancel && h('button', { onClick: onCancel, className: 'btn' }, 'Cancel'),
    ),
  );
}

function statusDot(s) {
  if (s.connected) return 'var(--color-success-fg)';
  if (s.auth_state === 'unauthorized') return 'var(--color-attention-fg, var(--color-fg-subtle))';
  return 'var(--color-fg-subtle)';
}

function connectLabel(s, connecting) {
  if (connecting) return '...';
  if (s.auth_state === 'unauthorized') return 'Authorize';
  if (s.auth_state === 'authorizing') return 'Authorizing...';
  return 'Connect';
}

export function McpServerPanel() {
  const { data: servers, reload } = useApi(() => api.mcpServers.list());
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState(null);
  const [connecting, setConnecting] = useState({});
  const [authorizing, setAuthorizing] = useState({});
  const [error, setError] = useState(null);

  // Tracks active OAuth poll per server id so we can cancel from any path.
  const pollRef = React.useRef({});

  const stopPoll = useCallback((id) => {
    const entry = pollRef.current[id];
    if (entry) {
      clearInterval(entry.interval);
      clearTimeout(entry.timeout);
      delete pollRef.current[id];
    }
    setAuthorizing(prev => {
      if (!prev[id]) return prev;
      return { ...prev, [id]: false };
    });
  }, []);

  const stopAllPolls = useCallback(() => {
    for (const id of Object.keys(pollRef.current)) stopPoll(id);
  }, [stopPoll]);

  // Cleanup all polls on unmount.
  useEffect(() => stopAllPolls, [stopAllPolls]);

  // postMessage fast-path: the callback page fires this the moment it loads,
  // but the backend is still exchanging the code for a token and connecting.
  // So we just reload (to show progress) and let the poll keep running — the
  // poll will stop itself once it sees connected: true.
  useEffect(() => {
    const handler = (event) => {
      if (event.data && event.data.type === 'mcp-oauth-done') {
        reload();
      }
    };
    window.addEventListener('message', handler);
    return () => window.removeEventListener('message', handler);
  }, [reload]);

  const handleSave = async (form) => {
    if (editing) { await api.mcpServers.update(editing.id, form); }
    else { await api.mcpServers.create(form); }
    setEditing(null); setAdding(false); reload();
  };

  const handleConnect = async (id) => {
    setConnecting(prev => ({ ...prev, [id]: true }));
    try {
      const res = await api.mcpServers.connect(id);
      if (res && res.status === 'authorization_required' && res.authorize_url) {
        // Cancel any previous poll for this id (duplicate click guard).
        stopPoll(id);
        setAuthorizing(prev => ({ ...prev, [id]: true }));
        window.open(res.authorize_url, 'mcp_oauth', 'width=520,height=640,popup=yes');
        // Poll our own backend — immune to COOP.
        const interval = setInterval(async () => {
          try {
            const srv = await api.mcpServers.get(id);
            if (srv && srv.connected) {
              stopPoll(id);
              reload();
            }
          } catch (_) { /* ignore transient errors */ }
        }, 2000);
        const timeout = setTimeout(() => { stopPoll(id); reload(); }, 5 * 60 * 1000);
        pollRef.current[id] = { interval, timeout };
      }
    } catch (e) {
      setError(e.message || 'Connect failed');
      setTimeout(() => setError(null), 8000);
    }
    setConnecting(prev => ({ ...prev, [id]: false }));
    reload();
  };
  const handleDisconnect = async (id) => { await api.mcpServers.disconnect(id); reload(); };
  const handleDelete = async (id) => { await api.mcpServers.delete(id); reload(); };

  return h('div', null,
    h('div', { className: 'SectionHeader' },
      h('h2', { className: 'SectionHeader-title' }, 'MCP Servers'),
      !adding && h('button', { onClick: () => setAdding(true), className: 'btn btn-primary btn-sm' }, '+ Add'),
    ),
    adding && h(McpForm, { onSave: handleSave, onCancel: () => setAdding(false) }),
    editing && h(McpForm, { initial: editing, onSave: handleSave, onCancel: () => setEditing(null) }),
    error && h('div', {
      className: 'flash flash-error',
      onClick: () => setError(null),
    }, error),
    h('div', { className: 'Box' },
      servers && servers.map(s =>
        h('div', { key: s.id, className: 'Box-row' },
          h('div', { style: { flex: 1, minWidth: 0 } },
            h('div', { className: 'form-status' },
              h('span', { className: 'form-status-dot', style: { background: statusDot(s) } }),
              h('span', { style: { fontWeight: 500, fontSize: '14px' } }, s.name),
              s.config && s.config.auth_mode === 'oauth' && h('span', { className: 'Label Label--secondary' }, 'OAuth'),
            ),
            h('div', { style: { fontSize: '12px', color: 'var(--color-fg-muted)', marginTop: '4px', marginLeft: '16px' } },
              s.transport_type + (s.config && s.config.command ? ': ' + s.config.command : '') + (s.config && s.config.endpoint ? ': ' + s.config.endpoint : ''),
            ),
          ),
          h('div', { style: { display: 'flex', gap: '6px', flexShrink: 0, alignItems: 'center' } },
            !s.connected
              ? h('button', {
                  onClick: () => handleConnect(s.id),
                  disabled: connecting[s.id] || authorizing[s.id],
                  className: 'btn btn-sm',
                  style: { color: 'var(--color-success-fg)' },
                }, connectLabel(s, connecting[s.id] || authorizing[s.id]))
              : h('button', { onClick: () => handleDisconnect(s.id), className: 'btn btn-sm btn-invisible' }, 'Disconnect'),
            h('button', { onClick: () => { setAdding(false); setEditing(s); }, className: 'btn btn-sm btn-invisible' }, 'Edit'),
            h('button', { onClick: () => handleDelete(s.id), className: 'btn btn-sm btn-danger' }, 'Delete'),
          ),
        ),
      ),
      (!servers || servers.length === 0) && !adding && h('div', { className: 'blankslate' }, 'No MCP servers configured.'),
    ),
  );
}

