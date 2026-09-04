import React, { useState } from 'react';
import { TextInput, Textarea, FormControl, Checkbox, Select, Stack } from '@primer/react';
import { TokenListInput } from '@/components/TokenListInput';
import { FormActions } from '@/components/FormActions';
import { CrudPanel, RowActionsMenu, ScopeBadge } from '@/components/CrudPanel';
import { useScopeFilter } from '@/components/ScopeFilter';
import { useTransfer } from '@/components/TransferDialog';
import { filterRows } from '@/lib/listFilter';
import { ReadOnlyContext, canDeleteRow, canDemoteRow, canEditRow, canReference } from '@/lib/access';
import { useMe } from '@/lib/me';
import { ResourceRow } from '@/components/ResourceRow';
import { api } from '@/lib/api';
import { useApi, useCrud } from '@/lib/hooks';
import { fc, seg } from '@/lib/form';
import { JsonField } from '@/lib/JsonField';
import { toast } from '@/lib/toast';
import { Disclosure } from '@/components/Disclosure';
import { AgentAvatar } from '@/components/AgentAvatar';
import { ScopeHint, collidingNames } from '@/components/AgentPicker';
import { AvatarPicker } from './AvatarPicker';
import { type Skill, type SkillGroup, groupSkills, qualifiedName } from '@/lib/skills';
import { providerMeta, providerFacts, type ProviderTypeInfo } from '@/lib/providers';

// The agent-config REST payload nests these scalar settings under JSON group
// objects. The form state stays flat, so flattenConfig lifts a loaded config's
// group keys to the top level and nestConfig folds them back before saving.
const CONFIG_GROUPS: Record<string, string[]> = {
  behavior: ['max_turns', 'handoff_description', 'tool_choice_reset', 'stop_at_tools', 'handoff_input_filter', 'max_tool_concurrency', 'tool_not_found_behavior', 'reasoning_item_id_policy', 'workflow_authoring', 'subagents', 'vision'],
  resilience: ['retry_enabled', 'retry_policy', 'fallback_models'],
  guardrails: ['guardrails', 'output_schema'],
  session: ['prompt_id', 'prompt_version', 'history_limit'],
  approval: ['approve_tools'],
  compaction: ['compaction_enabled', 'compaction_threshold_tokens', 'compaction_window', 'compaction_model', 'compaction_prompt'],
};

// The spellings the server reads as "feed the bad tool name back to the
// model" — which is what an unset tool_not_found_behavior now means.
const RETURN_TO_MODEL = new Set(['return_to_model', 'return_error_to_model']);

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
  avatar: string;
  description: string;
  instructions: string;
  model: string;
  provider_id: string;
  context_window: number;
  max_turns: number;
  handoff_description: string;
  tool_choice_reset: boolean;
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
  workflow_authoring: boolean;
  subagents: boolean;
  vision: boolean;
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
  scope?: string;
  owner_id?: string;
}

interface Agent {
  id: string | number;
  name: string;
  avatar?: string;
  description?: string;
  model: string;
  provider_id?: string;
  instructions: string;
  handoffs: string;
  tools: string;
  // Empty/absent means "not customized" -> the agent gets every installed skill.
  skills?: string;
  scope?: string;
  owner_id?: string;
}

// The referenced endpoints, for the picker and the list badge. Name and type
// are all this panel needs — credentials never reach it.
interface ProviderRef {
  id: string;
  name: string;
  type?: string;
  scope?: string;
  owner_id?: string;
}

interface AgentFormProps {
  initial?: Partial<AgentFormData> & { id?: string | number; scope?: string; owner_id?: string };
  onSave: (form: AgentFormData & { handoffs: string; tools: string; skills: string; model_settings: string }) => void;
  onCancel?: () => void;
  onDelete?: () => void;
  saving?: boolean;
  mcpServers?: McpServer[];
  skills?: Skill[];
  allAgents?: Agent[];
  providerTypes?: ProviderTypeInfo[];
  providers?: ProviderRef[];
}

function AgentForm({ initial, onSave, onCancel, onDelete, saving, mcpServers, skills, allAgents, providerTypes, providers }: AgentFormProps) {
  const { me } = useMe();
  const meId = me?.id;
  const initHandoffs = (): (string | number)[] => {
    try { return JSON.parse((initial && initial.handoffs) || '[]'); } catch { return []; }
  };
  const initTools = (): (string | number)[] => {
    try { return JSON.parse((initial && initial.tools) || '[]'); } catch { return []; }
  };
  // A brand-new agent starts with NO skills selected — skills are opt-in, so a
  // bot unrelated to any installed skill doesn't silently carry them all. Only
  // an EXISTING agent whose `skills` is unset (predates per-agent scoping) falls
  // back to "every installed skill" (null below), so an edit never strips them.
  const initSkills = (): string[] | null => {
    if (!initial) return [];
    if (typeof initial.skills !== 'string' || initial.skills === '') return null;
    try { return JSON.parse(initial.skills); } catch { return null; }
  };
  const parseModelSettings = (): Record<string, unknown> => {
    try { return JSON.parse((initial && initial.model_settings) || '{}'); } catch { return {}; }
  };
  const initMs = parseModelSettings() as { reasoning?: { effort?: string }; service_tier?: string; extra_body?: Record<string, unknown>; temperature?: number; top_p?: number; max_tokens?: number };
  const [form, setForm] = useState<AgentFormData>({
    name: '', avatar: '', description: '', instructions: '', model: '',
    provider_id: '', context_window: 0,
    max_turns: 0, handoff_description: '',
    tool_choice_reset: true, stop_at_tools: '',
    retry_enabled: false, retry_policy: '',
    fallback_models: '',
    guardrails: '', output_schema: '', error_handlers: '',
    prompt_id: '', prompt_version: '', history_limit: 0,
    handoff_input_filter: '', max_tool_concurrency: 0,
    tool_not_found_behavior: '', reasoning_item_id_policy: '', workflow_authoring: false, subagents: true, vision: false, approve_tools: '',
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
  const set = <K extends keyof AgentFormData>(k: K, v: AgentFormData[K]) => setForm(prev => ({ ...prev, [k]: v }));
  // The backend's facts follow the REFERENCED provider: wording from the
  // static table, machine facts (unsupported features) from the server's
  // registry. An agent with no provider runs on the built-in openai default.
  const selectedProvider = (providers || []).find(p => p.id === form.provider_id);
  const meta = providerMeta(selectedProvider?.type ?? '');
  const unsupported = providerFacts(providerTypes, selectedProvider?.type)?.unsupported ?? [];
  const providerHint = unsupported.length > 0
    ? `Fails loudly on this backend — leave unset: ${unsupported.slice(0, 6).join(', ')}${unsupported.length > 6 ? ` +${unsupported.length - 6} more` : ''}`
    : 'Endpoints and their API keys are managed under Providers';
  // Every picker offers only what this agent may REFERENCE (decisions §5.29): a
  // private agent sees global rows plus its owner's, a global one only global
  // rows. Without this an admin's all-rows listing would offer a foreign
  // private row the save then refuses.
  // A create — blank or a fork seed, which sheds scope/owner — will land
  // private and the caller's, so it references as such; only an EDIT takes
  // the stored row's pair.
  const holder = initial?.scope
    ? { scope: initial.scope, owner_id: initial.owner_id }
    : { scope: 'private', owner_id: meId };
  const refOK = (row: { scope?: string; owner_id?: string }) => canReference(holder, row);
  const visibleProviders = (providers || []).filter(refOK);
  const visibleMcp = (mcpServers || []).filter(refOK);
  const visibleSkills = (skills || []).filter(refOK);
  const handoffTargets = (allAgents || []).filter(a => a.id !== initial?.id && refOK(a));
  const handoffCollisions = collidingNames(handoffTargets);
  const toggleHandoff = (id: string | number) => {
    setSelectedHandoffs(prev => prev.includes(id) ? prev.filter(x => x !== id) : [...prev, id]);
  };
  const toggleMcp = (id: string | number) => {
    setSelectedMcp(prev => prev.includes(id) ? prev.filter(x => x !== id) : [...prev, id]);
  };
  // null selectedSkills = not customized yet -> effectively "every installed skill".
  // Computed from the live `skills` prop (not stale state) so it's correct even
  // before any effect/interaction has run. The selection stores skill IDS.
  const allSkillIds = visibleSkills.map(sk => sk.id);
  const effectiveSkills = selectedSkills ?? allSkillIds;
  const toggleSkill = (id: string) => {
    setSelectedSkills(prev => {
      const base = prev ?? allSkillIds;
      return base.includes(id) ? base.filter(x => x !== id) : [...base, id];
    });
  };
  // Skills are grouped by their import source (a repo can bundle dozens) so
  // the list stays manageable — collapsed by default, with a group-level
  // checkbox to select/deselect the whole source at once.
  const skillGroups = groupSkills(visibleSkills);
  const [expandedSkillRepos, setExpandedSkillRepos] = useState<Set<string>>(new Set());
  const toggleSkillRepoExpanded = (repo: string) => {
    setExpandedSkillRepos(prev => {
      const next = new Set(prev);
      if (next.has(repo)) next.delete(repo); else next.add(repo);
      return next;
    });
  };
  const toggleSkillGroup = (group: SkillGroup) => {
    const ids = group.skills.map(sk => sk.id);
    const allSelected = ids.every(id => effectiveSkills.includes(id));
    setSelectedSkills(prev => {
      const base = prev ?? allSkillIds;
      return allSelected ? base.filter(id => !ids.includes(id)) : Array.from(new Set([...base, ...ids]));
    });
  };

  return (
    <Stack gap="normal">
      {/* Avatar and name are one identity unit: the circle sits right of the
          name block, sized to span its label and field together. */}
      <div className="form-identity">
        <div>
          {fc('Name', <TextInput value={form.name} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('name', e.target.value)} placeholder="e.g. Code Assistant" block />)}
        </div>
        <AvatarPicker name={form.name} value={form.avatar || ''} onChange={v => set('avatar', v)} />
      </div>
      {fc('Description',
        <TextInput value={form.description} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('description', e.target.value)} placeholder="What this agent is for, in a sentence" block />,
        'Used to pick the right agent automatically — not sent to the model as instructions')}

      {/* The endpoint is a reference: its backend, credential and base URL
          live on the provider row, which is also where a key is entered. */}
      <div className="form-group">
        <div className="form-group-title">Provider</div>
        {fc('Endpoint',
          <Select value={form.provider_id} onChange={e => set('provider_id', e.target.value)} block>
            {/* An empty provider_id reaches no credential and the run fails
                its pre-flight, so the empty value is a placeholder, not an
                option that works. */}
            <Select.Option value="">Select an endpoint…</Select.Option>
            {visibleProviders.map(p => (
              <Select.Option key={p.id} value={p.id}>{p.name}</Select.Option>
            ))}
          </Select>,
          providerHint)}
      </div>

      <div className="form-group">
        <div className="form-group-title">Model</div>
        {fc('Model', <TextInput value={form.model} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('model', e.target.value)} placeholder={meta.modelPlaceholder} block />)}
        {fc('Context window',
          <TextInput block type="number" min={0} step={1000} value={String(form.context_window || 0)}
            onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('context_window', parseInt(e.target.value) || 0)} />,
          'Tokens this model accepts — the Context panel needs it to show how full the window is (0 = unknown, no provider reports it)')}
        <div className="form-row">
          <div>
            {fc('Temperature', <TextInput type="number" step={0.1} min={0} max={2} value={temperature} onChange={(e: React.ChangeEvent<HTMLInputElement>) => setTemperature(e.target.value)} block />)}
          </div>
          <div>
            {fc('Top-p', <TextInput type="number" step={0.05} min={0} max={1} value={topP} onChange={(e: React.ChangeEvent<HTMLInputElement>) => setTopP(e.target.value)} block />)}
          </div>
          <div>
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
          <span className="FormControl-caption">
            Set via API, preserved on save: {Object.keys(preservedMs).sort().join(', ')}
          </span>
        )}
      </div>

      <div className="form-group">
        <div className="form-group-title">Instructions</div>
        {fc('Instructions', <Textarea value={form.instructions} onChange={(e: React.ChangeEvent<HTMLTextAreaElement>) => set('instructions', e.target.value)} rows={8} placeholder="System prompt / instructions for this agent…" block className="textarea-grow" style={{ fontFamily: 'var(--fontStack-monospace)' }} />, null, { hideLabel: true })}
      </div>

      {visibleMcp.length > 0 && <div className="form-group">
        <div className="form-group-title">MCP servers</div>
        <div className="form-checkbox-group">
          {visibleMcp.map(s => {
            const usable = s.status === 'connected';
            return (
              <FormControl key={s.id} disabled={!usable}>
                <Checkbox checked={selectedMcp.includes(s.id)} disabled={!usable} onChange={() => toggleMcp(s.id)} />
                <FormControl.Label>
                  {s.name}
                  {usable
                    ? <span className="form-status-dot form-status-dot--success form-status-dot--inline" />
                    : <span className="resource-row-sub form-label-note">({s.status === 'disabled' ? 'disabled' : 'not connected'})</span>}
                </FormControl.Label>
              </FormControl>
            );
          })}
        </div>
        <div className="FormControl-caption">Select which MCP servers this agent can use — greyed-out servers are disabled or not currently connected</div>
      </div>}

      {visibleSkills.length > 0 && <div className="form-group">
        <div className="form-group-title">Skills</div>
        <div className="form-checkbox-group">
          {skillGroups.map(group => {
            const ids = group.skills.map(sk => sk.id);
            const selectedCount = ids.filter(id => effectiveSkills.includes(id)).length;
            const allSelected = selectedCount === ids.length;
            const expanded = expandedSkillRepos.has(group.key);
            return (
              <div key={group.key} className="checkbox-group-header">
                <Checkbox
                  checked={allSelected}
                  indeterminate={!allSelected && selectedCount > 0}
                  aria-label={`Select all skills in ${group.label}`}
                  onChange={() => toggleSkillGroup(group)}
                />
                <Disclosure variant="plain" className="checkbox-group-toggle" label={group.label} open={expanded} onToggle={() => toggleSkillRepoExpanded(group.key)}>
                  <div className="checkbox-group-body">
                    {group.skills.map(sk => (
                      <FormControl key={sk.id}>
                        <Checkbox checked={effectiveSkills.includes(sk.id)} onChange={() => toggleSkill(sk.id)} />
                        <FormControl.Label>{qualifiedName(sk)}</FormControl.Label>
                        {sk.description && <FormControl.Caption>{sk.description}</FormControl.Caption>}
                      </FormControl>
                    ))}
                  </div>
                </Disclosure>
                <span className="checkbox-group-header-count">{selectedCount}/{ids.length}</span>
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
                <FormControl.Label>
                  <span className="agent-inline">
                    <AgentAvatar name={a.name} avatar={a.avatar} size={20} />
                    {a.name}
                    <ScopeHint agent={a} colliding={handoffCollisions} />
                  </span>
                </FormControl.Label>
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

      {/* On by default (unlike workflow authoring): most chat agents delegate.
          A lean chat agent that never spawns subagents can opt out to drop the
          spawn_task / task_* schema from every request. */}
      <div className="form-group">
        <div className="form-group-title">Subagents</div>
        <FormControl>
          <Checkbox checked={form.subagents !== false} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('subagents', e.target.checked)} />
          <FormControl.Label>Spawn subagents & background tasks</FormControl.Label>
          <FormControl.Caption>spawn_task / task_status / task_stop / task_retry. Turn off for a chat-only agent to reclaim the task schema from every request. The /workflow command still runs workflows either way.</FormControl.Caption>
        </FormControl>
      </div>

      {/* Off by default: the flag is a claim that the MODEL accepts image
          input, and only a person can make it — turning it on for a text-only
          model just moves the failure from a clear config error to an opaque
          provider 400. */}
      <div className="form-group">
        <div className="form-group-title">Vision</div>
        <FormControl>
          <Checkbox checked={form.vision || false} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('vision', e.target.checked)} />
          <FormControl.Label>Accept image input</FormControl.Label>
          <FormControl.Caption>Lets messages to this agent carry images (the model must support vision). Requires the Attachment storage settings to be configured.</FormControl.Caption>
        </FormControl>
      </div>

      {/* Opt-in, unlike the task and todo tools a chat agent carries by default:
          the save schema costs every request, and authoring workflows is one
          agent's job, not every agent's. */}
      <div className="form-group">
        <div className="form-group-title">Workflows</div>
        <FormControl>
          <Checkbox checked={form.workflow_authoring || false} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('workflow_authoring', e.target.checked)} />
          <FormControl.Label>Author workflows from the chat</FormControl.Label>
          <FormControl.Caption>get_workflow / save_workflow — every save is shown to you for approval before it is written. Running one is spawn_task's, which an agent has unless subagents are turned off above.</FormControl.Caption>
        </FormControl>
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
          {fc('Summary prompt', <Textarea value={form.compaction_prompt || ''} onChange={(e: React.ChangeEvent<HTMLTextAreaElement>) => set('compaction_prompt', e.target.value)} rows={8} placeholder="Custom summarization instructions (leave empty for default)" block className="textarea-grow" style={{ fontFamily: 'var(--fontStack-monospace)' }} />)}
        </>}
      </div>

      <Disclosure variant="plain" className="advanced-toggle" label="Advanced">
        <div className="advanced-section">
          <div className="form-group">
            <div className="form-group-title">Behavior</div>
            {fc('Max turns', <TextInput block type="number" min={0} value={String(form.max_turns || 0)} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('max_turns', parseInt(e.target.value) || 0)} />, '0 = SDK default (10)')}
            {fc('Max tool concurrency', <TextInput block type="number" min={0} value={String(form.max_tool_concurrency || 0)} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('max_tool_concurrency', parseInt(e.target.value) || 0)} />, '0 = unlimited')}
            {fc('Stop at tools', <TokenListInput ariaLabel="Stop at tools" placeholder="tool1, tool2"
              values={(form.stop_at_tools || '').split(',').map(s => s.trim()).filter(Boolean)}
              onChange={vals => set('stop_at_tools', vals.join(','))} />, 'End the run after a turn that calls any of these; empty = run until the model stops')}
            {/* Older configs spell the default out ("return_to_model" or its
                alias); it is the default now, so those read as the unset button. */}
            {seg('Tool not found behavior', RETURN_TO_MODEL.has(form.tool_not_found_behavior || '') ? '' : form.tool_not_found_behavior, [['', 'Return to model (default)'], ['error', 'End the run']], v => set('tool_not_found_behavior', v),
              'What happens when the model calls a tool it does not have — a name it invented, or one plan mode is hiding')}
            {seg('Reasoning item ID policy', form.reasoning_item_id_policy || '', [['', 'Preserve (default)'], ['omit', 'Omit']], v => set('reasoning_item_id_policy', v),
              'Whether reasoning-item ids are kept when prior items are re-sent to the model on later turns')}
            <div className="form-checkbox-group">
              <FormControl>
                <Checkbox checked={form.tool_choice_reset !== false} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('tool_choice_reset', e.target.checked)} />
                <FormControl.Label>Reset tool choice after use</FormControl.Label>
                <FormControl.Caption>Clears a pinned tool_choice once a tool has run (the loop guard). Off keeps tool_choice across turns.</FormControl.Caption>
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

        </div>
      </Disclosure>

      <FormActions
        saving={saving}
        onSave={() => {
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
          const flatPayload = { ...form, handoffs: JSON.stringify(selectedHandoffs), tools: JSON.stringify(selectedMcp), skills: JSON.stringify(effectiveSkills), model_settings };
          onSave(nestConfig(flatPayload) as unknown as AgentFormData & { handoffs: string; tools: string; skills: string; model_settings: string });
        }}
        onCancel={onCancel}
        onDelete={onDelete}
      />
    </Stack>
  );
}

export function AgentConfigPanel() {
  const { me } = useMe();
  const isAdmin = me?.role === 'admin';
  const rowEditable = (a: Agent) => canEditRow(isAdmin, me?.id, a);
  const { items: agents, loading, adding, editing, startAdd, startEdit, cancel, save, saving, remove, reload } =
    useCrud<Agent, AgentFormData & { handoffs: string; tools: string; skills: string; model_settings: string }>(api.agents, 'agents');
  const [query, setQuery] = useState('');
  const scopeFilter = useScopeFilter();
  const rows = filterRows(agents, { mine: !!scopeFilter?.mine, meId: me?.id, query }, a => `${a.name} ${a.description || ''} ${a.model || ''}`);
  const transfer = useTransfer({ kindLabel: 'Agents', setOwner: api.agents.setOwner, onDone: reload });
  // Fork seeds the CREATE form from a row — nothing is written until Save.
  // Cleared on a plain "+ Add" so a stale seed never leaks into a blank form.
  const [forkOf, setForkOf] = useState<Agent | null>(null);
  const startFork = (a: Agent) => {
    const raw = a as Agent & { resilience?: { fallback_models?: string } };
    if (raw.resilience?.fallback_models?.includes('********')) {
      toast.info('Fallback-model keys are not copied to a fork — re-enter them before saving');
    }
    setForkOf(a); startAdd();
  };
  const startBlankAdd = () => { setForkOf(null); startAdd(); };
  const { data: mcpServers } = useApi<McpServer[]>(() => api.mcpServers.list() as Promise<McpServer[]>, [], 'mcp-servers');
  const { data: skills } = useApi<Skill[]>(() => api.skills.list() as Promise<Skill[]>, [], 'skills');
  const { data: providerTypes } = useApi<ProviderTypeInfo[]>(() => api.providerTypes.list() as Promise<ProviderTypeInfo[]>, [], 'provider-types');
  const { data: providers } = useApi<ProviderRef[]>(() => api.providers.list() as Promise<ProviderRef[]>, [], 'providers');

  // The key remounts the form when the seed changes; the fork seed drops the
  // id (so nothing treats it as the source row), sheds the source's
  // scope/owner (the copy lands like any create: private, the caller's) and
  // suffixes the name toward the per-scope unique index.
  const forkSeed = () => {
    const { id: _id, scope: _scope, owner_id: _owner, ...rest } =
      forkOf as Agent & { resilience?: { fallback_models?: string } };
    // A fork copies no secrets: on a create the ******** mask resolves to ""
    // server-side, so strip it and let the form show the truth instead of a
    // mask that would save as an empty key.
    if (rest.resilience?.fallback_models?.includes('********')) {
      try {
        const models = JSON.parse(rest.resilience.fallback_models) as { api_key?: string }[];
        for (const m of models) if (m.api_key === '********') delete m.api_key;
        rest.resilience = { ...rest.resilience, fallback_models: JSON.stringify(models) };
      } catch { /* malformed JSON: leave it; the form's JSON field surfaces it */ }
    }
    return { ...rest, name: forkOf!.name + '-fork' };
  };
  const form = adding ? <AgentForm key={forkOf ? 'fork-' + forkOf.id : 'blank'} saving={saving}
      initial={forkOf ? forkSeed() : undefined}
      onSave={save} onCancel={cancel} mcpServers={mcpServers ?? undefined} skills={skills ?? undefined} allAgents={agents} providerTypes={providerTypes ?? undefined} providers={providers ?? undefined} />
    : editing ? <AgentForm saving={saving} initial={editing} onSave={save} onCancel={cancel} onDelete={async () => { if (await remove(editing.id, editing.name)) cancel(); }} mcpServers={mcpServers ?? undefined} skills={skills ?? undefined} allAgents={agents} providerTypes={providerTypes ?? undefined} providers={providers ?? undefined} />
    : null;

  return (
    // Scoped rows: the form is a disabled view exactly when the opened row is
    // not the caller's to edit (canEditRow), not for every member.
    <ReadOnlyContext value={!!editing && !rowEditable(editing)}>
      <CrudPanel title="Agents" onAdd={startBlankAdd} onCancel={cancel} form={form} loading={loading} isEmpty={rows.length === 0}
        search={{ value: query, onChange: setQuery, placeholder: 'Search agents' }}
        onDelete={editing && canDeleteRow(isAdmin, me?.id, editing)
          ? async () => { if (await remove(editing.id, editing.name)) cancel(); } : null}
        empty={agents.length === 0 ? 'No agents yet.' : 'No matching agents.'}
        emptyHint={agents.length === 0 ? 'An agent is a model on an endpoint with its instructions and tools.' : undefined}>
        {rows.map(a => {
          const rowProvider = (providers || []).find(p => p.id === a.provider_id);
          return (
            <ResourceRow key={a.id}
              leading={<AgentAvatar name={a.name} avatar={a.avatar} size={32} />}
              title={a.name}
              badges={<ScopeBadge row={a} meId={me?.id} />}
              sub={a.description || undefined}
              // One meta line instead of a strip of labels: model@endpoint. An
              // unset or vanished provider is a row that cannot run — say so.
              meta={<span>{(a.model || 'no model') + (rowProvider ? '@' + rowProvider.name : ' · no endpoint')}</span>}
              actions={<RowActionsMenu name={a.name} editReadOnly={!rowEditable(a)} onEdit={() => startEdit(a)}
                onFork={() => startFork(a)}
                onTransfer={isAdmin ? () => transfer.start(a) : undefined}
                scope={{ row: a, setScope: api.agents.setScope, canPromote: isAdmin, canDemote: canDemoteRow(isAdmin, me?.id, a), onDone: reload }} />}
            />
          );
        })}
      </CrudPanel>
      {transfer.dialog}
    </ReadOnlyContext>
  );
}

export default AgentConfigPanel;
