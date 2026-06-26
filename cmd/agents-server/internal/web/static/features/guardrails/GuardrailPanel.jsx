import React from 'react';
import { api } from '/lib/api.js';
import { useApi } from '/lib/hooks.js';

const { useState } = React;
const h = React.createElement;

const TYPES = ['input', 'output'];
const MODES = ['regex', 'max_length'];
const MODE_LABELS = { regex: 'Regex Pattern', max_length: 'Max Length' };

function GuardrailForm({ initial, onSave, onCancel }) {
  const [form, setForm] = useState(initial || {
    name: '', description: '', type: 'input', mode: 'regex',
  });
  const [pattern, setPattern] = useState(() => {
    try { const c = JSON.parse((initial && initial.config) || '{}'); return c.pattern || ''; } catch { return ''; }
  });
  const [maxLength, setMaxLength] = useState(() => {
    try { const c = JSON.parse((initial && initial.config) || '{}'); return c.max_length || 0; } catch { return 0; }
  });
  const set = (k, v) => setForm(prev => ({ ...prev, [k]: v }));

  const handleSave = () => {
    const config = form.mode === 'regex'
      ? { pattern }
      : { max_length: parseInt(maxLength) || 0 };
    onSave({ ...form, config: JSON.stringify(config) });
  };

  return h('div', { className: 'form-box' },
    fc('Name', h('input', { value: form.name, onChange: e => set('name', e.target.value), placeholder: 'e.g. block_profanity', className: 'form-control' })),
    fc('Description', h('input', { value: form.description || '', onChange: e => set('description', e.target.value), placeholder: 'What this guardrail does', className: 'form-control' })),
    fc('Type', h('div', { className: 'SegmentedControl', role: 'radiogroup' },
      TYPES.map(v =>
        h('button', {
          key: v, type: 'button', className: 'SegmentedControl-item', role: 'radio',
          'aria-checked': form.type === v ? 'true' : 'false',
          onClick: () => set('type', v),
        }, v.charAt(0).toUpperCase() + v.slice(1)),
      ),
    ), 'Applied on user input or model output'),
    fc('Mode', h('div', { className: 'SegmentedControl', role: 'radiogroup' },
      MODES.map(v =>
        h('button', {
          key: v, type: 'button', className: 'SegmentedControl-item', role: 'radio',
          'aria-checked': form.mode === v ? 'true' : 'false',
          onClick: () => set('mode', v),
        }, MODE_LABELS[v]),
      ),
    )),
    form.mode === 'regex' && fc('Pattern', h('input', { value: pattern, onChange: e => setPattern(e.target.value), placeholder: '(?i)\\bbadword\\b', className: 'form-control form-control-mono' }), 'Go regexp syntax. Triggers when matched.'),
    form.mode === 'max_length' && fc('Max Length', h('input', { type: 'number', min: 1, value: maxLength || '', onChange: e => setMaxLength(e.target.value), placeholder: '4096', className: 'form-control', style: { width: '120px' } }), 'Maximum character count'),
    h('div', { className: 'form-actions' },
      h('button', { onClick: handleSave, className: 'btn btn-primary' }, 'Save'),
      onCancel && h('button', { onClick: onCancel, className: 'btn' }, 'Cancel'),
    ),
  );
}

export function GuardrailPanel() {
  const { data: guardrails, reload } = useApi(() => api.guardrails.list());
  const [editing, setEditing] = useState(null);
  const [adding, setAdding] = useState(false);

  const handleSave = async (form) => {
    if (editing) { await api.guardrails.update(editing.id, form); }
    else { await api.guardrails.create(form); }
    setEditing(null);
    setAdding(false);
    reload();
  };

  const handleDelete = async (id) => {
    await api.guardrails.delete(id);
    reload();
  };

  const isBuiltin = (g) => !g.id;

  return h('div', null,
    h('div', { className: 'SectionHeader' },
      h('h2', { className: 'SectionHeader-title' }, 'Guardrails'),
      !adding && h('button', { onClick: () => setAdding(true), className: 'btn btn-primary btn-sm' }, '+ Add'),
    ),

    adding && h(GuardrailForm, { onSave: handleSave, onCancel: () => setAdding(false) }),
    editing && h(GuardrailForm, { initial: editing, onSave: handleSave, onCancel: () => setEditing(null) }),

    h('div', { className: 'Box' },
      guardrails && guardrails.map((g, i) =>
        h('div', { key: g.id || ('builtin-' + i), className: 'Box-row' },
          h('div', { style: { flex: 1, minWidth: 0 } },
            h('div', { style: { fontWeight: 500, fontSize: '14px', display: 'flex', alignItems: 'center', gap: '6px' } },
              g.name,
              isBuiltin(g) && h('span', { className: 'Label' }, 'built-in'),
            ),
            h('div', { style: { fontSize: '12px', color: 'var(--color-fg-muted)', marginTop: '2px' } },
              [g.type, g.mode].filter(Boolean).join(' · '),
              g.description && (' — ' + g.description),
            ),
          ),
          !isBuiltin(g) && h('div', { style: { display: 'flex', gap: '6px', flexShrink: 0 } },
            h('button', { onClick: () => { setAdding(false); setEditing(g); }, className: 'btn btn-sm btn-invisible' }, 'Edit'),
            h('button', { onClick: () => handleDelete(g.id), className: 'btn btn-sm btn-danger' }, 'Delete'),
          ),
        ),
      ),
      (!guardrails || guardrails.length === 0) && !adding && h('div', { className: 'blankslate' },
        'No guardrails configured. Built-in guardrails (content_filter, max_input_length, max_output_length) are always available.',
      ),
    ),
  );
}

function fc(label, input, hint) {
  return h('div', { className: 'FormControl' },
    h('label', { className: 'FormControl-label' }, label),
    input,
    hint && h('div', { className: 'FormControl-caption' }, hint),
  );
}
