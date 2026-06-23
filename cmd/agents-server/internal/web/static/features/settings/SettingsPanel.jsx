import React from 'react';
import { api } from '/lib/api.js';
import { useApi } from '/lib/hooks.js';

const { useState, useCallback } = React;
const h = React.createElement;

const DEFAULT_KEYS = [
  { key: 'proxy_url', label: 'Proxy URL', placeholder: 'http://127.0.0.1:7890 or socks5://127.0.0.1:1080', description: 'All outbound API and MCP HTTP requests will be routed through this proxy.' },
  { key: 'system_prompt', label: 'Global System Prompt', placeholder: 'Optional instructions prepended to all agents', multiline: true },
  { key: 'brave_api_key', label: 'Brave Search API Key', placeholder: 'BSA-xxxxxxxx', description: 'When set, a brave_search tool is injected into all agents. Get a key at brave.com/search/api.' },
  { key: 'enable_editor_tools', label: 'Enable Editor Tools', placeholder: 'true / false', description: 'When set to "true", injects file-editing tools (view_file, create_file, str_replace, insert_text) scoped to --root-dir.' },
];

export function SettingsPanel() {
  const { data: settings, reload } = useApi(() => api.settings.list());
  const [saving, setSaving] = useState({});

  const getValue = useCallback((key) => {
    if (!settings) return '';
    const s = settings.find(s => s.key === key);
    return s ? s.value : '';
  }, [settings]);

  const handleSave = async (key, value) => {
    setSaving(prev => ({ ...prev, [key]: true }));
    try { await api.settings.set(key, value); reload(); }
    finally { setSaving(prev => ({ ...prev, [key]: false })); }
  };

  return h('div', null,
    h('h2', { className: 'SectionHeader-title', style: { marginBottom: '16px' } }, 'Settings'),
    DEFAULT_KEYS.map(def =>
      h(SettingRow, { key: def.key, def, value: getValue(def.key), saving: saving[def.key], onSave: v => handleSave(def.key, v) }),
    ),
    h(ProviderRoutesSection, null),
  );
}

function SettingRow({ def, value, saving, onSave }) {
  const [draft, setDraft] = useState(value);
  const changed = draft !== value;
  React.useEffect(() => { setDraft(value); }, [value]);

  const input = def.multiline
    ? h('textarea', { value: draft, onChange: e => setDraft(e.target.value), rows: 3, placeholder: def.placeholder, className: 'form-control form-control-mono' })
    : h('input', { value: draft, onChange: e => setDraft(e.target.value), placeholder: def.placeholder, className: 'form-control' });

  return h('div', { className: 'FormControl' },
    h('label', { className: 'FormControl-label', style: { fontSize: '13px', color: 'var(--color-fg-default)' } }, def.label),
    def.description && h('div', { className: 'FormControl-caption', style: { marginBottom: '6px' } }, def.description),
    input,
    changed && h('button', { onClick: () => onSave(draft), disabled: saving, className: 'btn btn-primary btn-sm', style: { marginTop: '6px' } },
      saving ? 'Saving...' : 'Save',
    ),
  );
}

function ProviderRoutesSection() {
  const { data: routes, reload } = useApi(() => api.providerRoutes.list());
  const [adding, setAdding] = useState(false);
  const [draft, setDraft] = useState({ prefix: '', api_key: '', base_url: '' });

  const handleAdd = async () => {
    if (!draft.prefix) return;
    await api.providerRoutes.create(draft);
    setDraft({ prefix: '', api_key: '', base_url: '' });
    setAdding(false);
    reload();
  };
  const handleDelete = async (id) => { await api.providerRoutes.delete(id); reload(); };

  return h('div', { style: { marginTop: '24px', borderTop: '1px solid var(--color-border-muted)', paddingTop: '16px' } },
    h('div', { className: 'SectionHeader' },
      h('h3', { style: { fontSize: '14px', fontWeight: 600 } }, 'Provider Routes'),
      !adding && h('button', { onClick: () => setAdding(true), className: 'btn btn-primary btn-sm' }, '+ Add'),
    ),
    h('div', { className: 'FormControl-caption', style: { marginBottom: '10px' } },
      'Route model names by prefix (e.g. "groq/llama-3" → prefix "groq"). The agent\'s own provider is the fallback.',
    ),
    adding && h('div', { className: 'form-box' },
      h('input', { value: draft.prefix, onChange: e => setDraft(d => ({ ...d, prefix: e.target.value })), placeholder: 'Prefix (e.g. groq)', className: 'form-control', style: { marginBottom: '6px' } }),
      h('input', { value: draft.api_key, onChange: e => setDraft(d => ({ ...d, api_key: e.target.value })), placeholder: 'API Key', type: 'password', className: 'form-control', style: { marginBottom: '6px' } }),
      h('input', { value: draft.base_url, onChange: e => setDraft(d => ({ ...d, base_url: e.target.value })), placeholder: 'Base URL', className: 'form-control', style: { marginBottom: '8px' } }),
      h('div', { style: { display: 'flex', gap: '6px' } },
        h('button', { onClick: handleAdd, className: 'btn btn-primary btn-sm' }, 'Save'),
        h('button', { onClick: () => setAdding(false), className: 'btn btn-sm' }, 'Cancel'),
      ),
    ),
    h('div', { className: 'Box' },
      routes && routes.map(r =>
        h('div', { key: r.id, className: 'Box-row' },
          h('div', null,
            h('span', { style: { fontWeight: 500, fontSize: '13px' } }, r.prefix + '/'),
            r.base_url && h('span', { style: { fontSize: '11px', color: 'var(--color-fg-muted)', marginLeft: '8px' } }, r.base_url),
          ),
          h('button', { onClick: () => handleDelete(r.id), className: 'btn btn-sm btn-danger' }, 'Delete'),
        ),
      ),
      (!routes || routes.length === 0) && !adding && h('div', { className: 'blankslate' }, 'No provider routes configured.'),
    ),
  );
}
