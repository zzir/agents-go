import React from 'react';
import { api } from '/lib/api.js';
import { useApi } from '/lib/hooks.js';

const { useState } = React;
const h = React.createElement;

function AgentForm({ initial, onSave, onCancel, mcpServers }) {
  const initTools = () => {
    try { return JSON.parse((initial && initial.tools) || '[]'); } catch { return []; }
  };
  const [form, setForm] = useState(initial || {
    name: '', instructions: '', model: 'gpt-4o',
    provider_type: '', api_key: '', base_url: '',
    max_turns: 0, handoff_description: '',
    disable_tool_choice_reset: false, tool_use_behavior: '',
    retry_enabled: false, retry_policy: '',
    fallback_models: '',
    input_guardrails: '', output_guardrails: '', output_schema: '',
    use_previous_response_id: false,
    prompt_id: '', prompt_version: '',
    handoff_input_filter: '', max_tool_concurrency: 0,
    tool_not_found_behavior: '',
  });
  const [selectedMcp, setSelectedMcp] = useState(initTools);
  const [showAdvanced, setShowAdvanced] = useState(false);
  const set = (k, v) => setForm(prev => ({ ...prev, [k]: v }));
  const toggleMcp = (id) => {
    setSelectedMcp(prev => prev.includes(id) ? prev.filter(x => x !== id) : [...prev, id]);
  };

  return h('div', { className: 'form-box' },
    fc('Name', h('input', { value: form.name, onChange: e => set('name', e.target.value), placeholder: 'e.g. Code Assistant', className: 'form-control' })),
    fc('Model', h('input', { value: form.model, onChange: e => set('model', e.target.value), placeholder: 'gpt-4o', className: 'form-control' })),

    h('div', { className: 'divider' }),
    h('div', { style: { fontSize: '12px', fontWeight: 600, color: 'var(--color-fg-muted)', textTransform: 'uppercase', letterSpacing: '0.5px', marginBottom: '8px' } }, 'Provider'),
    fc('API Key', h('input', { value: form.api_key, onChange: e => set('api_key', e.target.value), placeholder: 'sk-...', type: 'password', className: 'form-control' })),
    fc('Base URL', h('input', { value: form.base_url, onChange: e => set('base_url', e.target.value), placeholder: 'https://api.openai.com/v1 (leave empty for default)', className: 'form-control' })),

    h('div', { className: 'divider' }),
    fc('Instructions', h('textarea', { value: form.instructions, onChange: e => set('instructions', e.target.value), rows: 5, placeholder: 'System prompt / instructions for this agent...', className: 'form-control form-control-mono' })),

    mcpServers && mcpServers.length > 0 && h('div', null,
      h('div', { className: 'divider' }),
      h('div', { style: { fontSize: '12px', fontWeight: 600, color: 'var(--color-fg-muted)', textTransform: 'uppercase', letterSpacing: '0.5px', marginBottom: '8px' } }, 'MCP Servers'),
      mcpServers.map(s =>
        h('label', { key: s.id, className: 'form-checkbox', style: { display: 'flex', alignItems: 'center', gap: '6px' } },
          h('input', { type: 'checkbox', checked: selectedMcp.includes(s.id), onChange: () => toggleMcp(s.id) }),
          h('span', null, s.name),
          s.connected && h('span', { style: { width: 6, height: 6, borderRadius: '50%', background: 'var(--color-success-fg)', display: 'inline-block', marginLeft: '4px' } }),
        ),
      ),
      h('div', { className: 'FormControl-caption' }, 'Select which MCP servers this agent can use'),
    ),

    h('div', { className: 'advanced-toggle', onClick: () => setShowAdvanced(!showAdvanced) },
      (showAdvanced ? '▾' : '▸') + ' Advanced',
    ),

    showAdvanced && h('div', { className: 'advanced-section' },
      fc('Max Turns', h('input', { type: 'number', min: 0, value: form.max_turns || 0, onChange: e => set('max_turns', parseInt(e.target.value) || 0), className: 'form-control', style: { width: '100px' } }), '0 = SDK default (10)'),
      fc('Handoff Description', h('input', { value: form.handoff_description || '', onChange: e => set('handoff_description', e.target.value), placeholder: 'Description when this agent is a handoff target', className: 'form-control' })),
      fc('Tool Use Behavior', h('select', { value: form.tool_use_behavior || '', onChange: e => set('tool_use_behavior', e.target.value), className: 'form-select', style: { width: 'auto' } },
        h('option', { value: '' }, 'Run LLM Again (default)'),
        h('option', { value: 'stop_on_first' }, 'Stop on First Tool'),
        h('option', { value: 'stop_at:' }, 'Stop at Specific Tools'),
      )),
      form.tool_use_behavior && form.tool_use_behavior.startsWith('stop_at') &&
        fc('Stop At Tool Names', h('input', { value: (form.tool_use_behavior || '').replace('stop_at:', ''), onChange: e => set('tool_use_behavior', 'stop_at:' + e.target.value), placeholder: 'tool1, tool2', className: 'form-control' })),

      h('label', { className: 'form-checkbox', style: { marginTop: '12px' } },
        h('input', { type: 'checkbox', checked: form.retry_enabled || false, onChange: e => set('retry_enabled', e.target.checked) }),
        'Enable Retry',
      ),
      form.retry_enabled && h('div', { style: { marginTop: '6px', marginLeft: '20px' } },
        fc('Retry Policy (JSON)', h('input', { value: form.retry_policy || '', onChange: e => set('retry_policy', e.target.value), placeholder: '{"max_attempts":3,"base_delay_ms":500,"max_delay_ms":30000,"multiplier":2}', className: 'form-control form-control-mono' }), 'Empty = SDK defaults'),
      ),

      fc('Fallback Models (JSON)', h('input', { value: form.fallback_models || '', onChange: e => set('fallback_models', e.target.value), placeholder: '[{"model":"gpt-4o-mini","api_key":"sk-...","base_url":""}]', className: 'form-control form-control-mono', style: { marginTop: '8px' } }), 'JSON array of {model, api_key, base_url}'),

      h('div', { className: 'divider' }),
      fc('Input Guardrails (JSON)', h('input', { value: form.input_guardrails || '', onChange: e => set('input_guardrails', e.target.value), placeholder: '["content_filter","max_input_length"]', className: 'form-control form-control-mono' }), 'JSON array of guardrail names'),
      fc('Output Guardrails (JSON)', h('input', { value: form.output_guardrails || '', onChange: e => set('output_guardrails', e.target.value), placeholder: '["max_output_length"]', className: 'form-control form-control-mono' }), 'JSON array of guardrail names'),
      fc('Output Schema (JSON Schema)', h('textarea', { value: form.output_schema || '', onChange: e => set('output_schema', e.target.value), rows: 3, placeholder: '{"type":"object","properties":{...},"required":[...]}', className: 'form-control form-control-mono' }), 'Structured output JSON Schema — leave empty for plain text'),

      h('div', { className: 'divider' }),
      h('label', { className: 'form-checkbox' },
        h('input', { type: 'checkbox', checked: form.disable_tool_choice_reset || false, onChange: e => set('disable_tool_choice_reset', e.target.checked) }),
        'Disable Tool Choice Reset',
      ),
      h('label', { className: 'form-checkbox', style: { marginTop: '8px' } },
        h('input', { type: 'checkbox', checked: form.use_previous_response_id || false, onChange: e => set('use_previous_response_id', e.target.checked) }),
        'Use Previous Response ID (server-side state)',
      ),

      h('div', { className: 'divider' }),
      fc('Stored Prompt ID', h('input', { value: form.prompt_id || '', onChange: e => set('prompt_id', e.target.value), placeholder: 'prompt_abc123', className: 'form-control' }), 'OpenAI stored prompt ID'),
      form.prompt_id && fc('Prompt Version', h('input', { value: form.prompt_version || '', onChange: e => set('prompt_version', e.target.value), placeholder: 'Optional version pin', className: 'form-control' })),

      h('div', { className: 'divider' }),
      fc('Handoff Input Filter', h('select', { value: form.handoff_input_filter || '', onChange: e => set('handoff_input_filter', e.target.value), className: 'form-select', style: { width: 'auto' } },
        h('option', { value: '' }, 'None (default)'),
        h('option', { value: 'nest_history' }, 'Nest Handoff History'),
      )),
      fc('Max Tool Concurrency', h('input', { type: 'number', min: 0, value: form.max_tool_concurrency || 0, onChange: e => set('max_tool_concurrency', parseInt(e.target.value) || 0), className: 'form-control', style: { width: '100px' } }), '0 = unlimited'),
      fc('Tool Not Found Behavior', h('select', { value: form.tool_not_found_behavior || '', onChange: e => set('tool_not_found_behavior', e.target.value), className: 'form-select', style: { width: 'auto' } },
        h('option', { value: '' }, 'Error (default)'),
        h('option', { value: 'return_to_model' }, 'Return to Model'),
      )),
    ),

    h('div', { style: { display: 'flex', gap: '8px', marginTop: '16px' } },
      h('button', { onClick: () => onSave({ ...form, tools: JSON.stringify(selectedMcp) }), className: 'btn btn-primary' }, 'Save'),
      onCancel && h('button', { onClick: onCancel, className: 'btn' }, 'Cancel'),
    ),
  );
}

export function AgentConfigPanel() {
  const { data: agents, reload } = useApi(() => api.agents.list());
  const { data: mcpServers } = useApi(() => api.mcpServers.list());
  const [editing, setEditing] = useState(null);
  const [adding, setAdding] = useState(false);

  const handleSave = async (form) => {
    if (editing) { await api.agents.update(editing.id, form); }
    else { await api.agents.create(form); }
    setEditing(null);
    setAdding(false);
    reload();
  };

  const handleDelete = async (id) => {
    await api.agents.delete(id);
    reload();
  };

  return h('div', null,
    h('div', { className: 'SectionHeader' },
      h('h2', { className: 'SectionHeader-title' }, 'Agents'),
      !adding && h('button', { onClick: () => setAdding(true), className: 'btn btn-primary btn-sm' }, '+ Add'),
    ),

    adding && h(AgentForm, { onSave: handleSave, onCancel: () => setAdding(false), mcpServers }),
    editing && h(AgentForm, { initial: editing, onSave: handleSave, onCancel: () => setEditing(null), mcpServers }),

    h('div', { className: 'Box' },
      agents && agents.map(a =>
        h('div', { key: a.id, className: 'Box-row' },
          h('div', { style: { flex: 1, minWidth: 0 } },
            h('div', { style: { fontWeight: 500, fontSize: '14px' } }, a.name),
            h('div', { style: { fontSize: '12px', color: 'var(--color-fg-muted)', marginTop: '2px' } },
              [a.model || 'default model', a.base_url && ('@ ' + a.base_url)].filter(Boolean).join(' '),
            ),
            a.instructions && h('div', { style: { fontSize: '11px', color: 'var(--color-fg-subtle)', marginTop: '4px', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } },
              a.instructions.substring(0, 80) + (a.instructions.length > 80 ? '...' : ''),
            ),
            (() => {
              try {
                const ids = JSON.parse(a.tools || '[]');
                if (!ids.length || !mcpServers) return null;
                const names = ids.map(id => (mcpServers.find(s => s.id === id) || {}).name).filter(Boolean);
                if (!names.length) return null;
                return h('div', { style: { fontSize: '11px', color: 'var(--color-fg-muted)', marginTop: '3px' } },
                  'MCP: ' + names.join(', '));
              } catch { return null; }
            })(),
          ),
          h('div', { style: { display: 'flex', gap: '6px', flexShrink: 0 } },
            h('button', { onClick: () => { setAdding(false); setEditing(a); }, className: 'btn btn-sm btn-invisible' }, 'Edit'),
            h('button', { onClick: () => handleDelete(a.id), className: 'btn btn-sm btn-danger' }, 'Delete'),
          ),
        ),
      ),
      (!agents || agents.length === 0) && !adding && h('div', { className: 'blankslate' },
        'No agents configured. Add one to customize model, provider, and behavior.',
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
