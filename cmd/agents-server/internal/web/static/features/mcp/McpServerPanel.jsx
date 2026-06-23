import React from 'react';
import { api } from '/lib/api.js';
import { useApi } from '/lib/hooks.js';

const { useState } = React;
const h = React.createElement;
const TRANSPORTS = ['stdio', 'sse', 'streamable_http'];

// flatten turns a stored server (top-level columns + nested config) into the
// flat field set the form edits; pack() is its inverse.
function flatten(s) {
  const c = s.config || {};
  return {
    name: s.name || '', transport_type: s.transport_type || 'stdio', auto_connect: !!s.auto_connect,
    command: c.command || '',
    args: Array.isArray(c.args) ? JSON.stringify(c.args) : '', // array → JSON text for the input
    endpoint: c.endpoint || '',
  };
}

// pack assembles the API payload: shared columns at the top level, transport
// settings under config (interpreted server-side per transport_type).
function pack(form) {
  const base = { name: form.name, transport_type: form.transport_type, auto_connect: form.auto_connect };
  let config;
  if (form.transport_type === 'stdio') {
    let args = [];
    try { args = form.args ? JSON.parse(form.args) : []; } catch (e) { args = []; }
    config = { command: form.command, args };
  } else {
    config = { endpoint: form.endpoint };
  }
  return { ...base, config };
}

function McpForm({ initial, onSave, onCancel }) {
  const [form, setForm] = useState(flatten(initial || {}));
  const set = (k, v) => setForm(prev => ({ ...prev, [k]: v }));
  const isStdio = form.transport_type === 'stdio';

  return h('div', { className: 'form-box' },
    fc('Name', h('input', { value: form.name, onChange: e => set('name', e.target.value), className: 'form-control' })),
    fc('Transport', h('select', { value: form.transport_type, onChange: e => set('transport_type', e.target.value), className: 'form-select' },
      TRANSPORTS.map(t => h('option', { key: t, value: t }, t)),
    )),
    isStdio && fc('Command', h('input', { value: form.command, onChange: e => set('command', e.target.value), placeholder: 'npx -y @modelcontextprotocol/server-filesystem', className: 'form-control' })),
    isStdio && fc('Args (JSON array)', h('input', { value: form.args, onChange: e => set('args', e.target.value), placeholder: '["/path/to/dir"]', className: 'form-control form-control-mono' })),
    !isStdio && fc('Endpoint', h('input', { value: form.endpoint, onChange: e => set('endpoint', e.target.value), placeholder: 'http://localhost:3000/mcp', className: 'form-control' })),
    h('label', { className: 'form-checkbox', style: { marginBottom: '12px' } },
      h('input', { type: 'checkbox', checked: form.auto_connect, onChange: e => set('auto_connect', e.target.checked) }),
      'Auto-connect on server start',
    ),
    h('div', { style: { display: 'flex', gap: '8px' } },
      h('button', { onClick: () => onSave(pack(form)), className: 'btn btn-primary' }, 'Save'),
      onCancel && h('button', { onClick: onCancel, className: 'btn' }, 'Cancel'),
    ),
  );
}

export function McpServerPanel() {
  const { data: servers, reload } = useApi(() => api.mcpServers.list());
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState(null);
  const [connecting, setConnecting] = useState({});

  const handleSave = async (form) => {
    if (editing) { await api.mcpServers.update(editing.id, form); }
    else { await api.mcpServers.create(form); }
    setEditing(null); setAdding(false); reload();
  };

  const handleConnect = async (id) => {
    setConnecting(prev => ({ ...prev, [id]: true }));
    try { await api.mcpServers.connect(id); } catch (e) { console.error('connect failed:', e); }
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
    h('div', { className: 'Box' },
      servers && servers.map(s =>
        h('div', { key: s.id, className: 'Box-row' },
          h('div', { style: { flex: 1, minWidth: 0 } },
            h('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } },
              h('span', { style: { width: 8, height: 8, borderRadius: '50%', display: 'inline-block', background: s.connected ? 'var(--color-success-fg)' : 'var(--color-fg-subtle)' } }),
              h('span', { style: { fontWeight: 500, fontSize: '14px' } }, s.name),
            ),
            h('div', { style: { fontSize: '12px', color: 'var(--color-fg-muted)', marginTop: '4px', marginLeft: '16px' } },
              s.transport_type + (s.config && s.config.command ? ': ' + s.config.command : '') + (s.config && s.config.endpoint ? ': ' + s.config.endpoint : ''),
            ),
          ),
          h('div', { style: { display: 'flex', gap: '6px', flexShrink: 0, alignItems: 'center' } },
            !s.connected
              ? h('button', { onClick: () => handleConnect(s.id), disabled: connecting[s.id], className: 'btn btn-sm', style: { color: 'var(--color-success-fg)' } }, connecting[s.id] ? '...' : 'Connect')
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

function fc(label, input) {
  return h('div', { className: 'FormControl' },
    label && h('label', { className: 'FormControl-label' }, label),
    input,
  );
}
