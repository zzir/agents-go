import React, { useState, useEffect, useRef, useCallback } from 'react';
import { Button, TextInput, Textarea, Label, FormControl, Checkbox, SegmentedControl, Stack, PageHeader } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { api } from '@/lib/api';
import { useApi, useCrud } from '@/lib/hooks';
import { fc, seg } from '@/lib/form';
import { JsonField } from '@/lib/JsonField';
import { toast } from '@/lib/toast';
import { ChevronRightIcon } from '@primer/octicons-react';
import { type Skill, type SkillGroup, groupByRepo } from '@/lib/skills';
import { PROVIDERS, providerMeta, providerFacts, type ProviderTypeInfo } from '@/lib/providers';

// The agent-config REST payload nests these scalar settings under JSON group
// objects. The form state stays flat, so flattenConfig lifts a loaded config's
// group keys to the top level and nestConfig folds them back before saving.
const CONFIG_GROUPS: Record<string, string[]> = {
  provider: ['provider_type', 'auth_mode', 'api_key', 'base_url', 'context_window'],
  behavior: ['max_turns', 'handoff_description', 'disable_tool_choice_reset', 'stop_at_tools', 'handoff_input_filter', 'max_tool_concurrency', 'tool_not_found_behavior', 'reasoning_item_id_policy', 'plan_mode', 'todo_list'],
  resilience: ['retry_enabled', 'retry_policy', 'fallback_models'],
  guardrails: ['guardrails', 'output_schema'],
  session: ['prompt_id', 'prompt_version', 'history_limit'],
  approval: ['approve_tools'],
  compaction: ['compaction_enabled', 'compaction_threshold_tokens', 'compaction_window', 'compaction_model', 'compaction_prompt'],
};

function flattenConfig(c: Record<string, unknown> | undefined): Record<string, unknown> {
  if (!c) return {};
  const out: Record<string, unknown> = { ...c };
  for (const [group, keys] of Object.entries(CONFIG_GROUPS)) {
    const g = c[group] as Record<string, unknown> | undefined;
    delete out[group];
    if (g && typeof g === 'object') for (const k of keys) if (g[k] !== undefined) out[k] = g[k];
  }
  return out;
}

function nestConfig(flat: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = { ...flat };
  for (const [group, keys] of Object.entries(CONFIG_GROUPS)) {
    const g: Record<string, unknown> = {};
    for (const k of keys) if (flat[k] !== undefined) { g[k] = flat[k]; delete out[k]; }
    out[group] = g;
  }
  return out;
}

interface AgentFormData {
  name: string;
  instructions: string;
  model: string;
  provider_type: string;
  auth_mode: string;
  api_key: string;
  base_url: string;
  context_window: number;
  max_turns: number;
  handoff_description: string;
  disable_tool_choice_reset: boolean;
  plan_mode: boolean;
  todo_list: boolean;
  stop_at_tools: string;
  retry_enabled: boolean;
  retry_policy: string;
  fallback_models: string;
  guardrails: string;
  output_schema: string;
  error_handlers: string;
  prompt_id: string;
  prompt_version: string;
  history_limit: number;
  handoff_input_filter: string;
  max_tool_concurrency: number;
  tool_not_found_behavior: string;
  reasoning_item_id_policy: string;
  approve_tools: string;
  compaction_enabled: boolean;
  compaction_threshold_tokens: number;
  compaction_window: number;
  compaction_model: string;
  compaction_prompt: string;
  handoffs?: string;
  tools?: string;
  skills?: string;
  model_settings?: string;
}

interface McpServer {
  id: string | number;
  name: string;
  status?: string;
}

interface Agent {
  id: string | number;
  name: string;
  model: string;
  // Provider settings are nested under the provider group in the API response.
  provider?: { provider_type?: string; base_url?: string; auth_mode?: string };
  instructions: string;
  handoffs: string;
  tools: string;
  // Empty/absent means "not customized" -> the agent gets every installed skill.
  skills?: string;
  // Derived login signal from the backend; the token itself never reaches the API.
  chatgpt_logged_in?: boolean;
}

interface AgentFormProps {
  initial?: Partial<AgentFormData> & { id?: string | number };
  onSave: (form: AgentFormData & { handoffs: string; tools: string; skills: string; model_settings: string }) => void;
  onCancel?: () => void;
  onDelete?: () => void;
  mcpServers?: McpServer[];
  skills?: Skill[];
  allAgents?: Agent[];
  providerTypes?: ProviderTypeInfo[];
}

function AgentForm({ initial, onSave, onCancel, onDelete, mcpServers, skills, allAgents, providerTypes }: AgentFormProps) {
  const initHandoffs = (): (string | number)[] => {
    try { return JSON.parse((initial && initial.handoffs) || '[]'); } catch { return []; }
  };
  const initTools = (): (string | number)[] => {
    try { return JSON.parse((initial && initial.tools) || '[]'); } catch { return []; }
  };
  // undefined/absent `skills` means this agent predates per-agent skill
  // scoping (or is brand new) — null here signals "not customized yet", so
  // the effective set below defaults to every currently installed skill
  // instead of none, preserving the old "every agent gets every skill" behavior.
  const initSkills = (): string[] | null => {
    if (!initial || typeof initial.skills !== 'string' || initial.skills === '') return null;
    try { return JSON.parse(initial.skills); } catch { return null; }
  };
  const parseModelSettings = (): Record<string, unknown> => {
    try { return JSON.parse((initial && initial.model_settings) || '{}'); } catch { return {}; }
  };
  const initMs = parseModelSettings() as { reasoning?: { effort?: string }; service_tier?: string; extra_body?: Record<string, unknown>; temperature?: number; top_p?: number; max_tokens?: number };
  const [form, setForm] = useState<AgentFormData>({
    name: '', instructions: '', model: 'gpt-5.5',
    provider_type: '', auth_mode: '', api_key: '', base_url: '', context_window: 0,
    max_turns: 0, handoff_description: '',
    disable_tool_choice_reset: false, plan_mode: false, todo_list: false, stop_at_tools: '',
    retry_enabled: false, retry_policy: '',
    fallback_models: '',
    guardrails: '', output_schema: '', error_handlers: '',
    prompt_id: '', prompt_version: '', history_limit: 0,
    handoff_input_filter: '', max_tool_concurrency: 0,
    tool_not_found_behavior: '', reasoning_item_id_policy: '', approve_tools: '',
    compaction_enabled: false, compaction_threshold_tokens: 0,
    compaction_window: 0, compaction_model: '', compaction_prompt: '',
    ...flattenConfig(initial as Record<string, unknown> | undefined),
  });
  const [reasoningEffort, setReasoningEffort] = useState(initMs.reasoning?.effort || '');
  const [serviceTier, setServiceTier] = useState(initMs.service_tier || '');
  const [extraBody, setExtraBody] = useState(initMs.extra_body ? JSON.stringify(initMs.extra_body) : '');
  const [temperature, setTemperature] = useState(initMs.temperature !== undefined ? String(initMs.temperature) : '');
  const [topP, setTopP] = useState(initMs.top_p !== undefined ? String(initMs.top_p) : '');
  const [maxTokens, setMaxTokens] = useState(initMs.max_tokens !== undefined ? String(initMs.max_tokens) : '');
  // model_settings keys the form has no controls for (prompt_cache_options,
  // verbosity, metadata, …) can be set through the API. The save handler
  // rebuilds model_settings from the form, so anything not carried over here
  // would be silently dropped on the next UI save.
  const msFormKeys = ['reasoning', 'service_tier', 'extra_body', 'temperature', 'top_p', 'max_tokens'];
  const preservedMs = Object.fromEntries(Object.entries(parseModelSettings()).filter(([k]) => !msFormKeys.includes(k)));
  const [selectedHandoffs, setSelectedHandoffs] = useState<(string | number)[]>(initHandoffs);
  const [selectedMcp, setSelectedMcp] = useState<(string | number)[]>(initTools);
  const [selectedSkills, setSelectedSkills] = useState<string[] | null>(initSkills);
  const [showAdvanced, setShowAdvanced] = useState(false);
  const set = <K extends keyof AgentFormData>(k: K, v: AgentFormData[K]) => setForm(prev => ({ ...prev, [k]: v }));
  // Wording comes from the static table; machine facts (auth modes,
  // unsupported features) come from the server's provider registry, with a
  // static assumption as the pre-fetch fallback so controls don't flicker.
  const meta = providerMeta(form.provider_type);
  const authModesFor = (value: string | undefined): string[] =>
    providerFacts(providerTypes, value)?.auth_modes ?? (providerMeta(value).type === 'openai' ? ['chatgpt_login'] : []);
  const supportsChatGPT = authModesFor(form.provider_type).includes('chatgpt_login');
  const unsupported = providerFacts(providerTypes, form.provider_type)?.unsupported ?? [];
  // A masked key or a base URL saved under a DIFFERENT backend is the trap to
  // warn about: the field looks configured, but the credential belongs to the
  // provider this agent was switched away from.
  // The loaded config is the NESTED REST shape — provider_type lives under
  // the provider group, so it must be read through flattenConfig, and both
  // sides go through providerMeta so an explicitly stored "openai" compares
  // equal to the form's '' default.
  const initialProvider = (flattenConfig(initial as Record<string, unknown> | undefined).provider_type as string) || '';
  const providerChanged = initial !== undefined && providerMeta(form.provider_type).type !== providerMeta(initialProvider).type;
  const staleKeyHint = providerChanged && form.api_key === '********'
    ? 'This stored key was saved for the previously selected provider — replace it, clear it, or switch back'
    : 'Stored keys show as ******** — leave the mask to keep the current key, clear the field to remove it';
  const unsupportedHint = unsupported.length > 0
    ? `Fail loudly on this provider — leave unset: ${unsupported.slice(0, 6).join(', ')}${unsupported.length > 6 ? ` +${unsupported.length - 6} more` : ''}`
    : 'Which backend this agent calls';
  const handoffTargets = (allAgents || []).filter(a => a.id !== initial?.id);
  const toggleHandoff = (id: string | number) => {
    setSelectedHandoffs(prev => prev.includes(id) ? prev.filter(x => x !== id) : [...prev, id]);
  };
  const toggleMcp = (id: string | number) => {
    setSelectedMcp(prev => prev.includes(id) ? prev.filter(x => x !== id) : [...prev, id]);
  };
  // null selectedSkills = not customized yet -> effectively "every installed skill".
  // Computed from the live `skills` prop (not stale state) so it's correct even
  // before any effect/interaction has run.
  const allSkillPaths = (skills || []).map(sk => sk.path);
  const effectiveSkills = selectedSkills ?? allSkillPaths;
  const toggleSkill = (path: string) => {
    setSelectedSkills(prev => {
      const base = prev ?? allSkillPaths;
      return base.includes(path) ? base.filter(x => x !== path) : [...base, path];
    });
  };
  // Skills are grouped by their top-level directory (a cloned repo can bundle
  // dozens of skills) so the list stays manageable — collapsed by default,
  // with a group-level checkbox to select/deselect the whole directory at once.
  const skillGroups = groupByRepo(skills || []);
  const [expandedSkillRepos, setExpandedSkillRepos] = useState<Set<string>>(new Set());
  const toggleSkillRepoExpanded = (repo: string) => {
    setExpandedSkillRepos(prev => {
      const next = new Set(prev);
      if (next.has(repo)) next.delete(repo); else next.add(repo);
      return next;
    });
  };
  const toggleSkillGroup = (group: SkillGroup) => {
    const paths = group.skills.map(sk => sk.path);
    const allSelected = paths.every(p => effectiveSkills.includes(p));
    setSelectedSkills(prev => {
      const base = prev ?? allSkillPaths;
      return allSelected ? base.filter(p => !paths.includes(p)) : Array.from(new Set([...base, ...paths]));
    });
  };

  return (
    <Stack gap="normal">
      {fc('Name', <TextInput value={form.name} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('name', e.target.value)} placeholder="e.g. Code Assistant" block />)}

      {/* The backend comes first: it decides the model placeholder, the auth
          modes, and which of the settings below even apply. */}
      <div className="form-group">
        <div className="form-group-title">Provider</div>
        {seg('Backend', meta.value, PROVIDERS.map(p => [p.value, p.label] as const), v => {
          // An auth mode the new backend doesn't offer is cleared so the save
          // is not rejected for a control that is no longer shown. An
          // untouched default model name follows the backend, a customized
          // one is the user's and stays.
          setForm(prev => {
            const next = { ...prev, provider_type: v };
            if (prev.auth_mode && !authModesFor(v).includes(prev.auth_mode)) next.auth_mode = '';
            if (prev.model === providerMeta(prev.provider_type).defaultModel) next.model = providerMeta(v).defaultModel;
            return next;
          });
        }, unsupportedHint)}

        {supportsChatGPT && fc('Auth mode', <SegmentedControl aria-label="Auth mode" size="small">
          <SegmentedControl.Button
            selected={form.auth_mode !== 'chatgpt_login'}
            onClick={() => set('auth_mode', '')}
          >API Key</SegmentedControl.Button>
          <SegmentedControl.Button
            selected={form.auth_mode === 'chatgpt_login'}
            onClick={() => set('auth_mode', 'chatgpt_login')}
          >ChatGPT Subscribe</SegmentedControl.Button>
        </SegmentedControl>, 'Choose authentication method')}

        {(!supportsChatGPT || form.auth_mode !== 'chatgpt_login') && <>
          {fc('API key', <TextInput value={form.api_key} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('api_key', e.target.value)} placeholder={meta.keyPlaceholder} type="password" block />, staleKeyHint)}
          {fc('Base URL', <TextInput value={form.base_url} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('base_url', e.target.value)} placeholder={meta.baseURLPlaceholder} block />, providerChanged && form.base_url ? 'Saved for the previously selected provider — make sure it applies to this backend' : undefined)}
        </>}

        {supportsChatGPT && form.auth_mode === 'chatgpt_login' &&
          fc('Base URL override', <TextInput value={form.base_url} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('base_url', e.target.value)} placeholder="Leave empty for ChatGPT default" block />, 'Only change if you know what you\'re doing')
        }
      </div>

      <div className="form-group">
        <div className="form-group-title">Model</div>
        {fc('Model', <TextInput value={form.model} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('model', e.target.value)} placeholder={meta.modelPlaceholder} block />)}
        {fc('Context window',
          <TextInput block type="number" min={0} step={1000} value={String(form.context_window || 0)}
            onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('context_window', parseInt(e.target.value) || 0)} />,
          'Tokens this model accepts — the Context panel needs it to show how full the window is (0 = unknown, no provider reports it)')}
        <div style={{ display: 'flex', gap: 12 }}>
          <div style={{ flex: 1 }}>
            {fc('Temperature', <TextInput type="number" step={0.1} min={0} max={2} value={temperature} onChange={(e: React.ChangeEvent<HTMLInputElement>) => setTemperature(e.target.value)} block />)}
          </div>
          <div style={{ flex: 1 }}>
            {fc('Top-p', <TextInput type="number" step={0.05} min={0} max={1} value={topP} onChange={(e: React.ChangeEvent<HTMLInputElement>) => setTopP(e.target.value)} block />)}
          </div>
          <div style={{ flex: 1 }}>
            {fc('Max tokens', <TextInput type="number" step={1} min={1} value={maxTokens} onChange={(e: React.ChangeEvent<HTMLInputElement>) => setMaxTokens(e.target.value)} block />)}
          </div>
        </div>
        {/* A stored effort the current backend doesn't offer stays visible as
            its own extra option — never a hidden value the form would drop. */}
        {seg('Reasoning effort',
          reasoningEffort,
          meta.effortOptions.some(([v]) => v === reasoningEffort) ? meta.effortOptions : [...meta.effortOptions, [reasoningEffort, reasoningEffort] as const],
          setReasoningEffort, meta.effortHint)}
        {/* Hidden when the backend has no service tiers AND nothing is stored:
            a control that can only produce a failing value is noise. A stored
            value stays visible with its warning, so it is never a hidden trap. */}
        {(serviceTier !== '' || !unsupported.includes('service_tier')) &&
          seg('Service tier', serviceTier, [['', 'Not set'], ['auto', 'Auto'], ['default', 'Default'], ['flex', 'Flex'], ['priority', 'Priority']], setServiceTier,
            unsupported.includes('service_tier') ? 'Not supported by this provider — a set value fails runs' : undefined)}
        <JsonField label="Extra body (JSON)" value={extraBody} onChange={setExtraBody} placeholder='{"enable_thinking": true, "thinking_budget": 1024}' caption="Provider-specific parameters injected into every API request" />
        {Object.keys(preservedMs).length > 0 && (
          <span style={{ color: 'var(--fgColor-muted)', fontSize: 'var(--text-body-size-small)' }}>
            Set via API, preserved on save: {Object.keys(preservedMs).sort().join(', ')}
          </span>
        )}
      </div>

      <div className="form-group">
        <div className="form-group-title">Instructions</div>
        {fc('Instructions', <Textarea value={form.instructions} onChange={(e: React.ChangeEvent<HTMLTextAreaElement>) => set('instructions', e.target.value)} rows={5} placeholder="System prompt / instructions for this agent..." block style={{ fontFamily: 'var(--fontStack-monospace)' }} />, null, { hideLabel: true })}
      </div>

      <div className="form-group">
        <div className="form-group-title">Workflow</div>
        <FormControl>
          <Checkbox checked={form.plan_mode || false} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('plan_mode', e.target.checked)} />
          <FormControl.Label>Plan mode</FormControl.Label>
          <FormControl.Caption>Each run starts read-only: the agent explores, submits a plan for your approval, and only an approved plan unlocks the full toolset</FormControl.Caption>
        </FormControl>
        <FormControl>
          <Checkbox checked={form.todo_list || false} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('todo_list', e.target.checked)} />
          <FormControl.Label>Todo list</FormControl.Label>
          <FormControl.Caption>The agent keeps a working todo list; the chat renders it as a live checklist</FormControl.Caption>
        </FormControl>
      </div>

      {mcpServers && mcpServers.length > 0 && <div className="form-group">
        <div className="form-group-title">MCP Servers</div>
        <div className="form-checkbox-group">
          {mcpServers.map(s => {
            const usable = s.status === 'connected';
            return (
              <FormControl key={s.id} disabled={!usable}>
                <Checkbox checked={selectedMcp.includes(s.id)} disabled={!usable} onChange={() => toggleMcp(s.id)} />
                <FormControl.Label>
                  {s.name}
                  {usable
                    ? <span className="form-status-dot form-status-dot--success" style={{ width: 6, height: 6, marginLeft: 4, display: 'inline-block' }} />
                    : <span className="resource-row-sub" style={{ marginLeft: 4 }}>({s.status === 'disabled' ? 'disabled' : 'not connected'})</span>}
                </FormControl.Label>
              </FormControl>
            );
          })}
        </div>
        <div className="FormControl-caption">Select which MCP servers this agent can use — greyed-out servers are disabled or not currently connected</div>
      </div>}

      {skills && skills.length > 0 && <div className="form-group">
        <div className="form-group-title">Skills</div>
        <div className="form-checkbox-group">
          {skillGroups.map(group => {
            const paths = group.skills.map(sk => sk.path);
            const selectedCount = paths.filter(p => effectiveSkills.includes(p)).length;
            const allSelected = selectedCount === paths.length;
            const expanded = expandedSkillRepos.has(group.repo);
            return (
              <div key={group.repo}>
                <div className="checkbox-group-header">
                  <Checkbox
                    checked={allSelected}
                    indeterminate={!allSelected && selectedCount > 0}
                    aria-label={`Select all skills in ${group.repo}`}
                    onChange={() => toggleSkillGroup(group)}
                  />
                  <button type="button" className="checkbox-group-toggle" aria-expanded={expanded} onClick={() => toggleSkillRepoExpanded(group.repo)}>
                    <ChevronRightIcon size={12} />
                    {group.repo}
                  </button>
                  <span className="checkbox-group-header-count">{selectedCount}/{paths.length}</span>
                </div>
                {expanded && (
                  <div className="checkbox-group-body">
                    {group.skills.map(sk => (
                      <FormControl key={sk.path}>
                        <Checkbox checked={effectiveSkills.includes(sk.path)} onChange={() => toggleSkill(sk.path)} />
                        <FormControl.Label>{sk.name}</FormControl.Label>
                        {sk.description && <FormControl.Caption>{sk.description}</FormControl.Caption>}
                      </FormControl>
                    ))}
                  </div>
                )}
              </div>
            );
          })}
        </div>
        <div className="FormControl-caption">Select which installed skills this agent can use</div>
      </div>}

      <div className="form-group">
        <div className="form-group-title">Handoffs</div>
        {handoffTargets.length > 0 && <>
          <div className="form-checkbox-group">
            {handoffTargets.map(a => (
              <FormControl key={a.id}>
                <Checkbox checked={selectedHandoffs.includes(a.id)} onChange={() => toggleHandoff(a.id)} />
                <FormControl.Label>{a.name}</FormControl.Label>
                <FormControl.Caption>{a.model || 'default model'}</FormControl.Caption>
              </FormControl>
            ))}
          </div>
          <div className="FormControl-caption">Select which agents this agent can hand off to</div>
        </>}
        {handoffTargets.length === 0 && <div className="FormControl-caption">Create other agents to enable handoffs</div>}
        {seg('Handoff input filter', form.handoff_input_filter || '', [['', 'None (default)'], ['nest_history', 'Nest handoff history']], v => set('handoff_input_filter', v))}
        {fc('Handoff description', <TextInput value={form.handoff_description || ''} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('handoff_description', e.target.value)} placeholder="Description when this agent is a handoff target" block />)}
      </div>

      <div className="form-group">
        <div className="form-group-title">Compaction</div>
        <FormControl>
          <Checkbox checked={form.compaction_enabled || false} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('compaction_enabled', e.target.checked)} />
          <FormControl.Label>Enable compaction</FormControl.Label>
          <FormControl.Caption>Summarize old messages when history grows large (provider-agnostic)</FormControl.Caption>
        </FormControl>
        {form.compaction_enabled && <>
          {fc('Threshold (tokens)', <TextInput block type="number" min={0} value={String(form.compaction_threshold_tokens || 0)} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('compaction_threshold_tokens', parseInt(e.target.value) || 0)} />, 'Token count that triggers compaction (0 = default 50000); sized from real usage, byte-estimated where unmeasured')}
          {fc('Window size', <TextInput block type="number" min={0} value={String(form.compaction_window || 0)} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('compaction_window', parseInt(e.target.value) || 0)} />, 'Recent items to keep intact (0 = default 10)')}
          {fc('Summary model', <TextInput value={form.compaction_model || ''} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('compaction_model', e.target.value)} placeholder="e.g. gpt-4.1-mini" block />, "Model used to generate conversation summaries (empty = the agent's model)")}
          {fc('Summary prompt', <Textarea value={form.compaction_prompt || ''} onChange={(e: React.ChangeEvent<HTMLTextAreaElement>) => set('compaction_prompt', e.target.value)} rows={3} placeholder="Custom summarization instructions (leave empty for default)" block style={{ fontFamily: 'var(--fontStack-monospace)' }} />)}
        </>}
      </div>

      <button type="button" className="advanced-toggle" aria-expanded={showAdvanced} onClick={() => setShowAdvanced(!showAdvanced)}>
        <ChevronRightIcon size={12} />
        Advanced
      </button>

      {showAdvanced && <div className="advanced-section">
        <div className="form-group">
          <div className="form-group-title">Behavior</div>
          {fc('Max turns', <TextInput block type="number" min={0} value={String(form.max_turns || 0)} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('max_turns', parseInt(e.target.value) || 0)} />, '0 = SDK default (10)')}
          {fc('Max tool concurrency', <TextInput block type="number" min={0} value={String(form.max_tool_concurrency || 0)} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('max_tool_concurrency', parseInt(e.target.value) || 0)} />, '0 = unlimited')}
          {fc('Stop at tools', <TextInput value={form.stop_at_tools || ''} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('stop_at_tools', e.target.value)} placeholder="tool1, tool2" block />, 'End the run after a turn that calls any of these; empty = run until the model stops')}
          {seg('Tool not found behavior', form.tool_not_found_behavior || '', [['', 'Error (default)'], ['return_to_model', 'Return to model']], v => set('tool_not_found_behavior', v))}
          {seg('Reasoning item ID policy', form.reasoning_item_id_policy || '', [['', 'Preserve (default)'], ['omit', 'Omit']], v => set('reasoning_item_id_policy', v),
            'Whether reasoning-item ids are kept when prior items are re-sent to the model on later turns')}
          <div className="form-checkbox-group">
            <FormControl>
              <Checkbox checked={form.disable_tool_choice_reset || false} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('disable_tool_choice_reset', e.target.checked)} />
              <FormControl.Label>Disable tool choice reset</FormControl.Label>
              <FormControl.Caption>Keep tool_choice across turns instead of resetting</FormControl.Caption>
            </FormControl>
          </div>
        </div>

        <div className="form-group">
          <div className="form-group-title">Resilience</div>
          <FormControl>
            <Checkbox checked={form.retry_enabled || false} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('retry_enabled', e.target.checked)} />
            <FormControl.Label>Enable retry</FormControl.Label>
            <FormControl.Caption>Automatically retry failed model calls with backoff</FormControl.Caption>
          </FormControl>
          {form.retry_enabled &&
            <JsonField label="Retry policy (JSON)" value={form.retry_policy || ''} onChange={v => set('retry_policy', v)} placeholder='{"max_attempts":3,"base_delay_ms":500,"max_delay_ms":30000,"multiplier":2}' caption="Empty = SDK defaults" />}
          <JsonField label="Fallback models (JSON)" value={form.fallback_models || ''} onChange={v => set('fallback_models', v)} placeholder='[{"model":"gpt-5.4-mini","api_key":"sk-..."},{"model":"claude-opus-5","provider_type":"anthropic","api_key":"sk-ant-..."}]' caption='JSON array of {model, provider_type, api_key, base_url} — provider_type is "openai" (default) or "anthropic"' />
        </div>

        <div className="form-group">
          <div className="form-group-title">Guardrails &amp; output</div>
          <JsonField label="Guardrails (JSON)" value={form.guardrails || ''} onChange={v => set('guardrails', v)} placeholder='["content_filter","max_output_length"]' caption="JSON array of guardrail names. Each guardrail carries the stages it inspects, so it is named once." />
          <JsonField label="Output schema (JSON Schema)" value={form.output_schema || ''} onChange={v => set('output_schema', v)} placeholder='{"type":"object","properties":{...},"required":[...]}' caption="Structured output JSON Schema — leave empty for plain text" multiline rows={3} />
          <JsonField label="Error handlers (JSON)" value={form.error_handlers || ''} onChange={v => set('error_handlers', v)} placeholder='{"max_turns":{"final_output":"Ran out of turns — please narrow the request."},"invalid_final_output":{"final_output":{...}}}' caption='Fallback final outputs keyed by error kind (max_turns / model_refusal / invalid_final_output) — the run completes with the fallback instead of failing. Values must be a JSON string for plain-text agents, or match the output schema. Optional per-kind "exclude_from_history": true keeps the fallback out of the conversation.' multiline rows={3} />
        </div>

        <div className="form-group">
          <div className="form-group-title">Approvals</div>
          <JsonField label="Approve tools (HITL)" value={form.approve_tools || ''} onChange={v => set('approve_tools', v)} placeholder='["*"] or ["tool_name1","tool_name2"]' caption='JSON array of tool names requiring human approval before execution. Use ["*"] for all tools.' />
        </div>

        <div className="form-group">
          <div className="form-group-title">Session</div>
          {fc('History limit', <TextInput block type="number" min={0} value={String(form.history_limit || 0)} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('history_limit', parseInt(e.target.value) || 0)} />, 'Max recent session items loaded per turn (0 = full history)')}
          {fc('Stored prompt ID', <TextInput value={form.prompt_id || ''} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('prompt_id', e.target.value)} placeholder="prompt_abc123" block />, 'OpenAI stored prompt ID')}
          {form.prompt_id && fc('Prompt version', <TextInput value={form.prompt_version || ''} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('prompt_version', e.target.value)} placeholder="Optional version pin" block />)}
        </div>

      </div>}

      <div className="form-actions">
        <Button onClick={() => {
          const ms: Record<string, unknown> = { ...preservedMs };
          const reasoning: Record<string, unknown> = { ...(initMs.reasoning as Record<string, unknown> | undefined) };
          if (reasoningEffort) reasoning.effort = reasoningEffort; else delete reasoning.effort;
          if (Object.keys(reasoning).length > 0) ms.reasoning = reasoning;
          if (serviceTier) ms.service_tier = serviceTier;
          for (const [label, raw, key] of [['Temperature', temperature, 'temperature'], ['Top-p', topP, 'top_p'], ['Max tokens', maxTokens, 'max_tokens']] as const) {
            if (raw.trim() === '') continue;
            const n = Number(raw);
            if (Number.isNaN(n)) { toast.error(label + ' is not a number — fix or clear it before saving'); return; }
            ms[key] = n;
          }
          if (extraBody.trim()) {
            // Block the save on malformed JSON — silently dropping the field
            // would lose the user's input without any feedback.
            try { ms.extra_body = JSON.parse(extraBody); }
            catch { toast.error('Extra body is not valid JSON — fix or clear it before saving'); return; }
          }
          const model_settings = Object.keys(ms).length > 0 ? JSON.stringify(ms) : '';
          // auth_mode is normalized at save too, not only in the provider
          // onChange: a legacy row with an auth mode its backend doesn't offer
          // would otherwise be rejected by a control the form no longer shows.
          const auth_mode = form.auth_mode && authModesFor(form.provider_type).includes(form.auth_mode) ? form.auth_mode : '';
          const flatPayload = { ...form, auth_mode, handoffs: JSON.stringify(selectedHandoffs), tools: JSON.stringify(selectedMcp), skills: JSON.stringify(effectiveSkills), model_settings };
          onSave(nestConfig(flatPayload) as unknown as AgentFormData & { handoffs: string; tools: string; skills: string; model_settings: string });
        }} variant="primary">Save</Button>
        {onCancel && <Button onClick={onCancel}>Cancel</Button>}
        {onDelete && <Button onClick={onDelete} variant="danger" style={{ marginLeft: 'auto' }}>Delete</Button>}
      </div>
    </Stack>
  );
}

export function AgentConfigPanel() {
  const { items: agents, adding, editing, startAdd, startEdit, cancel, save, remove, reload } =
    useCrud<Agent, AgentFormData & { handoffs: string; tools: string; skills: string; model_settings: string }>(api.agents);
  const { data: mcpServers } = useApi<McpServer[]>(() => api.mcpServers.list() as Promise<McpServer[]>);
  const { data: skills } = useApi<Skill[]>(() => api.skills.list() as Promise<Skill[]>);
  const { data: providerTypes } = useApi<ProviderTypeInfo[]>(() => api.providerTypes.list() as Promise<ProviderTypeInfo[]>);
  const [signingIn, setSigningIn] = useState<Record<string | number, boolean>>({});
  const pollRef = useRef<Record<string | number, { interval: ReturnType<typeof setInterval>; timeout: ReturnType<typeof setTimeout> }>>({});

  const stopPoll = useCallback((id: string | number) => {
    const entry = pollRef.current[id];
    if (entry) {
      clearInterval(entry.interval);
      clearTimeout(entry.timeout);
      delete pollRef.current[id];
    }
    setSigningIn(prev => {
      if (!prev[id]) return prev;
      return { ...prev, [id]: false };
    });
  }, []);

  useEffect(() => () => {
    for (const id of Object.keys(pollRef.current)) stopPoll(id);
  }, [stopPoll]);

  // The ChatGPT callback runs on a separate localhost server, so there is no
  // postMessage from the popup — polling the status endpoint is the only
  // completion signal. The button stays clickable while polling: a re-click
  // supersedes the stale attempt (stopPoll + fresh login) instead of leaving
  // the user stuck when the popup was closed or denied.
  const handleLogin = async (id: string | number) => {
    stopPoll(id);
    setSigningIn(prev => ({ ...prev, [id]: true }));
    try {
      const d = await api.chatgpt.login(id) as { authorize_url: string };
      window.open(d.authorize_url, 'chatgpt_oauth', 'width=500,height=700');
      const interval = setInterval(async () => {
        try {
          const s = await api.chatgpt.status(id) as { logged_in: boolean };
          if (s.logged_in) { stopPoll(id); reload(); }
        } catch { /* ignore transient */ }
      }, 2000);
      // Give up after 2 minutes: the button reverts to "Sign in" and a later
      // completed login still shows up on the next reload.
      const timeout = setTimeout(() => { stopPoll(id); reload(); }, 2 * 60 * 1000);
      pollRef.current[id] = { interval, timeout };
    } catch (e) {
      toast.error((e as Error).message);
      setSigningIn(prev => ({ ...prev, [id]: false }));
    }
  };

  const handleLogout = async (id: string | number) => {
    try {
      await api.chatgpt.logout(id);
      reload();
    } catch (e) {
      toast.error((e as Error).message);
    }
  };

  // Number of MCP servers this agent references that still exist.
  const mcpCount = (toolsJson: string): number => {
    if (!mcpServers) return 0;
    try {
      const ids: (string | number)[] = JSON.parse(toolsJson || '[]');
      return ids.filter(id => mcpServers.some(s => s.id === id)).length;
    } catch { return 0; }
  };

  // Number of skills enabled for this agent. An empty/absent skills field means
  // "not customized" -> every installed skill (mirrors AgentForm's effectiveSkills).
  const skillCount = (skillsJson?: string): number => {
    const all = skills || [];
    if (!skillsJson) return all.length;
    try {
      const paths: string[] = JSON.parse(skillsJson);
      return paths.filter(p => all.some(sk => sk.path === p)).length;
    } catch { return all.length; }
  };

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title>Agents</PageHeader.Title>
        </PageHeader.TitleArea>
        {!adding && !editing && <PageHeader.Actions><Button onClick={startAdd} variant="primary" size="small">+ Add</Button></PageHeader.Actions>}
      </PageHeader>

      {adding && <AgentForm onSave={save} onCancel={cancel} mcpServers={mcpServers ?? undefined} skills={skills ?? undefined} allAgents={agents} providerTypes={providerTypes ?? undefined} />}
      {editing && <AgentForm initial={editing} onSave={save} onCancel={cancel} onDelete={() => { remove(editing.id); cancel(); }} mcpServers={mcpServers ?? undefined} skills={skills ?? undefined} allAgents={agents} providerTypes={providerTypes ?? undefined} />}

      {!adding && !editing && <div className="Box">
        {agents.map(a => {
          const isChatGPT = a.provider?.auth_mode === 'chatgpt_login';
          const loggedIn = isChatGPT && !!a.chatgpt_logged_in;
          const mcp = mcpCount(a.tools);
          const skl = skillCount(a.skills);
          const pmeta = providerMeta(a.provider?.provider_type);
          return (
            <div key={a.id} className="Box-row">
              <div className="resource-row-main">
                <div className="resource-row-head">
                  {isChatGPT && <span className="form-status-dot" style={{ background: loggedIn ? 'var(--fgColor-success)' : 'var(--fgColor-muted)' }} />}
                  <span className="resource-row-title">{a.name}</span>
                  <Label variant={pmeta.badgeVariant}>{pmeta.badge}</Label>
                  {isChatGPT && <Label variant={loggedIn ? 'success' : 'secondary'}>ChatGPT</Label>}
                  {mcp > 0 && <Label variant="accent">{'MCP·' + mcp}</Label>}
                  {skl > 0 && <Label variant="done">{'Skills·' + skl}</Label>}
                </div>
                <div className="resource-row-meta">
                  <span>{[a.model || 'default model', a.provider?.base_url && ('@ ' + a.provider.base_url)].filter(Boolean).join(' ')}</span>
                </div>
              </div>
              <div className="resource-row-actions">
                {isChatGPT && (loggedIn
                  ? <Button onClick={() => handleLogout(a.id)} size="small" variant="invisible">Disconnect</Button>
                  : <Button
                      onClick={() => handleLogin(a.id)}
                      size="small"
                      style={{ color: 'var(--fgColor-success)' }}
                    >{signingIn[a.id] ? 'Signing in… (retry)' : 'Sign in'}</Button>
                )}
                <Button onClick={() => startEdit(a)} size="small" variant="invisible">Edit</Button>
              </div>
            </div>
          );
        })}
        {agents.length === 0 && (
          <Blankslate>
            <Blankslate.Description>No agents configured. Add one to customize model, provider, and behavior.</Blankslate.Description>
          </Blankslate>
        )}
      </div>}
    </Stack>
  );
}

export default AgentConfigPanel;
