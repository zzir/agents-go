import React from 'react';
import { api } from '/lib/api.js';
import { useApi } from '/lib/hooks.js';

const { useState } = React;
const h = React.createElement;
const TYPES = ['local', 'docker'];

const TYPE_INFO = {
  local:  'Run code directly on the host machine.',
  docker: 'Run code inside an isolated Docker container.',
};

function SandboxForm({ initial, onSave, onCancel }) {
  const defaults = { name: '', type: 'docker', host: '', image: 'python:3.12-slim', network: false, run_cmd: '', filename: '', timeout: 30 };
  const [form, setForm] = useState(initial || defaults);
  const set = (k, v) => setForm(prev => ({ ...prev, [k]: v }));
  const t = form.type;

  return h('div', { className: 'form-box' },
    fc('Name', h('input', { value: form.name, onChange: e => set('name', e.target.value), placeholder: 'e.g. my-sandbox', className: 'form-control' })),
    fc('Type',
      h('div', null,
        h('select', { value: t, onChange: e => set('type', e.target.value), className: 'form-select' },
          TYPES.map(v => h('option', { key: v, value: v }, v === 'local' ? 'Local' : 'Docker')),
        ),
        h('span', { className: 'FormControl-caption' }, TYPE_INFO[t]),
      ),
    ),

    t === 'docker' && fc('Docker Host',
      h('div', null,
        h('input', { value: form.host, onChange: e => set('host', e.target.value), placeholder: 'unix:///var/run/docker.sock', className: 'form-control' }),
        h('span', { className: 'FormControl-caption' }, 'Unix socket or TCP address. Leave empty to use DOCKER_HOST env or the platform default.'),
      ),
    ),
    t === 'docker' && fc('Image',
      h('input', { value: form.image, onChange: e => set('image', e.target.value), placeholder: 'python:3.12-slim', className: 'form-control' }),
    ),
    t === 'docker' && h('label', { className: 'form-checkbox', style: { marginBottom: '12px' } },
      h('input', { type: 'checkbox', checked: form.network, onChange: e => set('network', e.target.checked) }),
      'Allow network access',
    ),

    fc('Run Command',
      h('div', null,
        h('input', { value: form.run_cmd, onChange: e => set('run_cmd', e.target.value), placeholder: '["python", "main.py"]', className: 'form-control form-control-mono' }),
        h('span', { className: 'FormControl-caption' }, 'JSON array. Leave empty for the image default entrypoint.'),
      ),
    ),
    fc('Filename',
      h('div', null,
        h('input', { value: form.filename, onChange: e => set('filename', e.target.value), placeholder: 'main.py', className: 'form-control' }),
        h('span', { className: 'FormControl-caption' }, 'Code is written to this file before execution.'),
      ),
    ),
    fc('Timeout',
      h('input', { type: 'number', value: form.timeout, onChange: e => set('timeout', parseInt(e.target.value) || 0), placeholder: '30', className: 'form-control', min: 0, style: { width: '120px' } }),
      'seconds',
    ),
    h('div', { style: { display: 'flex', gap: '8px', marginTop: '12px' } },
      h('button', { onClick: () => onSave(form), className: 'btn btn-primary' }, 'Save'),
      onCancel && h('button', { onClick: onCancel, className: 'btn' }, 'Cancel'),
    ),
  );
}

function ExecPanel({ sandbox, onClose }) {
  const [code, setCode] = useState('print("Hello from sandbox!")');
  const [result, setResult] = useState(null);
  const [running, setRunning] = useState(false);

  const handleRun = async () => {
    setRunning(true); setResult(null);
    try { const res = await api.sandboxes.exec(sandbox.id, code); setResult(res); }
    catch (e) { setResult({ stderr: e.message, exit_code: -1 }); }
    setRunning(false);
  };

  return h('div', { className: 'form-box' },
    h('div', { style: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '8px' } },
      h('span', { style: { fontSize: '13px', fontWeight: 500 } }, 'Test: ' + sandbox.name),
      h('button', { onClick: onClose, className: 'btn btn-sm btn-invisible' }, '×'),
    ),
    h('textarea', { value: code, onChange: e => setCode(e.target.value), rows: 5, className: 'form-control form-control-mono' }),
    h('div', { style: { display: 'flex', gap: '8px', marginTop: '8px' } },
      h('button', { onClick: handleRun, disabled: running, className: 'btn btn-primary btn-sm' }, running ? 'Running...' : 'Run'),
    ),
    result && h('div', { style: { marginTop: '8px', padding: '8px', borderRadius: 'var(--radius-sm)', background: 'var(--color-canvas-inset)', border: '1px solid var(--color-border-default)', fontFamily: 'var(--font-mono)', fontSize: '12px', whiteSpace: 'pre-wrap', maxHeight: '200px', overflow: 'auto' } },
      result.timed_out && h('div', { style: { color: 'var(--color-danger-fg)', marginBottom: '4px' } }, '[timed out]'),
      h('div', { style: { color: 'var(--color-fg-muted)', marginBottom: '2px' } }, 'exit_code: ' + result.exit_code),
      result.stdout && h('div', null, result.stdout),
      result.stderr && h('div', { style: { color: 'var(--color-danger-fg)' } }, result.stderr),
    ),
  );
}

function sandboxSummary(s) {
  const parts = [];
  if (s.type === 'docker') {
    parts.push(s.host || 'local socket');
    if (s.image) parts.push(s.image);
  }
  return parts.join(' · ') || 'default settings';
}

export function SandboxPanel() {
  const { data: sandboxes, reload } = useApi(() => api.sandboxes.list());
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState(null);
  const [testing, setTesting] = useState(null);

  const handleSave = async (form) => {
    if (editing) { await api.sandboxes.update(editing.id, form); }
    else { await api.sandboxes.create(form); }
    setEditing(null); setAdding(false); reload();
  };
  const handleDelete = async (id) => { await api.sandboxes.delete(id); reload(); };

  const typeLabel = (t) => t === 'local' ? 'Local' : 'Docker';
  const typeClass = (t) => t === 'docker' ? 'Label-accent' : 'Label-default';

  return h('div', null,
    h('div', { className: 'SectionHeader' },
      h('h2', { className: 'SectionHeader-title' }, 'Sandbox Environments'),
      !adding && !editing && h('button', { onClick: () => { setAdding(true); setTesting(null); }, className: 'btn btn-primary btn-sm' }, '+ Add'),
    ),
    adding && h(SandboxForm, { onSave: handleSave, onCancel: () => setAdding(false) }),
    editing && h(SandboxForm, { initial: editing, onSave: handleSave, onCancel: () => setEditing(null) }),
    testing && h(ExecPanel, { sandbox: testing, onClose: () => setTesting(null) }),
    h('div', { className: 'Box' },
      sandboxes && sandboxes.map(s =>
        h('div', { key: s.id, className: 'Box-row' },
          h('div', { style: { flex: 1, minWidth: 0 } },
            h('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } },
              h('span', { className: 'Label ' + typeClass(s.type) }, typeLabel(s.type)),
              h('span', { style: { fontWeight: 500, fontSize: '14px' } }, s.name),
            ),
            h('div', { style: { fontSize: '12px', color: 'var(--color-fg-muted)', marginTop: '4px' } }, sandboxSummary(s)),
          ),
          h('div', { style: { display: 'flex', gap: '6px', flexShrink: 0, alignItems: 'center' } },
            h('button', { onClick: () => { setAdding(false); setEditing(null); setTesting(s); }, className: 'btn btn-sm', style: { color: 'var(--color-success-fg)' } }, 'Test'),
            h('button', { onClick: () => { setAdding(false); setTesting(null); setEditing(s); }, className: 'btn btn-sm btn-invisible' }, 'Edit'),
            h('button', { onClick: () => handleDelete(s.id), className: 'btn btn-sm btn-danger' }, 'Delete'),
          ),
        ),
      ),
      (!sandboxes || sandboxes.length === 0) && !adding && h('div', { className: 'blankslate' }, 'No sandbox environments configured.'),
    ),
  );
}

function fc(label, input, suffix) {
  return h('div', { className: 'FormControl' },
    label && h('label', { className: 'FormControl-label' }, label),
    suffix ? h('div', { style: { display: 'flex', alignItems: 'center', gap: '6px' } }, input, h('span', { style: { fontSize: '12px', color: 'var(--color-fg-muted)' } }, suffix)) : input,
  );
}
