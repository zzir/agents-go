import './trace.css';
import { useState, useEffect, useMemo, useRef, useCallback, type ChangeEvent } from 'react';
import { Button, Checkbox, CounterLabel, Dialog, Flash, Link, SegmentedControl, Select, SelectPanel, Textarea, TextInput } from '@primer/react';
import type { SelectPanelItemInput } from '@primer/react';
import { api } from '@/lib/api';
import { diffLines } from '@/lib/diff';
import { providerMeta } from '@/lib/providers';
import {
  PulseIcon, ToolsIcon, ArrowSwitchIcon, DiamondIcon,
  DependabotIcon, CpuIcon, ShieldCheckIcon, ChevronRightIcon,
  CommentIcon, TriangleDownIcon,
} from '@primer/octicons-react';
import type { Icon } from '@primer/octicons-react';
import { SidePanel } from '@/layout/SidePanel';
import { Disclosure } from '@/components/Disclosure';

export interface TraceEventData {
  kind?: string;
  name: string;
  detail?: string;
  type?: string;
  span_id?: string;
  parent_id?: string;
  // The run's lineage (a wake-up run's spawning run), carried by the trace
  // itself — what the drawer's run grouping reads.
  parent_run_id?: string;
  error?: string;
  started_at?: string;
  ended_at?: string;
  data?: Record<string, unknown> | null;
  duration?: string;
}

// A Context panel jump: which span to open and take the reader to. The nonce
// makes a second click on the same item a new instruction rather than a no-op.
export interface TraceReveal {
  runId: string;
  spanId: string;
  nonce: number;
}

// What a span row needs of it — the run is already resolved by then.
type SpanReveal = Pick<TraceReveal, 'spanId' | 'nonce'>;

// Span type → icon + color, mirroring the SDK's typed span constructors.
const SPAN_META: Record<string, { color: string; icon: Icon }> = {
  agent:      { color: 'var(--fgColor-open)',      icon: DependabotIcon },
  generation: { color: 'var(--fgColor-accent)',    icon: CpuIcon },
  function:   { color: 'var(--fgColor-done)',      icon: ToolsIcon },
  handoff:    { color: 'var(--fgColor-severe)',    icon: ArrowSwitchIcon },
  guardrail:  { color: 'var(--fgColor-attention)', icon: ShieldCheckIcon },
  compaction: { color: 'var(--fgColor-attention)', icon: DiamondIcon },
};

/* ---------- generation payload (full model request/response) ---------- */

type PayloadRecord = Record<string, unknown>;

function itemTag(item: PayloadRecord): string {
  if (typeof item.role === 'string' && item.role) return item.role;
  if (typeof item.type === 'string' && item.type) return item.type;
  return 'item';
}

// One-line summary text for a request/response item; falls back to JSON.
function itemText(item: PayloadRecord): string {
  const content = item.content;
  if (typeof content === 'string' && content) return content;
  if (Array.isArray(content)) {
    // Refusal parts carry their text in `refusal`, not `text` — without
    // this, an Anthropic refusal in the trace renders as raw JSON.
    const texts = content
      .map(p => (p && typeof p === 'object' ? ((p as PayloadRecord).text ?? (p as PayloadRecord).refusal) : null))
      .filter((t): t is string => typeof t === 'string' && t !== '');
    if (texts.length > 0) return texts.join('\n');
  }
  if (item.type === 'function_call') {
    return String(item.name || '') + '(' + String(item.arguments || '') + ')';
  }
  if (item.type === 'function_call_output') {
    const o = item.output;
    return typeof o === 'string' ? o : JSON.stringify(o);
  }
  if (Array.isArray(item.summary)) {
    const s = item.summary
      .map(p => (p && typeof p === 'object' ? (p as PayloadRecord).text : null))
      .filter((t): t is string => typeof t === 'string' && t !== '')
      .join('\n');
    if (s) return s;
  }
  return JSON.stringify(item);
}

// tagClass maps a payload tag to its Primer Label color variant so roles are
// distinguishable at a glance.
function tagClass(tag: string): string {
  const t = tag.split(' ')[0];
  if (t === 'user') return ' trace-ev-tag-user';
  if (t === 'assistant') return ' trace-ev-tag-assistant';
  if (t === 'system') return ' trace-ev-tag-system';
  if (t === 'function_call' || t === 'function_call_output' || t === 'tools' || t === 'input' || t === 'output') return ' trace-ev-tag-fn';
  return '';
}

// A single request/response entry: tag + one-line preview, expandable to the
// full text (or full item JSON when there is no plain text).
function PayloadItem({ tag, text, full }: { tag: string; text: string; full: string }) {
  const [open, setOpen] = useState(false);
  return (
    <div className="trace-payload-item">
      <div className="trace-payload-line" onClick={() => setOpen(o => !o)}>
        <span className={'trace-ev-tag' + tagClass(tag)} title={tag}>{tag}</span>
        <span className="trace-payload-preview">{text.length > 120 ? text.slice(0, 120) + '…' : text}</span>
      </div>
      {open && <pre className="trace-span-data trace-payload-full">{full}</pre>}
    </div>
  );
}

function payloadItems(value: unknown): PayloadRecord[] {
  return Array.isArray(value) ? value.filter((x): x is PayloadRecord => !!x && typeof x === 'object') : [];
}

// prettyMaybeJSON pretty-prints a value that may hold a JSON string.
function prettyMaybeJSON(v: unknown): string {
  const s = typeof v === 'string' ? v : JSON.stringify(v);
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}

// Structured view of a function span's data: the tool call's arguments and
// its stringified result.
function FunctionPayload({ data, indent }: { data: PayloadRecord; indent: number }) {
  return (
    <div className="trace-payload" style={{ marginLeft: indent }}>
      <div className="trace-payload-list">
        {data.input !== undefined && (
          <PayloadItem tag="input" text={typeof data.input === 'string' ? data.input : JSON.stringify(data.input)} full={prettyMaybeJSON(data.input)} />
        )}
        {data.output !== undefined && (
          <PayloadItem tag="output" text={typeof data.output === 'string' ? data.output : JSON.stringify(data.output)} full={prettyMaybeJSON(data.output)} />
        )}
      </div>
    </div>
  );
}

interface AgentOption {
  id: string | number;
  name: string;
  model?: string;
  model_settings?: string;
  provider?: { provider_type?: string };
}

// itemsForDisplay renders a list of response items with the shared
// tag + preview + expandable-full treatment.
function ResponseItems({ items, prefix }: { items: PayloadRecord[]; prefix: string }) {
  return (
    <>
      {items.map((item, i) => (
        <PayloadItem key={prefix + i} tag={itemTag(item)} text={itemText(item)} full={itemText(item) === JSON.stringify(item) ? JSON.stringify(item, null, 2) : itemText(item)} />
      ))}
    </>
  );
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
  'brave_search', 'spawn_task', 'task_status', 'task_stop',
  // The plan/todo tools the chat build injects.
  'submit_plan', 'todo_write',
]);

// toolGroup buckets a traced tool by provenance for the tools picker.
function toolGroup(name: string): string {
  if (name === 'read_skill_file') return 'Skills';
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
function ReplayDialog({ data, onClose }: { data: PayloadRecord; onClose: () => void }) {
  const [agents, setAgents] = useState<AgentOption[]>([]);
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
  const [agentTools, setAgentTools] = useState<PayloadRecord[]>([]);
  const [includeHandoffs, setIncludeHandoffs] = useState(true);
  const [includeSchema, setIncludeSchema] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [attempts, setAttempts] = useState<ReplayAttempt[]>([]);
  const [selected, setSelected] = useState(0);
  const [view, setView] = useState<'output' | 'diff'>('output');
  const [itemsView, setItemsView] = useState<'list' | 'json'>('list');
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
  useEffect(() => {
    if (!agentId) { setAgentTools([]); return; }
    let stale = false;
    (api.agents.tools(agentId) as Promise<PayloadRecord[]>)
      .then(list => { if (!stale) setAgentTools(list || []); })
      .catch(() => { if (!stale) setAgentTools([]); });
    return () => { stale = true; };
  }, [agentId]);
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

  useEffect(() => {
    api.agents.list().then((list: AgentOption[]) => {
      setAgents(list || []);
      // Initial selection keeps the TRACED model and settings — replay means
      // the traced request. Only an explicit agent switch reseeds them.
      const match = (list || []).find(a => a.name === data.name);
      setAgentId(String(match ? match.id : (list && list[0] ? list[0].id : '')));
    }).catch(() => {});
  }, [data.name]);

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
      // editor needs; explicit style wins over both (same as SettingsDialog).
      // BOTH axes are capped: an uncapped 100vh height with a fixed width cap
      // turned the dialog into a tall narrow slab on large displays.
      height="large"
      style={{ width: 'min(1440px, calc(100vw - 48px))', height: 'min(900px, calc(100vh - 96px))' }}
      // The scroll wrapper between dialog and body is not a flex container,
      // so the body must claim the height explicitly for the panes to fill.
      renderBody={({ children }) => (
        <Dialog.Body style={{ display: 'flex', height: '100%' }}>{children}</Dialog.Body>
      )}
    >
      <div className="trace-replay">
        <div className="trace-replay-toolbar">
          <Select value={agentId} onChange={e => applyAgent(e.target.value)} className="trace-replay-agent">
            {agents.map(a => <Select.Option key={a.id} value={String(a.id)}>{a.name}</Select.Option>)}
          </Select>
          <TextInput value={model} onChange={e => setModel(e.target.value)} placeholder="model (agent default)" className="trace-replay-model" />
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
        <div className="trace-replay-cols">
          <div className="trace-replay-col">
            <div className="trace-replay-sec">System instructions</div>
            <Textarea value={instructions} onChange={e => setInstructions(e.target.value)} rows={4} block resize="none" placeholder="System instructions" />
            <div className="trace-replay-sec">
              Model settings
              {settingsParsed.error
                ? <span className="trace-replay-invalid">{settingsParsed.error}</span>
                : <span className="trace-replay-hint">empty = agent defaults</span>}
            </div>
            <div className="trace-replay-settings">
              <Textarea value={settingsText} onChange={e => setSettingsText(e.target.value)} rows={4} block resize="none" placeholder="{ }" />
            </div>
            <div className="trace-replay-sec">
              Input items
              {itemsParsed.error
                ? <span className="trace-replay-invalid">{itemsParsed.error}</span>
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
          <div className="trace-replay-col">
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
                <div className="trace-empty">Edit the request on the left, then Run.</div>
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

// Structured view of a generation span's data: the exact request body the
// model received (instructions, tool definitions, settings, items) and the
// items it returned.
function GenerationPayload({ data, indent }: { data: PayloadRecord; indent: number }) {
  const [replayOpen, setReplayOpen] = useState(false);
  const input = payloadItems(data.input);
  const output = payloadItems(data.output);
  const tools = payloadItems(data.tools);
  const handoffs = payloadItems(data.handoffs);
  const settingsRaw = data.model_settings && typeof data.model_settings === 'object' ? data.model_settings as PayloadRecord : null;
  const settings = settingsRaw && Object.keys(settingsRaw).length > 0 ? settingsRaw : null;
  const outputSchema = data.output_schema && typeof data.output_schema === 'object' ? data.output_schema as PayloadRecord : null;
  const instructions = typeof data.system_instructions === 'string' ? data.system_instructions : '';
  const meta = [
    typeof data.model === 'string' && data.model ? data.model : null,
    typeof data.time_to_first_token_ms === 'number' ? 'ttft ' + data.time_to_first_token_ms + 'ms' : null,
    typeof data.previous_response_id === 'string' && data.previous_response_id ? 'prev: ' + data.previous_response_id : null,
    typeof data.conversation_id === 'string' && data.conversation_id ? 'conv: ' + data.conversation_id : null,
  ].filter(Boolean).join(' · ');

  return (
    <div className="trace-payload" style={{ marginLeft: indent }}>
      <div className="trace-payload-meta">
        <span className="trace-payload-meta-text" title={meta}>{meta}</span>
        <Link as="button" onClick={() => setReplayOpen(true)} style={{ flexShrink: 0, fontSize: 11 }}>
          Replay
        </Link>
      </div>
      {replayOpen && <ReplayDialog data={data} onClose={() => setReplayOpen(false)} />}
      {/* One shared grid for both sections so the tag column width (and thus
          the preview start) is identical across Request and Response. */}
      <div className="trace-payload-list">
        <div className="trace-section-label">Request</div>
        {instructions && <PayloadItem tag="system" text={instructions} full={instructions} />}
        {tools.length > 0 && (
          <PayloadItem
            tag={'tools (' + tools.length + ')'}
            text={tools.map(t => String(t.name || '')).join(', ')}
            full={JSON.stringify(tools, null, 2)}
          />
        )}
        {settings && (
          <PayloadItem tag="settings" text={JSON.stringify(settings)} full={JSON.stringify(settings, null, 2)} />
        )}
        {handoffs.length > 0 && (
          <PayloadItem
            tag={'handoffs (' + handoffs.length + ')'}
            text={handoffs.map(h => String(h.agent_name || h.tool_name || '')).join(', ')}
            full={JSON.stringify(handoffs, null, 2)}
          />
        )}
        {outputSchema && (
          <PayloadItem tag="output_schema" text={String(outputSchema.name || 'schema')} full={JSON.stringify(outputSchema, null, 2)} />
        )}
        {input.map((item, i) => (
          <PayloadItem key={'in-' + i} tag={itemTag(item)} text={itemText(item)} full={itemText(item) === JSON.stringify(item) ? JSON.stringify(item, null, 2) : itemText(item)} />
        ))}
        {typeof data.input === 'string' && <div className="trace-payload-preview">{String(data.input)}</div>}
        <div className="trace-section-label">Response</div>
        {output.map((item, i) => (
          <PayloadItem key={'out-' + i} tag={itemTag(item)} text={itemText(item)} full={itemText(item) === JSON.stringify(item) ? JSON.stringify(item, null, 2) : itemText(item)} />
        ))}
        {typeof data.output === 'string' && <div className="trace-payload-preview">{String(data.output)}</div>}
      </div>
    </div>
  );
}

/* ---------- span tree + waterfall ---------- */

interface SpanNode {
  span: TraceEventData;
  children: SpanNode[];
}

function buildSpanTree(spans: TraceEventData[]): SpanNode[] {
  const byId = new Map<string, SpanNode>();
  for (const s of spans) {
    if (s.span_id) byId.set(s.span_id, { span: s, children: [] });
  }
  const roots: SpanNode[] = [];
  for (const s of spans) {
    const node = s.span_id ? byId.get(s.span_id) : undefined;
    if (!node) continue;
    const parent = s.parent_id ? byId.get(s.parent_id) : undefined;
    if (parent) parent.children.push(node);
    else roots.push(node);
  }
  return roots;
}

interface TimeRange {
  t0: number;
  total: number;
}

function spanTimeRange(spans: TraceEventData[]): TimeRange | null {
  let t0 = Infinity, t1 = -Infinity;
  for (const s of spans) {
    if (!s.started_at) continue;
    const a = new Date(s.started_at).getTime();
    const b = s.ended_at ? new Date(s.ended_at).getTime() : a;
    if (a < t0) t0 = a;
    if (b > t1) t1 = b;
  }
  if (!isFinite(t0)) return null;
  return { t0, total: Math.max(t1 - t0, 1) };
}

// spanHasDetails reports whether a span row can expand: the server strips
// content-free data before sending, so any data at all means real details
// (payload, counts), and errors always expand.
function spanHasDetails(s: TraceEventData): boolean {
  return !!s.error || !!(s.data && Object.keys(s.data).length > 0);
}

// alignChevron: reserve the chevron slot even without details, so icons line
// up when siblings on the same level are expandable.
// reveal: the span the Context panel sent the reader to — it opens, scrolls
// into view and flashes. Its children are already rendered, so a sandbox or MCP
// span under it needs no separate reveal. The nonce re-fires the same target.
function SpanRow({ node, depth, range, alignChevron, reveal }: { node: SpanNode; depth: number; range: TimeRange | null; alignChevron: boolean; reveal?: SpanReveal }) {
  const [open, setOpen] = useState(false);
  const rowRef = useRef<HTMLDivElement>(null);
  const s = node.span;
  const failed = !!s.error;
  const running = !s.ended_at;
  const meta = SPAN_META[s.type || ''] || { color: 'var(--fgColor-muted)', icon: DiamondIcon };
  const SpanIcon = meta.icon;
  const iconColor = failed ? 'var(--fgColor-danger)' : meta.color;
  const displayName = s.name.includes(':') ? s.name.slice(s.name.indexOf(':') + 1) : s.name;
  const extraData = !!s.data && Object.keys(s.data).length > 0;
  const hasData = spanHasDetails(s);
  const childExpandable = node.children.some(c => spanHasDetails(c.span));

  const revealed = !!reveal && s.span_id === reveal.spanId;
  useEffect(() => {
    if (!revealed || !rowRef.current) return;
    if (hasData) setOpen(true);
    rowRef.current.scrollIntoView({ behavior: 'smooth', block: 'center' });
    const el = rowRef.current;
    el.classList.add('trace-span-revealed');
    const t = window.setTimeout(() => el.classList.remove('trace-span-revealed'), 1800);
    return () => window.clearTimeout(t);
  }, [revealed, hasData, reveal?.nonce]);

  let bar: { left: string; width: string } | null = null;
  if (range && s.started_at) {
    const a = new Date(s.started_at).getTime();
    const b = s.ended_at ? new Date(s.ended_at).getTime() : a;
    bar = {
      left: (((a - range.t0) / range.total) * 100).toFixed(1) + '%',
      width: Math.max(((b - a) / range.total) * 100, 1.5).toFixed(1) + '%',
    };
  }

  return (
    <>
      <div
        ref={rowRef}
        className={'trace-span' + (hasData ? ' trace-span-clickable' : '')}
        style={{ paddingLeft: 2 + depth * 10 }}
        onClick={hasData ? () => setOpen(o => !o) : undefined}
      >
        {(hasData || alignChevron) && (
          <span className={'trace-span-chevron' + (open ? ' open' : '')}>
            {hasData && <ChevronRightIcon size={10} />}
          </span>
        )}
        <span className="trace-ev-icon" style={{ color: iconColor }}><SpanIcon size={12} /></span>
        <span className={'trace-span-name' + (failed ? ' trace-span-failed' : '')} title={s.name}>{displayName}</span>
        {s.type && <span className="trace-ev-tag trace-ev-tag-span">{s.type}</span>}
        {failed && <span className="trace-ev-tag trace-ev-tag-error">error</span>}
        {s.type === 'generation' && s.data && s.data.input_tokens !== undefined && (
          <span className="trace-ev-tokens">
            <span>{'↑' + Number(s.data.input_tokens || 0)}</span>
            <span>{'↓' + Number(s.data.output_tokens || 0)}</span>
          </span>
        )}
        {s.type === 'compaction' && s.data && s.data.before_items !== undefined && (
          <span className="trace-ev-detail">{Number(s.data.before_items) + '→' + Number(s.data.after_items) + ' items'}</span>
        )}
        {running && <span className="trace-span-live-dot" />}
        {s.duration && <span className="trace-span-duration">{s.duration}</span>}
        {bar && (
          <span className="trace-span-track">
            <span className={'trace-span-bar' + (running ? ' live' : '')} style={{ left: bar.left, width: bar.width, background: iconColor }} />
          </span>
        )}
      </div>
      {open && failed && (
        <div className="trace-span-error" style={{ marginLeft: 14 + depth * 10 }}>{s.error}</div>
      )}
      {open && s.data && extraData && (
        s.type === 'generation' && (s.data.input !== undefined || s.data.output !== undefined)
          ? <GenerationPayload data={s.data} indent={14 + depth * 10} />
          : s.type === 'function' && (s.data.input !== undefined || s.data.output !== undefined)
            ? <FunctionPayload data={s.data} indent={14 + depth * 10} />
            : <pre className="trace-span-data" style={{ marginLeft: 14 + depth * 10 }}>
                {JSON.stringify(s.data, null, 2)}
              </pre>
      )}
      {node.children.map((c, i) => <SpanRow key={c.span.span_id || i} node={c} depth={depth + 1} range={range} alignChevron={childExpandable} reveal={reveal} />)}
    </>
  );
}

/* ---------- per-run card ---------- */

// One run's events inside a trace card. A card usually holds a single run,
// but a conversation exchange that spawned background tasks also pulls in the
// wake-up runs their results triggered — each segment keeps its own waterfall
// timeline (the runs are minutes apart; one shared scale would be unreadable).
export interface TraceRunSegment {
  runId: string;
  events: TraceEventData[];
  // label, when set, renders a small heading above the segment — the task
  // panel names each attempt of a retried task with it. Absent (the chat
  // drawer), segments render unlabeled as before.
  label?: string;
}

interface TraceRunProps {
  runId: string;
  segments: TraceRunSegment[];
  label: string;
  // stale marks a run on a branch the session has moved away from.
  stale?: boolean;
  isLive: boolean;
  isExpanded: boolean;
  onToggle: () => void;
  // onJump scrolls the chat to this run's user message; absent when the
  // conversation has no message for the run.
  onJump?: () => void;
  // reveal is the span to open and scroll to (a Context panel jump).
  reveal?: SpanReveal;
}

export function TraceRun({ segments, label, stale, isLive, isExpanded, onToggle, onJump, reveal }: TraceRunProps) {
  const ref = useRef<HTMLDivElement>(null);

  const { parts, tokens, spanCount } = useMemo(() => {
    let inp = 0, out = 0, count = 0;
    const parts = segments.map(seg => {
      const spanEvents = seg.events.filter(ev => ev.kind === 'span');
      count += spanEvents.length;
      for (const ev of spanEvents) {
        if (ev.type === 'generation' && ev.data) {
          inp += Number(ev.data.input_tokens) || 0;
          out += Number(ev.data.output_tokens) || 0;
        }
      }
      return {
        runId: seg.runId,
        label: seg.label,
        spanRoots: buildSpanTree(spanEvents),
        range: spanTimeRange(spanEvents),
      };
    });
    return { parts, tokens: inp > 0 ? { input: inp, output: out } : null, spanCount: count };
  }, [segments]);

  useEffect(() => {
    if (isExpanded && ref.current) {
      ref.current.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    }
  }, [isExpanded]);

  const headerLabel = (
    <>
      <span className="trace-run-label">{label}</span>
      {stale && <span className="trace-run-stale" title="This answer was regenerated; the session is on another attempt">replaced</span>}
      {isLive && <span className="trace-tab-live" />}
      {onJump && (
        <span
          className="trace-run-jump"
          role="button"
          title="Jump to message"
          onClick={e => { e.stopPropagation(); onJump(); }}
        >
          <CommentIcon size={12} />
        </span>
      )}
      {tokens && (
        <span className="trace-run-tokens">
          {(tokens.input + tokens.output).toLocaleString() + ' tok'}
        </span>
      )}
      <CounterLabel>{spanCount}</CounterLabel>
    </>
  );

  return (
    <Disclosure
      ref={ref}
      variant="default"
      // div header: the jump control is a nested interactive element, which
      // is invalid inside the default <button> header.
      as="div"
      label={headerLabel}
      open={isExpanded}
      onToggle={onToggle}
      className="trace-run"
    >
      {spanCount === 0 && <div className="trace-empty">No trace events.</div>}
      {parts.map(p => (
        <div key={p.runId} className="trace-run-segment">
          {p.label && <div className="trace-segment-label">{p.label}</div>}
          {p.spanRoots.map((n, i) => <SpanRow key={n.span.span_id || i} node={n} depth={0} range={p.range} alignChevron={p.spanRoots.some(r => spanHasDetails(r.span))} reveal={reveal} />)}
        </div>
      ))}
    </Disclosure>
  );
}

interface TraceDrawerProps {
  traceRuns: Record<string, TraceEventData[]>;
  liveRunId: string | null;
  activeRunId: string | null;
  runLabels: Record<string, string>;
  // Runs belonging to an abandoned branch — the answer was regenerated and the
  // session moved on. Listed, but marked: their work is real history, it is
  // just not the conversation as it currently stands.
  staleRuns?: Set<string>;
  // runParents maps a wake-up run (auto-started by a task result) to the run
  // whose spawn_task originated it; the chain renders as ONE card.
  runParents?: Record<string, string>;
  onClose: () => void;
  // onJumpToRun scrolls the chat to the run's user message; messageRunIds
  // lists the runs that actually have one, gating the jump control.
  onJumpToRun?: (runId: string) => void;
  messageRunIds?: Set<string>;
  // reveal is a Context panel jump: open the card holding this run and take
  // the reader to the span. A new object identity re-fires it, so clicking the
  // same item twice works.
  reveal?: TraceReveal;
}

export function TraceDrawer({ traceRuns, liveRunId, activeRunId, runLabels, staleRuns, runParents, onClose, onJumpToRun, messageRunIds, reveal }: TraceDrawerProps) {
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  // One card per conversation exchange: a run plus the wake-up runs its tasks
  // triggered, in chronological (insertion) order. rootOf routes expand/live
  // state for any run in a chain to the card that hosts it. A parent missing
  // from traceRuns (e.g. trimmed by retention) leaves the wake run as its own
  // top-level card.
  const { groups, rootOf } = useMemo(() => {
    const ids = Object.keys(traceRuns);
    const children: Record<string, string[]> = {};
    const roots: string[] = [];
    for (const rid of ids) {
      const parent = runParents ? runParents[rid] : undefined;
      if (parent && parent !== rid && traceRuns[parent]) {
        if (!children[parent]) children[parent] = [];
        children[parent].push(rid);
      } else {
        roots.push(rid);
      }
    }
    const groups: Array<{ rootId: string; segments: TraceRunSegment[] }> = [];
    const rootOf: Record<string, string> = {};
    const seen = new Set<string>();
    for (const root of roots) {
      const segments: TraceRunSegment[] = [];
      const visit = (rid: string) => {
        if (seen.has(rid)) return;
        seen.add(rid);
        rootOf[rid] = root;
        segments.push({ runId: rid, events: traceRuns[rid] || [] });
        for (const c of children[rid] || []) visit(c);
      };
      visit(root);
      groups.push({ rootId: root, segments });
    }
    return { groups, rootOf };
  }, [traceRuns, runParents]);

  useEffect(() => {
    if (activeRunId && traceRuns[activeRunId]) {
      const root = rootOf[activeRunId] || activeRunId;
      setExpanded(prev => prev[root] ? prev : { ...prev, [root]: true });
    }
  }, [activeRunId, traceRuns, rootOf]);

  // Auto-expand the live run's card so in-flight spans are visible as they
  // stream in — for a wake-up run that is the card of its originating run.
  useEffect(() => {
    if (liveRunId) {
      const root = rootOf[liveRunId] || liveRunId;
      setExpanded(prev => prev[root] ? prev : { ...prev, [root]: true });
    }
  }, [liveRunId, rootOf]);

  // A revealed span's card must be open before the row can scroll itself in.
  useEffect(() => {
    if (!reveal) return;
    const root = rootOf[reveal.runId] || reveal.runId;
    setExpanded(prev => prev[root] ? prev : { ...prev, [root]: true });
  }, [reveal, rootOf]);

  const toggle = (rid: string) => setExpanded(prev => ({ ...prev, [rid]: !prev[rid] }));

  return (
    <SidePanel icon={PulseIcon} title="Traces" count={groups.length} onClose={onClose} storageKey="inspectorWidth">
      {groups.length === 0 && (
        <div className="trace-empty">No traces yet.</div>
      )}
      {groups.map(({ rootId, segments }) => (
        <TraceRun
          key={rootId}
          runId={rootId}
          segments={segments}
          label={(runLabels && runLabels[rootId]) || rootId.slice(0, 8)}
          stale={staleRuns?.has(rootId)}
          isLive={segments.some(s => s.runId === liveRunId)}
          isExpanded={!!expanded[rootId]}
          onToggle={() => toggle(rootId)}
          onJump={onJumpToRun && messageRunIds && messageRunIds.has(rootId) ? () => onJumpToRun(rootId) : undefined}
          reveal={reveal && segments.some(s => s.runId === reveal.runId) ? reveal : undefined}
        />
      ))}
    </SidePanel>
  );
}
