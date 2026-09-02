import './trace.css';
import { useState, useEffect, useMemo, useRef } from 'react';
import { CounterLabel, Link } from '@primer/react';
import {
  PulseIcon, ToolsIcon, ArrowSwitchIcon, DiamondIcon,
  DependabotIcon, CpuIcon, ShieldCheckIcon, ChevronRightIcon,
  CommentIcon,
} from '@primer/octicons-react';
import type { Icon } from '@primer/octicons-react';
import { SidePanel } from '@/layout/SidePanel';
import { Disclosure } from '@/components/Disclosure';
import { useChatActions, useChatSession } from '@/features/chat/ChatSessionContext';
import { ReplayDialog } from '@/features/chat/ReplayDialog';
import { PayloadItem, itemTag, itemText, payloadItems, prettyMaybeJSON, type PayloadRecord } from '@/features/chat/TracePayload';

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
  // The payload fields (input, output, …) were left out of data — by the
  // summary listing, or by the live cap — and load on open from the stored
  // row (ChatActions.loadSpan).
  payloadOmitted?: boolean;
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

/* ---------- span payloads ---------- */

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
        <Link as="button" onClick={() => setReplayOpen(true)} style={{ flexShrink: 0, fontSize: 'var(--base-text-size-xs)' }}>
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
// (payload, counts), a payload left out of the listing is details to fetch,
// and errors always expand.
function spanHasDetails(s: TraceEventData): boolean {
  return !!s.error || !!s.payloadOmitted || !!(s.data && Object.keys(s.data).length > 0);
}

// alignChevron: reserve the chevron slot even without details, so icons line
// up when siblings on the same level are expandable.
// loadSpan fetches the row's payload when the listing left it out; opening the
// row asks once, and the parent swaps the whole span in.
function SpanRow({ node, depth, range, alignChevron, loadSpan }: { node: SpanNode; depth: number; range: TimeRange | null; alignChevron: boolean; loadSpan?: (spanId: string) => Promise<void> }) {
  const [open, setOpen] = useState(false);
  // The payload fetch of an opened row: pending, done, or failed — a live span
  // not yet ended has no stored row. Reset on close, so reopening asks again;
  // never asked twice while open, whatever the answer.
  const [payload, setPayload] = useState<'idle' | 'loading' | 'loaded' | 'failed'>('idle');
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

  const spanId = s.span_id;
  const omitted = !!s.payloadOmitted;
  useEffect(() => {
    if (!open || !omitted || !loadSpan || !spanId || payload !== 'idle') return;
    setPayload('loading');
    loadSpan(spanId).then(() => setPayload('loaded'), () => setPayload('failed'));
  }, [open, omitted, spanId, loadSpan, payload]);
  const toggle = () => { setOpen(o => !o); setPayload('idle'); };

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
        style={{ paddingLeft: 2 + depth * 8 }}
        role={hasData ? 'button' : undefined}
        tabIndex={hasData ? 0 : undefined}
        aria-expanded={hasData ? open : undefined}
        onClick={hasData ? toggle : undefined}
        onKeyDown={hasData ? e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(); } } : undefined}
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
        <div className="trace-span-error" style={{ marginLeft: 14 + depth * 8 }}>{s.error}</div>
      )}
      {open && omitted && payload === 'loading' && (
        <div className="trace-span-note" style={{ marginLeft: 14 + depth * 8 }}>Loading the payload…</div>
      )}
      {open && omitted && payload === 'failed' && (
        <div className="trace-span-note" style={{ marginLeft: 14 + depth * 8 }}>The payload is not stored yet — a span still running has no row; reopen once it ends.</div>
      )}
      {open && s.data && extraData && !(omitted && payload === 'loading') && (
        s.type === 'generation' && (s.data.input !== undefined || s.data.output !== undefined)
          ? <GenerationPayload data={s.data} indent={14 + depth * 8} />
          : s.type === 'function' && (s.data.input !== undefined || s.data.output !== undefined)
            ? <FunctionPayload data={s.data} indent={14 + depth * 8} />
            : <pre className="trace-span-data" style={{ marginLeft: 14 + depth * 8 }}>
                {JSON.stringify(s.data, null, 2)}
              </pre>
      )}
      {node.children.map((c, i) => <SpanRow key={c.span.span_id || i} node={c} depth={depth + 1} range={range} alignChevron={childExpandable} loadSpan={loadSpan} />)}
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
  // payloadSessionId is the session whose stored rows hold these spans'
  // payload — the chat's own by default; an inspected task's child session
  // for the task inspector.
  payloadSessionId?: string;
}

export function TraceRun({ segments, label, stale, isLive, isExpanded, onToggle, onJump, payloadSessionId }: TraceRunProps) {
  const ref = useRef<HTMLDivElement>(null);
  const { loadSpan } = useChatActions();
  const { sessionId } = useChatSession();
  const payloadSession = payloadSessionId || sessionId;

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
        loadSpan: loadSpan && payloadSession ? (spanId: string) => loadSpan(payloadSession, seg.runId, spanId) : undefined,
      };
    });
    return { parts, tokens: inp > 0 ? { input: inp, output: out } : null, spanCount: count };
  }, [segments, loadSpan, payloadSession]);

  useEffect(() => {
    if (isExpanded && ref.current) {
      ref.current.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    }
  }, [isExpanded]);

  const spanCountText = spanCount + (spanCount === 1 ? ' span' : ' spans');
  const headerLabel = (
    <>
      <span className="trace-run-label">{label}</span>
      {stale && <span className="trace-run-stale" title="This answer was regenerated; the session is on another attempt">replaced</span>}
      {isLive && <span className="trace-tab-live" />}
      {onJump && (
        <span
          className="trace-run-jump"
          role="button"
          tabIndex={0}
          title="Jump to message"
          aria-label="Jump to message"
          onClick={e => { e.stopPropagation(); onJump(); }}
          onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); e.stopPropagation(); onJump(); } }}
        >
          <CommentIcon size={12} />
        </span>
      )}
      {tokens && (
        <span className="trace-run-tokens">
          {(tokens.input + tokens.output).toLocaleString() + ' tok'}
        </span>
      )}
      <CounterLabel title={spanCountText} aria-label={spanCountText}>{spanCount}</CounterLabel>
    </>
  );

  return (
    <Disclosure
      ref={ref}
      variant="default"
      // div header: the jump control nests inside it, which a <button> header
      // cannot hold; Disclosure keeps the div a keyboard-operable role=button.
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
          {p.spanRoots.map((n, i) => <SpanRow key={n.span.span_id || i} node={n} depth={0} range={p.range} alignChevron={p.spanRoots.some(r => spanHasDetails(r.span))} loadSpan={p.loadSpan} />)}
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
}

export function TraceDrawer({ traceRuns, liveRunId, activeRunId, runLabels, staleRuns, runParents, onClose, onJumpToRun, messageRunIds }: TraceDrawerProps) {
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
        />
      ))}
    </SidePanel>
  );
}
