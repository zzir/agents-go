import { useState, useEffect, useMemo, useRef, useCallback, type ChangeEvent } from 'react';
import { Button, Checkbox, Dialog, Flash, Link, SegmentedControl, Select, SelectPanel, Textarea, TextInput } from '@primer/react';
import type { SelectPanelItemInput } from '@primer/react';
import { TriangleDownIcon } from '@primer/octicons-react';
import { AgentPicker } from '@/components/AgentPicker';
import { api } from '@/lib/api';
import { useApi, useNarrow } from '@/lib/hooks';
import { diffLines } from '@/lib/diff';
import { providerMeta } from '@/lib/providers';
import { PayloadItem, ResponseItems, itemTag, itemText, payloadItems, type PayloadRecord } from '@/features/chat/TracePayload';

interface AgentOption {
  id: string | number;
  name: string;
  avatar?: string;
  scope?: string;
  model?: string;
  model_settings?: string;
  provider?: { provider_type?: string };
}

// One completed replay run: the response plus the request knobs that
// produced it, so attempts remain comparable after the form moved on.
interface ReplayAttempt {
  output: PayloadRecord[];
  usage?: PayloadRecord;
  durationMs?: number;
  ttftMs?: number;
  model: string;
  settings: string;
}

// The bridge's fixed local tool names (staticLocalToolNames + the task
// manager's trio). MCP tools are recognized by their "<server>__" prefix
// instead, so this list only has to track the bridge's own tools.
const BUILTIN_TOOL_NAMES = new Set([
  'exec_command', 'read_file', 'write_file', 'list_files', 'apply_patch',
  'spawn_task', 'task_status', 'task_stop',
  // The plan/todo tools the chat build injects.
  'submit_plan', 'todo_write',
]);

// toolGroup buckets a traced tool by provenance for the tools picker.
function toolGroup(name: string): string {
  if (name === 'read_skill') return 'Skills';
  if (BUILTIN_TOOL_NAMES.has(name)) return 'Built-in';
  const sep = name.indexOf('__');
  if (sep > 0) return 'MCP: ' + name.slice(0, sep);
  return 'Other';
}

// comparableText projects response items into diffable lines: message text
// and tool calls. Reasoning items are skipped — they vary run to run by
// design and would drown the diff in noise.
function comparableText(items: PayloadRecord[]): string {
  return items
    .filter(it => it.type !== 'reasoning')
    .map(it => itemText(it))
    .filter(Boolean)
    .join('\n');
}

// ReplayDialog is a two-pane playground for one traced model call: the left
// pane edits the request (instructions, settings, input items — seeded from
// the trace), the right pane shows the replay attempts next to the original
// response. Requests go through POST /playground/generate (no session, no
// run, tools are schema-only and never executed).
export function ReplayDialog({ data, onClose }: { data: PayloadRecord; onClose: () => void }) {
  const { data: agentList } = useApi<AgentOption[]>(() => api.agents.list() as Promise<AgentOption[]>, [], 'agents');
  const agents = useMemo(() => agentList || [], [agentList]);
  const [agentId, setAgentId] = useState('');
  const [model, setModel] = useState(typeof data.model === 'string' ? data.model : '');
  const [instructions, setInstructions] = useState(typeof data.system_instructions === 'string' ? data.system_instructions : '');
  const [settingsText, setSettingsText] = useState(() => {
    const s = data.model_settings && typeof data.model_settings === 'object' ? data.model_settings as PayloadRecord : null;
    return s && Object.keys(s).length > 0 ? JSON.stringify(s, null, 2) : '';
  });
  const [items, setItems] = useState(() => JSON.stringify(Array.isArray(data.input) ? data.input : [], null, 2));
  // Which tools go into the replay, keyed by name — the traced set by
  // default, individually toggleable from the grouped picker (which also
  // offers the agent's current tools beyond the trace, unselected).
  const [enabledTools, setEnabledTools] = useState<Set<string>>(
    () => new Set(payloadItems(data.tools).map(t => String(t.name || '')).filter(Boolean)),
  );
  const [includeHandoffs, setIncludeHandoffs] = useState(true);
  const [includeSchema, setIncludeSchema] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [attempts, setAttempts] = useState<ReplayAttempt[]>([]);
  const [selected, setSelected] = useState(0);
  const [view, setView] = useState<'output' | 'diff'>('output');
  const [itemsView, setItemsView] = useState<'list' | 'json'>('list');
  // Narrow layout: the two panes collapse to a Request/Response tab switch and
  // the request knobs/tool pickers hide behind an Options toggle.
  const narrow = useNarrow();
  const [pane, setPane] = useState<'request' | 'response'>('request');
  const [optsOpen, setOptsOpen] = useState(false);
  const [streamText, setStreamText] = useState('');
  const [streamReasoning, setStreamReasoning] = useState('');
  const abortRef = useRef<AbortController | null>(null);
  // Closing the dialog mid-run tears the model call down with it.
  useEffect(() => () => abortRef.current?.abort(), []);

  const tools = useMemo(() => payloadItems(data.tools), [data.tools]);
  const tracedNames = useMemo(
    () => new Set(tools.map(t => String(t.name || '')).filter(Boolean)),
    [tools],
  );
  // The agent's CURRENT tool surface — offered by the picker beyond the
  // traced set, for what-if replays (would the model have called this?).
  // A failed fetch offers nothing, not the previous agent's tools.
  const { data: agentToolList, error: agentToolsError } = useApi<PayloadRecord[]>(
    () => agentId ? (api.agents.tools(agentId) as Promise<PayloadRecord[]>) : Promise.resolve([]),
    [agentId],
  );
  const agentTools = useMemo(
    () => (agentToolsError ? [] : agentToolList || []),
    [agentToolList, agentToolsError],
  );
  // Traced tools first (their traced schema wins on a name collision), then
  // the agent's extras.
  const allTools = useMemo(() => {
    const out = [...tools];
    for (const t of agentTools) {
      const n = String(t.name || '');
      if (n && !tracedNames.has(n)) out.push(t);
    }
    return out;
  }, [tools, agentTools, tracedNames]);
  // Per-category rows for the three pickers, name only — descriptions made
  // every row two lines tall and the list a chore to scan. MCP keeps a group
  // per server inside its panel.
  const pickerData = useMemo(() => {
    const builtin: SelectPanelItemInput[] = [];
    const mcp: SelectPanelItemInput[] = [];
    const skills: SelectPanelItemInput[] = [];
    for (const t of allTools) {
      const n = String(t.name || '');
      if (!n) continue;
      const item: SelectPanelItemInput = {
        id: n,
        text: n,
        trailingVisual: tracedNames.has(n) ? undefined : <span className="trace-replay-nottraced">not traced</span>,
      };
      const g = toolGroup(n);
      if (g === 'Skills') skills.push(item);
      else if (g.startsWith('MCP: ')) { item.groupId = g.slice(5); mcp.push(item); }
      else builtin.push(item);
    }
    const mcpGroups = [...new Set(mcp.map(i => String(i.groupId)))].sort()
      .map(g => ({ groupId: g, header: { title: g, variant: 'filled' as const } }));
    return { builtin, mcp, skills, mcpGroups };
  }, [allTools, tracedNames]);
  // A panel reports its own full selection; swap exactly that slice of the
  // shared enabled-set so the other panels' choices survive.
  const applyPanelSelection = useCallback((panelItems: SelectPanelItemInput[], sel: SelectPanelItemInput[]) => {
    setEnabledTools(prev => {
      const next = new Set(prev);
      for (const i of panelItems) next.delete(String(i.id));
      for (const i of sel) next.add(String(i.id));
      return next;
    });
  }, []);
  // Handoffs are part of the tool surface the model saw; replayed as
  // schema-only tools. Older traces recorded only the names — those replay
  // with an empty parameter schema.
  const handoffs = useMemo(() => payloadItems(data.handoffs), [data.handoffs]);
  const handoffTools = useMemo(() => handoffs.map(h => ({
    name: String(h.tool_name || ''),
    description: typeof h.description === 'string' && h.description
      ? h.description
      : 'Handoff to ' + String(h.agent_name || 'agent'),
    parameters: h.parameters && typeof h.parameters === 'object' ? h.parameters : undefined,
  })).filter(t => t.name), [handoffs]);
  const outputSchema = useMemo(() => {
    const s = data.output_schema && typeof data.output_schema === 'object' ? data.output_schema as PayloadRecord : null;
    if (!s || !s.schema || typeof s.schema !== 'object') return null;
    return {
      name: typeof s.name === 'string' ? s.name : undefined,
      schema: s.schema as Record<string, unknown>,
      strict: s.strict === true,
    };
  }, [data.output_schema]);
  const originalOutput = useMemo(() => payloadItems(data.output), [data.output]);

  // Both editors validate as you type; Run stays disabled while either is broken.
  const itemsParsed = useMemo((): { value?: unknown[]; error?: string } => {
    try {
      const v: unknown = JSON.parse(items);
      if (!Array.isArray(v)) return { error: 'must be a JSON array' };
      return { value: v };
    } catch (e) {
      return { error: (e as Error).message };
    }
  }, [items]);
  const settingsParsed = useMemo((): { value?: Record<string, unknown>; error?: string } => {
    const t = settingsText.trim();
    if (!t) return {};
    try {
      const v: unknown = JSON.parse(t);
      if (!v || typeof v !== 'object' || Array.isArray(v)) return { error: 'must be a JSON object' };
      return { value: v as Record<string, unknown> };
    } catch (e) {
      return { error: (e as Error).message };
    }
  }, [settingsText]);

  // Initial selection keeps the TRACED model and settings — replay means
  // the traced request. Only an explicit agent switch reseeds them.
  useEffect(() => {
    if (!agentList) return;
    const match = agentList.find(a => a.name === data.name);
    setAgentId(String(match ? match.id : (agentList[0] ? agentList[0].id : '')));
  }, [agentList, data.name]);

  const selectedAgent = useMemo(() => agents.find(a => String(a.id) === agentId), [agents, agentId]);
  // Switching agents reseeds model + settings from that agent's config, so
  // the knobs and JSON reflect what would actually run.
  const applyAgent = (id: string) => {
    setAgentId(id);
    const a = agents.find(x => String(x.id) === id);
    if (!a) return;
    setModel(a.model || '');
    const ms = (a.model_settings || '').trim();
    if (!ms) { setSettingsText(''); return; }
    try { setSettingsText(JSON.stringify(JSON.parse(ms), null, 2)); } catch { setSettingsText(ms); }
  };

  const run = async () => {
    if (!itemsParsed.value) return;
    setError('');
    setStreamText('');
    setStreamReasoning('');
    setBusy(true);
    const ac = new AbortController();
    abortRef.current = ac;
    const sentTools = [
      ...allTools.filter(t => enabledTools.has(String(t.name || ''))),
      ...(includeHandoffs ? handoffTools : []),
    ];
    try {
      const res = await api.playground.generateStream({
        agent_config_id: agentId,
        model: model.trim() || undefined,
        system_instructions: instructions,
        input_items: itemsParsed.value,
        model_settings: settingsParsed.value,
        tools: sentTools.length > 0 ? sentTools : undefined,
        output_schema: includeSchema && outputSchema ? outputSchema : undefined,
      }, {
        onDelta: t => setStreamText(prev => prev + t),
        onReasoning: t => setStreamReasoning(prev => prev + t),
      }, ac.signal);
      // `busy` serializes runs, so the closure's length is the new index.
      setAttempts(prev => [...prev, {
        output: payloadItems(res.output),
        usage: res.usage,
        durationMs: res.duration_ms,
        ttftMs: res.ttft_ms,
        model: model.trim(),
        settings: settingsText.trim(),
      }]);
      setSelected(attempts.length);
    } catch (e) {
      // A user cancel is not an error worth flashing.
      if (!ac.signal.aborted) setError((e as Error).message);
    }
    abortRef.current = null;
    setStreamText('');
    setStreamReasoning('');
    setBusy(false);
  };

  const attempt = attempts[selected] || null;
  const usage = attempt && attempt.usage ? attempt.usage : null;
  const replayMeta = attempt
    ? [
        attempt.durationMs !== undefined ? attempt.durationMs + 'ms' : null,
        attempt.ttftMs !== undefined && attempt.ttftMs >= 0 ? 'ttft ' + attempt.ttftMs + 'ms' : null,
        usage ? '↑' + Number(usage.input_tokens || 0) + ' ↓' + Number(usage.output_tokens || 0) + ' tok' : null,
      ].filter(Boolean).join(' · ')
    : '';
  const diff = useMemo(() => {
    if (view !== 'diff' || !attempt) return null;
    return diffLines(comparableText(originalOutput), comparableText(attempt.output));
  }, [view, attempt, originalOutput]);

  return (
    <Dialog
      title="Replay generation"
      onClose={onClose}
      // Primer's named sizes cap out well below what a request/response
      // editor needs; explicit style wins over both (same as PanelDialog).
      // BOTH axes are capped: an uncapped 100vh height with a fixed width cap
      // turned the dialog into a tall narrow slab on large displays. A narrow
      // screen takes Primer's fullscreen — no room for a centered slab.
      height="large"
      position={{ narrow: 'fullscreen', regular: 'center' }}
      style={narrow ? undefined : { width: 'min(1440px, calc(100vw - 48px))', height: 'min(900px, calc(100vh - 96px))' }}
      // The scroll wrapper between dialog and body is not a flex container,
      // so the body must claim the height explicitly for the panes to fill.
      renderBody={({ children }) => (
        <Dialog.Body style={{ display: 'flex', height: '100%' }}>{children}</Dialog.Body>
      )}
    >
      <div className="trace-replay">
        <div className="trace-replay-toolbar">
          <AgentPicker className="trace-replay-agent" agents={agents} value={agentId} onChange={applyAgent} />
          {narrow && (
            <Button trailingVisual={TriangleDownIcon} aria-expanded={optsOpen} onClick={() => setOptsOpen(o => !o)} className="trace-replay-optsbtn">
              Options
            </Button>
          )}
          {/* Inline on desktop (display:contents); a collapsible panel on narrow. */}
          <div className={'trace-replay-secondary' + (optsOpen ? ' open' : '')}>
            <SettingsKnobs
              parsed={settingsParsed.error ? null : (settingsParsed.value || {})}
              onChange={s => setSettingsText(Object.keys(s).length > 0 ? JSON.stringify(s, null, 2) : '')}
              effortOptions={providerMeta(selectedAgent?.provider?.provider_type).effortOptions}
            />
            <ToolPicker label="Built-in" items={pickerData.builtin} enabled={enabledTools} onSelect={applyPanelSelection} />
            <ToolPicker label="MCP" items={pickerData.mcp} groupMetadata={pickerData.mcpGroups} enabled={enabledTools} onSelect={applyPanelSelection} />
            <ToolPicker label="Skills" items={pickerData.skills} enabled={enabledTools} onSelect={applyPanelSelection} />
            {handoffTools.length > 0 && (
              <label className="trace-replay-tools-toggle">
                <Checkbox checked={includeHandoffs} onChange={e => setIncludeHandoffs(e.target.checked)} />
                {'Handoffs (' + handoffTools.length + ')'}
              </label>
            )}
            {outputSchema && (
              <label className="trace-replay-tools-toggle" title="Replay with the traced structured-output schema">
                <Checkbox checked={includeSchema} onChange={e => setIncludeSchema(e.target.checked)} />
                Schema
              </label>
            )}
          </div>
          {busy ? (
            <Button variant="danger" onClick={() => abortRef.current?.abort()} className="trace-replay-run">
              Cancel
            </Button>
          ) : (
            <Button
              variant="primary"
              onClick={() => void run()}
              disabled={!agentId || !!itemsParsed.error || !!settingsParsed.error}
              className="trace-replay-run"
            >
              Run
            </Button>
          )}
        </div>
        {narrow && (
          <SegmentedControl aria-label="Pane" fullWidth size="small" className="trace-replay-paneseg" onChange={i => setPane(i === 0 ? 'request' : 'response')}>
            <SegmentedControl.Button selected={pane === 'request'}>Request</SegmentedControl.Button>
            <SegmentedControl.Button selected={pane === 'response'}>Response</SegmentedControl.Button>
          </SegmentedControl>
        )}
        <div className="trace-replay-cols" data-pane={pane}>
          <div className="trace-replay-col trace-replay-col-request">
            <div className="trace-replay-sec">System instructions</div>
            <Textarea value={instructions} onChange={e => setInstructions(e.target.value)} rows={4} block resize="none" placeholder="System instructions" className="trace-replay-instructions" />
            <div className="trace-replay-sec">
              Model settings
              {settingsParsed.error
                ? <span className="trace-replay-hint trace-replay-invalid">{settingsParsed.error}</span>
                : <span className="trace-replay-hint">empty = agent defaults</span>}
            </div>
            <div className="trace-replay-settings">
              <Textarea value={settingsText} onChange={e => setSettingsText(e.target.value)} rows={4} block resize="none" placeholder="{ }" />
            </div>
            <div className="trace-replay-sec">
              Input items
              {itemsParsed.error
                ? <span className="trace-replay-hint trace-replay-invalid">{itemsParsed.error}</span>
                : <span className="trace-replay-hint">{(itemsParsed.value ? itemsParsed.value.length : 0) + ' items'}</span>}
              <SegmentedControl aria-label="Input items view" size="small" className="trace-replay-itemsview" onChange={i => setItemsView(i === 0 ? 'list' : 'json')}>
                <SegmentedControl.Button selected={itemsView === 'list'}>List</SegmentedControl.Button>
                <SegmentedControl.Button selected={itemsView === 'json'}>JSON</SegmentedControl.Button>
              </SegmentedControl>
              {itemsView === 'json' && (
                <Link
                  as="button"
                  className="trace-replay-format"
                  style={{ marginLeft: 0 }}
                  onClick={() => { if (itemsParsed.value) setItems(JSON.stringify(itemsParsed.value, null, 2)); }}
                >
                  Format
                </Link>
              )}
            </div>
            {itemsView === 'json' ? (
              <div className="trace-replay-fill">
                <Textarea value={items} onChange={e => setItems(e.target.value)} block resize="none" />
              </div>
            ) : (
              <div className="trace-replay-fill">
                <div className="trace-replay-outbox trace-payload-list">
                  {itemsParsed.error
                    ? <div className="trace-empty">Invalid JSON — fix it in the JSON view.</div>
                    : (itemsParsed.value || []).length === 0
                      ? <div className="trace-empty">No input items.</div>
                      : (itemsParsed.value || []).map((it, i) => {
                        const item = (it && typeof it === 'object' ? it : { value: it }) as PayloadRecord;
                        return <PayloadItem key={'in-' + i} tag={itemTag(item)} text={itemText(item)} full={JSON.stringify(item, null, 2)} />;
                      })}
                </div>
              </div>
            )}
          </div>
          <div className="trace-replay-col trace-replay-col-response">
            {error && <Flash variant="danger" className="trace-replay-error">{error}</Flash>}
            <div className="trace-replay-sec">
              Replay response
              {replayMeta && <span className="trace-replay-hint">{replayMeta}</span>}
            </div>
            {attempts.length > 0 && (
              <div className="trace-replay-resultbar">
                <SegmentedControl aria-label="Attempt" size="small" onChange={i => setSelected(i)}>
                  {attempts.map((a, i) => (
                    <SegmentedControl.Button
                      key={i}
                      selected={i === selected}
                      title={[a.model && 'model: ' + a.model, a.settings && 'settings: ' + a.settings].filter(Boolean).join('\n') || undefined}
                    >
                      {String(i + 1)}
                    </SegmentedControl.Button>
                  ))}
                </SegmentedControl>
                <SegmentedControl aria-label="View" size="small" className="trace-replay-viewseg" onChange={i => setView(i === 0 ? 'output' : 'diff')}>
                  <SegmentedControl.Button selected={view === 'output'}>Output</SegmentedControl.Button>
                  <SegmentedControl.Button selected={view === 'diff'}>Diff</SegmentedControl.Button>
                </SegmentedControl>
              </div>
            )}
            <div className="trace-replay-outbox trace-payload-list">
              {busy ? (
                <div className="trace-replay-live">
                  {streamReasoning && <pre className="trace-replay-live-reasoning">{streamReasoning}</pre>}
                  {streamText
                    ? <pre className="trace-replay-live-text">{streamText}</pre>
                    : !streamReasoning && <div className="trace-empty">Waiting for the first token…</div>}
                </div>
              ) : !attempt ? (
                <div className="trace-empty">Edit the request, then Run.</div>
              ) : view === 'diff' && diff ? (
                <div className="trace-replay-diff">
                  {diff.length === 0 || diff.every(d => d.type === 'same')
                    ? <div className="trace-empty">Identical to the original response.</div>
                    : diff.map((d, i) => (
                      <div key={i} className={'trace-diff-line trace-diff-' + d.type}>
                        <span className="trace-diff-sign">{d.type === 'add' ? '+' : d.type === 'del' ? '-' : ' '}</span>
                        {d.text}
                      </div>
                    ))}
                </div>
              ) : (
                <ResponseItems items={attempt.output} prefix={'rp' + selected + '-'} />
              )}
            </div>
            <div className="trace-replay-sec">Original response</div>
            <div className="trace-replay-outbox trace-payload-list">
              {originalOutput.length > 0
                ? <ResponseItems items={originalOutput} prefix="og-" />
                : <div className="trace-empty">No output recorded for this generation.</div>}
            </div>
          </div>
        </div>
      </div>
    </Dialog>
  );
}

// ToolPicker is one tool category's SelectPanel — name-only rows, optional
// per-server groups — anchored on a "<label> n/N" button. Selection lives in
// the parent's single enabled-set shared by all pickers; a panel reports its
// full selection and the parent swaps that slice. Renders nothing when the
// category is empty.
function ToolPicker({ label, items, groupMetadata, enabled, onSelect }: {
  label: string;
  items: SelectPanelItemInput[];
  groupMetadata?: { groupId: string; header: { title: string; variant: 'filled' } }[];
  enabled: Set<string>;
  onSelect: (panelItems: SelectPanelItemInput[], sel: SelectPanelItemInput[]) => void;
}) {
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState('');
  const selected = useMemo(() => items.filter(i => enabled.has(String(i.id))), [items, enabled]);
  const filtered = useMemo(() => {
    const f = filter.trim().toLowerCase();
    return f ? items.filter(i => String(i.text).toLowerCase().includes(f)) : items;
  }, [items, filter]);
  if (items.length === 0) return null;
  return (
    <SelectPanel
      title={label + ' tools'}
      subtitle="Schema-only — never executed."
      renderAnchor={({ children: _children, ...anchorProps }) => (
        <Button trailingAction={TriangleDownIcon} {...anchorProps}>
          {label + ' ' + selected.length + '/' + items.length}
        </Button>
      )}
      open={open}
      onOpenChange={setOpen}
      items={filtered}
      selected={selected}
      onSelectedChange={(sel: SelectPanelItemInput[]) => onSelect(items, sel)}
      onFilterChange={setFilter}
      groupMetadata={groupMetadata}
      showSelectAll
      placeholderText="Filter"
      overlayProps={{ width: 'medium' }}
    />
  );
}

// SettingsKnobs lifts the common sampling parameters out of the raw settings
// JSON. The JSON text stays the single source of truth: knobs render from the
// parsed value and every knob edit re-serializes the whole object, so the two
// can never disagree. Invalid JSON disables the knobs until it parses again.
// effortOptions comes from the selected agent's provider (the backends accept
// different effort levels); a stored value outside the list stays visible.
function SettingsKnobs({ parsed, onChange, effortOptions }: {
  parsed: Record<string, unknown> | null;
  onChange: (s: Record<string, unknown>) => void;
  effortOptions: ReadonlyArray<readonly [string, string]>;
}) {
  const disabled = parsed === null;
  const s = parsed || {};
  const reasoning = s.reasoning && typeof s.reasoning === 'object' ? s.reasoning as Record<string, unknown> : null;
  const effort = reasoning && typeof reasoning.effort === 'string' ? reasoning.effort : '';
  const num = (v: unknown): string => (typeof v === 'number' ? String(v) : '');
  const set = (mut: (next: Record<string, unknown>) => void): void => {
    const next = { ...s };
    mut(next);
    onChange(next);
  };
  const setNum = (key: string) => (e: ChangeEvent<HTMLInputElement>) => {
    const v = e.target.value;
    set(next => {
      if (v === '') delete next[key];
      else next[key] = Number(v);
    });
  };
  const setEffort = (e: ChangeEvent<HTMLSelectElement>) => {
    const v = e.target.value;
    set(next => {
      const r = { ...(reasoning || {}) };
      if (v === '') delete r.effort;
      else r.effort = v;
      if (Object.keys(r).length === 0) delete next.reasoning;
      else next.reasoning = r;
    });
  };
  return (
    <>
      <label className="trace-replay-knob">
        temp
        <TextInput type="number" step={0.1} min={0} max={2} value={num(s.temperature)} onChange={setNum('temperature')} disabled={disabled} aria-label="temperature" />
      </label>
      <label className="trace-replay-knob">
        top_p
        <TextInput type="number" step={0.05} min={0} max={1} value={num(s.top_p)} onChange={setNum('top_p')} disabled={disabled} aria-label="top_p" />
      </label>
      <label className="trace-replay-knob">
        max tok
        <TextInput type="number" step={1} min={1} value={num(s.max_tokens)} onChange={setNum('max_tokens')} disabled={disabled} aria-label="max tokens" />
      </label>
      <label className="trace-replay-knob">
        effort
        <Select value={effort} onChange={setEffort} disabled={disabled} aria-label="reasoning effort">
          {(effortOptions.some(([v]) => v === effort) ? effortOptions : [...effortOptions, [effort, effort] as const])
            .map(([v, label]) => <Select.Option key={v || 'unset'} value={v}>{v === '' ? '' : label}</Select.Option>)}
        </Select>
      </label>
    </>
  );
}
