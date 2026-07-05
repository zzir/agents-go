import React, { useState, useEffect, useRef, useCallback } from 'react';
import { Button, TextInput, Textarea, Label, FormControl, Checkbox, SegmentedControl, Select, Stack, PageHeader } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { api } from '@/lib/api';
import { useApi, useCrud } from '@/lib/hooks';
import { fc } from '@/lib/form';
import { JsonField } from '@/lib/JsonField';
import { toast } from '@/lib/toast';
import { ChevronRightIcon } from '@primer/octicons-react';
import { type Skill, type SkillGroup, groupByRepo } from '@/lib/skills';

interface AgentFormData {
  name: string;
  instructions: string;
  model: string;
  provider_type: string;
  auth_mode: string;
  api_key: string;
  base_url: string;
  max_turns: number;
  handoff_description: string;
  disable_tool_choice_reset: boolean;
  tool_use_behavior: string;
  retry_enabled: boolean;
  retry_policy: string;
  fallback_models: string;
  input_guardrails: string;
  output_guardrails: string;
  output_schema: string;
  use_previous_response_id: boolean;
  prompt_id: string;
  prompt_version: string;
  handoff_input_filter: string;
  max_tool_concurrency: number;
  tool_not_found_behavior: string;
  approve_tools: string;
  compaction_enabled: boolean;
  compaction_threshold: number;
  compaction_window: number;
  compaction_model: string;
  compaction_prompt: string;
  tools?: string;
  skills?: string;
  model_settings?: string;
}

interface McpServer {
  id: string | number;
  name: string;
  enabled?: boolean;
  connected?: boolean;
}

interface Agent {
  id: string | number;
  name: string;
  model: string;
  base_url: string;
  auth_mode: string;
  instructions: string;
  tools: string;
  chatgpt_token: string;
}

interface AgentFormProps {
  initial?: Partial<AgentFormData> & { id?: string | number };
  onSave: (form: AgentFormData & { tools: string; skills: string; model_settings: string }) => void;
  onCancel?: () => void;
  onDelete?: () => void;
  mcpServers?: McpServer[];
  skills?: Skill[];
}

function AgentForm({ initial, onSave, onCancel, onDelete, mcpServers, skills }: AgentFormProps) {
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
  const initMs = parseModelSettings() as { reasoning?: { effort?: string }; service_tier?: string; extra_body?: Record<string, unknown> };
  const [form, setForm] = useState<AgentFormData>({
    name: '', instructions: '', model: 'gpt-5.5',
    provider_type: '', auth_mode: '', api_key: '', base_url: '',
    max_turns: 0, handoff_description: '',
    disable_tool_choice_reset: false, tool_use_behavior: '',
    retry_enabled: false, retry_policy: '',
    fallback_models: '',
    input_guardrails: '', output_guardrails: '', output_schema: '',
    use_previous_response_id: false,
    prompt_id: '', prompt_version: '',
    handoff_input_filter: '', max_tool_concurrency: 0,
    tool_not_found_behavior: '', approve_tools: '',
    compaction_enabled: false, compaction_threshold: 0,
    compaction_window: 0, compaction_model: '', compaction_prompt: '',
    ...initial,
  });
  const [reasoningEffort, setReasoningEffort] = useState(initMs.reasoning?.effort || '');
  const [serviceTier, setServiceTier] = useState(initMs.service_tier || '');
  const [extraBody, setExtraBody] = useState(initMs.extra_body ? JSON.stringify(initMs.extra_body) : '');
  const [selectedMcp, setSelectedMcp] = useState<(string | number)[]>(initTools);
  const [selectedSkills, setSelectedSkills] = useState<string[] | null>(initSkills);
  const [showAdvanced, setShowAdvanced] = useState(false);
  const set = <K extends keyof AgentFormData>(k: K, v: AgentFormData[K]) => setForm(prev => ({ ...prev, [k]: v }));
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
      {fc('Model', <TextInput value={form.model} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('model', e.target.value)} placeholder="gpt-5.5" block />)}
      {fc('Reasoning effort', <Select value={reasoningEffort} onChange={(e: React.ChangeEvent<HTMLSelectElement>) => setReasoningEffort(e.target.value)}>
        <Select.Option value="">Not set</Select.Option>
        <Select.Option value="low">Low</Select.Option>
        <Select.Option value="medium">Medium</Select.Option>
        <Select.Option value="high">High</Select.Option>
        <Select.Option value="xhigh">Extra High</Select.Option>
      </Select>)}
      {fc('Service tier', <Select value={serviceTier} onChange={(e: React.ChangeEvent<HTMLSelectElement>) => setServiceTier(e.target.value)}>
        <Select.Option value="">Not set</Select.Option>
        <Select.Option value="auto">Auto</Select.Option>
        <Select.Option value="default">Default</Select.Option>
        <Select.Option value="flex">Flex</Select.Option>
        <Select.Option value="priority">Priority</Select.Option>
      </Select>)}

      <JsonField label="Extra body (JSON)" value={extraBody} onChange={setExtraBody} placeholder='{"enable_thinking": true, "thinking_budget": 1024}' caption="Provider-specific parameters injected into every API request" />

      <div className="form-group">
        <div className="form-group-title">Provider</div>
        {fc('Auth mode', <SegmentedControl aria-label="Auth mode" size="small">
          <SegmentedControl.Button
            selected={form.auth_mode !== 'chatgpt_login'}
            onClick={() => set('auth_mode', '')}
          >API Key</SegmentedControl.Button>
          <SegmentedControl.Button
            selected={form.auth_mode === 'chatgpt_login'}
            onClick={() => set('auth_mode', 'chatgpt_login')}
          >ChatGPT Subscribe</SegmentedControl.Button>
        </SegmentedControl>, 'Choose authentication method')}

        {form.auth_mode !== 'chatgpt_login' && <>
          {fc('API key', <TextInput value={form.api_key} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('api_key', e.target.value)} placeholder="sk-..." type="password" block />, 'Stored keys show as ******** — leave the mask to keep the current key, clear the field to remove it')}
          {fc('Base URL', <TextInput value={form.base_url} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('base_url', e.target.value)} placeholder="https://api.openai.com/v1 (leave empty for default)" block />)}
        </>}

        {form.auth_mode === 'chatgpt_login' &&
          fc('Base URL override', <TextInput value={form.base_url} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('base_url', e.target.value)} placeholder="Leave empty for ChatGPT default" block />, 'Only change if you know what you\'re doing')
        }
      </div>

      <div className="form-group">
        {fc('Instructions', <Textarea value={form.instructions} onChange={(e: React.ChangeEvent<HTMLTextAreaElement>) => set('instructions', e.target.value)} rows={5} placeholder="System prompt / instructions for this agent..." block style={{ fontFamily: 'var(--fontStack-monospace)' }} />)}
      </div>

      {mcpServers && mcpServers.length > 0 && <div className="form-group">
        <div className="form-group-title">MCP Servers</div>
        <div className="form-checkbox-group">
          {mcpServers.map(s => {
            const usable = !!s.connected;
            return (
              <FormControl key={s.id} disabled={!usable}>
                <Checkbox checked={selectedMcp.includes(s.id)} disabled={!usable} onChange={() => toggleMcp(s.id)} />
                <FormControl.Label>
                  {s.name}
                  {usable
                    ? <span className="form-status-dot form-status-dot--success" style={{ width: 6, height: 6, marginLeft: 4, display: 'inline-block' }} />
                    : <span className="resource-row-sub" style={{ marginLeft: 4 }}>({s.enabled === false ? 'disabled' : 'not connected'})</span>}
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

      <button type="button" className="advanced-toggle" aria-expanded={showAdvanced} onClick={() => setShowAdvanced(!showAdvanced)}>
        <ChevronRightIcon size={12} />
        Advanced
      </button>

      {showAdvanced && <div className="advanced-section">
        <div className="form-group">
          <div className="form-group-title">Limits</div>
          {fc('Max turns', <TextInput block type="number" min={0} value={String(form.max_turns || 0)} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('max_turns', parseInt(e.target.value) || 0)} />, '0 = SDK default (10)')}
          {fc('Max tool concurrency', <TextInput block type="number" min={0} value={String(form.max_tool_concurrency || 0)} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('max_tool_concurrency', parseInt(e.target.value) || 0)} />, '0 = unlimited')}
        </div>

        <div className="form-group">
          <div className="form-group-title">Tool behavior</div>
          {fc('Tool use behavior', <Select value={form.tool_use_behavior || ''} onChange={(e: React.ChangeEvent<HTMLSelectElement>) => set('tool_use_behavior', e.target.value)}>
            <Select.Option value="">Run LLM Again (default)</Select.Option>
            <Select.Option value="stop_on_first">Stop on First Tool</Select.Option>
            <Select.Option value="stop_at:">Stop at Specific Tools</Select.Option>
          </Select>)}
          {form.tool_use_behavior && form.tool_use_behavior.startsWith('stop_at') &&
            fc('Stop at tool names', <TextInput value={(form.tool_use_behavior || '').replace('stop_at:', '')} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('tool_use_behavior', 'stop_at:' + e.target.value)} placeholder="tool1, tool2" block />)}
          {fc('Tool not found behavior', <Select value={form.tool_not_found_behavior || ''} onChange={(e: React.ChangeEvent<HTMLSelectElement>) => set('tool_not_found_behavior', e.target.value)}>
            <Select.Option value="">Error (default)</Select.Option>
            <Select.Option value="return_to_model">Return to Model</Select.Option>
          </Select>)}
        </div>

        <div className="form-group">
          <div className="form-group-title">Handoffs</div>
          {fc('Handoff input filter', <Select value={form.handoff_input_filter || ''} onChange={(e: React.ChangeEvent<HTMLSelectElement>) => set('handoff_input_filter', e.target.value)}>
            <Select.Option value="">None (default)</Select.Option>
            <Select.Option value="nest_history">Nest Handoff History</Select.Option>
          </Select>)}
          {fc('Handoff description', <TextInput value={form.handoff_description || ''} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('handoff_description', e.target.value)} placeholder="Description when this agent is a handoff target" block />)}
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
          <JsonField label="Fallback models (JSON)" value={form.fallback_models || ''} onChange={v => set('fallback_models', v)} placeholder='[{"model":"gpt-5.4-mini","api_key":"sk-...","base_url":""}]' caption="JSON array of {model, api_key, base_url}" />
        </div>

        <div className="form-group">
          <div className="form-group-title">Guardrails &amp; output</div>
          <JsonField label="Input guardrails (JSON)" value={form.input_guardrails || ''} onChange={v => set('input_guardrails', v)} placeholder='["content_filter","max_input_length"]' caption="JSON array of guardrail names" />
          <JsonField label="Output guardrails (JSON)" value={form.output_guardrails || ''} onChange={v => set('output_guardrails', v)} placeholder='["max_output_length"]' caption="JSON array of guardrail names" />
          <JsonField label="Output schema (JSON Schema)" value={form.output_schema || ''} onChange={v => set('output_schema', v)} placeholder='{"type":"object","properties":{...},"required":[...]}' caption="Structured output JSON Schema — leave empty for plain text" multiline rows={3} />
        </div>

        <div className="form-group">
          <div className="form-group-title">Approvals</div>
          <JsonField label="Approve tools (HITL)" value={form.approve_tools || ''} onChange={v => set('approve_tools', v)} placeholder='["*"] or ["tool_name1","tool_name2"]' caption='JSON array of tool names requiring human approval before execution. Use ["*"] for all tools.' />
        </div>

        <div className="form-group">
          <div className="form-group-title">Flags</div>
          <div className="form-checkbox-group">
            <FormControl>
              <Checkbox checked={form.disable_tool_choice_reset || false} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('disable_tool_choice_reset', e.target.checked)} />
              <FormControl.Label>Disable tool choice reset</FormControl.Label>
              <FormControl.Caption>Keep tool_choice across turns instead of resetting</FormControl.Caption>
            </FormControl>
          </div>
        </div>

        <div className="form-group">
          <div className="form-group-title">Compaction</div>
          <FormControl>
            <Checkbox checked={form.compaction_enabled || false} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('compaction_enabled', e.target.checked)} />
            <FormControl.Label>Enable compaction</FormControl.Label>
            <FormControl.Caption>Summarize old messages when history grows large (provider-agnostic)</FormControl.Caption>
          </FormControl>
          {form.compaction_enabled && <>
            {fc('Threshold', <TextInput block type="number" min={0} value={String(form.compaction_threshold || 0)} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('compaction_threshold', parseInt(e.target.value) || 0)} />, 'Item count that triggers compaction (0 = default 20)')}
            {fc('Window size', <TextInput block type="number" min={0} value={String(form.compaction_window || 0)} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('compaction_window', parseInt(e.target.value) || 0)} />, 'Recent items to keep intact (0 = default 10)')}
            {fc('Summary model', <TextInput value={form.compaction_model || ''} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('compaction_model', e.target.value)} placeholder="e.g. gpt-4.1-mini" block />, "Model used to generate conversation summaries (empty = the agent's model)")}
            {fc('Summary prompt', <Textarea value={form.compaction_prompt || ''} onChange={(e: React.ChangeEvent<HTMLTextAreaElement>) => set('compaction_prompt', e.target.value)} rows={3} placeholder="Custom summarization instructions (leave empty for default)" block style={{ fontFamily: 'var(--fontStack-monospace)' }} />)}
          </>}
        </div>

        <div className="form-group">
          <div className="form-group-title">Stored prompt</div>
          {fc('Stored prompt ID', <TextInput value={form.prompt_id || ''} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('prompt_id', e.target.value)} placeholder="prompt_abc123" block />, 'OpenAI stored prompt ID')}
          {form.prompt_id && fc('Prompt version', <TextInput value={form.prompt_version || ''} onChange={(e: React.ChangeEvent<HTMLInputElement>) => set('prompt_version', e.target.value)} placeholder="Optional version pin" block />)}
        </div>

      </div>}

      <div className="form-actions">
        <Button onClick={() => {
          const ms: Record<string, unknown> = {};
          if (reasoningEffort) ms.reasoning = { effort: reasoningEffort };
          if (serviceTier) ms.service_tier = serviceTier;
          if (extraBody.trim()) {
            // Block the save on malformed JSON — silently dropping the field
            // would lose the user's input without any feedback.
            try { ms.extra_body = JSON.parse(extraBody); }
            catch { toast.error('Extra body is not valid JSON — fix or clear it before saving'); return; }
          }
          const model_settings = Object.keys(ms).length > 0 ? JSON.stringify(ms) : '';
          // use_previous_response_id is rejected by the server (incompatible
          // with server-side session storage); always send false so legacy
          // configs that still carry the flag are cleaned up on their next save.
          onSave({ ...form, use_previous_response_id: false, tools: JSON.stringify(selectedMcp), skills: JSON.stringify(effectiveSkills), model_settings });
        }} variant="primary">Save</Button>
        {onCancel && <Button onClick={onCancel}>Cancel</Button>}
        {onDelete && <Button onClick={onDelete} variant="danger" style={{ marginLeft: 'auto' }}>Delete</Button>}
      </div>
    </Stack>
  );
}

export function AgentConfigPanel() {
  const { items: agents, adding, editing, startAdd, startEdit, cancel, save, remove, reload } =
    useCrud<Agent, AgentFormData & { tools: string; skills: string; model_settings: string }>(api.agents);
  const { data: mcpServers } = useApi<McpServer[]>(() => api.mcpServers.list() as Promise<McpServer[]>);
  const { data: skills } = useApi<Skill[]>(() => api.skills.list() as Promise<Skill[]>);
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

  const handleLogin = async (id: string | number) => {
    stopPoll(id);
    setSigningIn(prev => ({ ...prev, [id]: true }));
    try {
      const d = await api.chatgpt.login(id) as { authorize_url: string };
      window.open(d.authorize_url, 'chatgpt_oauth', 'width=500,height=700');
      const interval = setInterval(async () => {
        try {
          const a = await api.agents.get(id) as Agent;
          if (a.chatgpt_token) { stopPoll(id); reload(); }
        } catch { /* ignore transient */ }
      }, 2000);
      const timeout = setTimeout(() => { stopPoll(id); reload(); }, 5 * 60 * 1000);
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

  const mcpNames = (toolsJson: string): string | null => {
    if (!mcpServers) return null;
    try {
      const ids: (string | number)[] = JSON.parse(toolsJson || '[]');
      const names = ids.map(id => (mcpServers.find(s => s.id === id) || {} as McpServer).name).filter(Boolean);
      return names.length ? 'MCP: ' + names.join(', ') : null;
    } catch { return null; }
  };

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title>Agents</PageHeader.Title>
        </PageHeader.TitleArea>
        {!adding && !editing && <PageHeader.Actions><Button onClick={startAdd} variant="primary" size="small">+ Add</Button></PageHeader.Actions>}
      </PageHeader>

      {adding && <AgentForm onSave={save} onCancel={cancel} mcpServers={mcpServers ?? undefined} skills={skills ?? undefined} />}
      {editing && <AgentForm initial={editing} onSave={save} onCancel={cancel} onDelete={() => { remove(editing.id); cancel(); }} mcpServers={mcpServers ?? undefined} skills={skills ?? undefined} />}

      {!adding && !editing && <div className="Box">
        {agents.map(a => {
          const mcp = mcpNames(a.tools);
          const isChatGPT = a.auth_mode === 'chatgpt_login';
          const loggedIn = isChatGPT && !!a.chatgpt_token;
          return (
            <div key={a.id} className="Box-row">
              <div className="resource-row-main">
                <div className="form-status">
                  {isChatGPT && <span className="form-status-dot" style={{ background: loggedIn ? 'var(--fgColor-success)' : 'var(--fgColor-muted)' }} />}
                  <span className="resource-row-title">{a.name}</span>
                </div>
                <div className="resource-row-meta">
                  <span>{[a.model || 'default model', a.base_url && ('@ ' + a.base_url)].filter(Boolean).join(' ')}</span>
                  {isChatGPT && <Label variant={loggedIn ? 'success' : 'secondary'}>ChatGPT</Label>}
                </div>
                {a.instructions && <div className="resource-row-sub">
                  {a.instructions.substring(0, 80) + (a.instructions.length > 80 ? '...' : '')}
                </div>}
                {mcp && <div className="resource-row-meta">{mcp}</div>}
              </div>
              <div className="resource-row-actions">
                {isChatGPT && (loggedIn
                  ? <Button onClick={() => handleLogout(a.id)} size="small" variant="invisible">Disconnect</Button>
                  : <Button
                      onClick={() => handleLogin(a.id)}
                      disabled={signingIn[a.id]}
                      size="small"
                      style={{ color: 'var(--fgColor-success)' }}
                    >{signingIn[a.id] ? 'Signing in...' : 'Sign in'}</Button>
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
