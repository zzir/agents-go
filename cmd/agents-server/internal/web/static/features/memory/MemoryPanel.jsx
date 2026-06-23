import React from 'react';
import { api } from '/lib/api.js';
import { useApi } from '/lib/hooks.js';

const { useState } = React;
const h = React.createElement;

function MemoryForm({ initial, onSave, onCancel, agents }) {
  const [form, setForm] = useState(initial || { key: '', content: '', metadata: '', agent_config_id: '' });
  const set = (k, v) => setForm(prev => ({ ...prev, [k]: v }));

  return h('div', { className: 'form-box' },
    fc('Agent', h('select', { value: form.agent_config_id || '', onChange: e => set('agent_config_id', e.target.value), className: 'form-select' },
      h('option', { value: '' }, '(Global - all agents)'),
      agents && agents.map(a => h('option', { key: a.id, value: a.id }, a.name)),
    )),
    fc('Key', h('input', { value: form.key, onChange: e => set('key', e.target.value), className: 'form-control', placeholder: 'unique-key' })),
    fc('Content', h('textarea', { value: form.content, onChange: e => set('content', e.target.value), rows: 4, className: 'form-control' })),
    fc('Metadata (JSON)', h('input', { value: form.metadata, onChange: e => set('metadata', e.target.value), placeholder: '{"tag": "value"}', className: 'form-control form-control-mono' })),
    h('div', { style: { display: 'flex', gap: '8px', marginTop: '12px' } },
      h('button', { onClick: () => onSave(form), className: 'btn btn-primary' }, 'Save'),
      onCancel && h('button', { onClick: onCancel, className: 'btn' }, 'Cancel'),
    ),
  );
}

export function MemoryPanel() {
  const { data: memories, reload } = useApi(() => api.memories.list());
  const { data: agents } = useApi(() => api.agents.list());
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState(null);

  const agentName = (id) => {
    if (!id || !agents) return 'Global';
    const a = agents.find(a => a.id === id);
    return a ? a.name : id.substring(0, 8);
  };

  const handleSave = async (form) => {
    if (editing) { await api.memories.update(editing.id, form); }
    else { await api.memories.create(form); }
    setEditing(null); setAdding(false); reload();
  };
  const handleDelete = async (id) => { await api.memories.delete(id); reload(); };

  return h('div', null,
    h('div', { className: 'SectionHeader' },
      h('h2', { className: 'SectionHeader-title' }, 'Memory'),
      !adding && h('button', { onClick: () => setAdding(true), className: 'btn btn-primary btn-sm' }, '+ Add'),
    ),
    adding && h(MemoryForm, { onSave: handleSave, onCancel: () => setAdding(false), agents }),
    editing && h(MemoryForm, { initial: editing, onSave: handleSave, onCancel: () => setEditing(null), agents }),
    h('div', { className: 'Box' },
      memories && memories.map(m =>
        h('div', { key: m.id, className: 'Box-row' },
          h('div', { style: { flex: 1, minWidth: 0 } },
            h('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } },
              h('span', { className: 'Label ' + (m.agent_config_id ? 'Label-accent' : 'Label-default') }, agentName(m.agent_config_id)),
              h('span', { style: { fontWeight: 500, fontSize: '13px', color: 'var(--color-accent-fg)', fontFamily: 'var(--font-mono)' } }, m.key),
            ),
            h('div', { style: { fontSize: '13px', color: 'var(--color-fg-default)', marginTop: '4px', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } },
              m.content.substring(0, 120) + (m.content.length > 120 ? '...' : ''),
            ),
          ),
          h('div', { style: { display: 'flex', gap: '6px', flexShrink: 0 } },
            h('button', { onClick: () => { setAdding(false); setEditing(m); }, className: 'btn btn-sm btn-invisible' }, 'Edit'),
            h('button', { onClick: () => handleDelete(m.id), className: 'btn btn-sm btn-danger' }, 'Delete'),
          ),
        ),
      ),
      (!memories || memories.length === 0) && !adding && h('div', { className: 'blankslate' }, 'No memories stored.'),
    ),
  );
}

function fc(label, input) {
  return h('div', { className: 'FormControl' },
    h('label', { className: 'FormControl-label' }, label),
    input,
  );
}
