import './trace.css';
import { useState, useEffect, useMemo, useRef } from 'react';
import { Button, Checkbox, CounterLabel, Dialog, Flash, Link, Select, Textarea, TextInput } from '@primer/react';
import { api } from '@/lib/api';
import {
  PulseIcon, ToolsIcon, ArrowSwitchIcon, DiamondIcon,
  DependabotIcon, CpuIcon, ShieldCheckIcon, ChevronRightIcon,
  CommentIcon,
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
  error?: string;
  started_at?: string;
  ended_at?: string;
  data?: Record<string, unknown> | null;
  duration?: string;
}

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
    const texts = content
      .map(p => (p && typeof p === 'object' ? (p as PayloadRecord).text : null))
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

// ReplayDialog is a two-pane playground for one traced model call: the left
// pane edits the request (instructions, settings, input items — seeded from
// the trace), the right pane shows the replay result next to the original
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
  const [includeTools, setIncludeTools] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [result, setResult] = useState<{ output: PayloadRecord[]; usage?: PayloadRecord; duration_ms?: number } | null>(null);

  const tools = useMemo(() => payloadItems(data.tools), [data.tools]);
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
      const match = (list || []).find(a => a.name === data.name);
      setAgentId(String(match ? match.id : (list && list[0] ? list[0].id : '')));
    }).catch(() => {});
  }, [data.name]);

  const run = async () => {
    if (!itemsParsed.value) return;
    setError('');
    setResult(null);
    setBusy(true);
    try {
      const res = await api.playground.generate({
        agent_config_id: agentId,
        model: model.trim() || undefined,
        system_instructions: instructions,
        input_items: itemsParsed.value,
        model_settings: settingsParsed.value,
        tools: includeTools && tools.length > 0 ? tools : undefined,
      });
      setResult({ output: payloadItems(res.output), usage: res.usage, duration_ms: res.duration_ms });
    } catch (e) {
      setError((e as Error).message);
    }
    setBusy(false);
  };

  const usage = result && result.usage ? result.usage : null;
  const replayMeta = result
    ? [
        result.duration_ms !== undefined ? result.duration_ms + 'ms' : null,
        usage ? '↑' + Number(usage.input_tokens || 0) + ' ↓' + Number(usage.output_tokens || 0) + ' tok' : null,
      ].filter(Boolean).join(' · ')
    : '';

  return (
    <Dialog
      title="Replay generation"
      onClose={onClose}
      // Primer's named sizes cap out well below what a request/response
      // editor needs; explicit style wins over both (same as SettingsDialog).
      height="large"
      style={{ width: 'min(1200px, calc(100vw - 48px))', height: 'calc(100vh - 96px)' }}
      // The scroll wrapper between dialog and body is not a flex container,
      // so the body must claim the height explicitly for the panes to fill.
      renderBody={({ children }) => (
        <Dialog.Body style={{ display: 'flex', height: '100%' }}>{children}</Dialog.Body>
      )}
    >
      <div className="trace-replay">
        <div className="trace-replay-toolbar">
          <Select value={agentId} onChange={e => setAgentId(e.target.value)} className="trace-replay-agent">
            {agents.map(a => <Select.Option key={a.id} value={String(a.id)}>{a.name}</Select.Option>)}
          </Select>
          <TextInput value={model} onChange={e => setModel(e.target.value)} placeholder="model (agent default)" className="trace-replay-model" />
          {tools.length > 0 && (
            <label className="trace-replay-tools-toggle">
              <Checkbox checked={includeTools} onChange={e => setIncludeTools(e.target.checked)} />
              {'Tools (' + tools.length + ')'}
            </label>
          )}
          <Button
            variant="primary"
            onClick={() => void run()}
            disabled={busy || !agentId || !!itemsParsed.error || !!settingsParsed.error}
            className="trace-replay-run"
          >
            {busy ? 'Running…' : 'Run'}
          </Button>
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
              <Link
                as="button"
                className="trace-replay-format"
                onClick={() => { if (itemsParsed.value) setItems(JSON.stringify(itemsParsed.value, null, 2)); }}
              >
                Format
              </Link>
            </div>
            <div className="trace-replay-fill">
              <Textarea value={items} onChange={e => setItems(e.target.value)} block resize="none" />
            </div>
          </div>
          <div className="trace-replay-col">
            {error && <Flash variant="danger" className="trace-replay-error">{error}</Flash>}
            <div className="trace-replay-sec">
              Replay response
              {replayMeta && <span className="trace-replay-hint">{replayMeta}</span>}
            </div>
            <div className="trace-replay-outbox trace-payload-list">
              {busy
                ? <div className="trace-empty">Running…</div>
                : result
                  ? <ResponseItems items={result.output} prefix="rp-" />
                  : <div className="trace-empty">Edit the request on the left, then Run.</div>}
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
function SpanRow({ node, depth, range, alignChevron }: { node: SpanNode; depth: number; range: TimeRange | null; alignChevron: boolean }) {
  const [open, setOpen] = useState(false);
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
      {node.children.map((c, i) => <SpanRow key={c.span.span_id || i} node={c} depth={depth + 1} range={range} alignChevron={childExpandable} />)}
    </>
  );
}

/* ---------- per-run card ---------- */

interface TraceRunProps {
  runId: string;
  events: TraceEventData[];
  label: string;
  isLive: boolean;
  isExpanded: boolean;
  onToggle: () => void;
  // onJump scrolls the chat to this run's user message; absent when the
  // conversation has no message for the run.
  onJump?: () => void;
}

function TraceRun({ events, label, isLive, isExpanded, onToggle, onJump }: TraceRunProps) {
  const ref = useRef<HTMLDivElement>(null);

  const { spanRoots, range, tokens } = useMemo(() => {
    const spanEvents = events.filter(ev => ev.kind === 'span');
    let inp = 0, out = 0;
    for (const ev of spanEvents) {
      if (ev.type === 'generation' && ev.data) {
        inp += Number(ev.data.input_tokens) || 0;
        out += Number(ev.data.output_tokens) || 0;
      }
    }
    return {
      spanRoots: buildSpanTree(spanEvents),
      range: spanTimeRange(spanEvents),
      tokens: inp > 0 ? { input: inp, output: out } : null,
    };
  }, [events]);

  useEffect(() => {
    if (isExpanded && ref.current) {
      ref.current.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    }
  }, [isExpanded]);

  const headerLabel = (
    <>
      <span className="trace-run-label">{label}</span>
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
      <CounterLabel>{events.length}</CounterLabel>
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
      {spanRoots.length === 0 && <div className="trace-empty">No trace events.</div>}
      {spanRoots.map((n, i) => <SpanRow key={n.span.span_id || i} node={n} depth={0} range={range} alignChevron={spanRoots.some(r => spanHasDetails(r.span))} />)}
    </Disclosure>
  );
}

interface TraceDrawerProps {
  traceRuns: Record<string, TraceEventData[]>;
  liveRunId: string | null;
  activeRunId: string | null;
  runLabels: Record<string, string>;
  onClose: () => void;
  // onJumpToRun scrolls the chat to the run's user message; messageRunIds
  // lists the runs that actually have one, gating the jump control.
  onJumpToRun?: (runId: string) => void;
  messageRunIds?: Set<string>;
}

export function TraceDrawer({ traceRuns, liveRunId, activeRunId, runLabels, onClose, onJumpToRun, messageRunIds }: TraceDrawerProps) {
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const runIds = useMemo(() => Object.keys(traceRuns), [traceRuns]);

  useEffect(() => {
    if (activeRunId && traceRuns[activeRunId]) {
      setExpanded(prev => ({ ...prev, [activeRunId]: true }));
    }
  }, [activeRunId, traceRuns]);

  // Auto-expand the live run so in-flight spans are visible as they stream in.
  useEffect(() => {
    if (liveRunId) {
      setExpanded(prev => prev[liveRunId] ? prev : { ...prev, [liveRunId]: true });
    }
  }, [liveRunId]);

  const toggle = (rid: string) => setExpanded(prev => ({ ...prev, [rid]: !prev[rid] }));

  return (
    <SidePanel icon={PulseIcon} title="Traces" count={runIds.length} onClose={onClose} storageKey="tracePanelWidth">
      {runIds.length === 0 && (
        <div className="trace-empty">No traces yet.</div>
      )}
      {runIds.map(rid => (
        <TraceRun
          key={rid}
          runId={rid}
          events={traceRuns[rid] || []}
          label={(runLabels && runLabels[rid]) || rid.slice(0, 8)}
          isLive={rid === liveRunId}
          isExpanded={!!expanded[rid]}
          onToggle={() => toggle(rid)}
          onJump={onJumpToRun && messageRunIds && messageRunIds.has(rid) ? () => onJumpToRun(rid) : undefined}
        />
      ))}
    </SidePanel>
  );
}
