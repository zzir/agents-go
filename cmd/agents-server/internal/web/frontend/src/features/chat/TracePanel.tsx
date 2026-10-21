import './trace.css';
import { useState, useEffect, useMemo, useRef, type CSSProperties, type ReactNode } from 'react';
import { CounterLabel, IconButton, Link } from '@primer/react';
import {
  PulseIcon, ToolsIcon, ArrowSwitchIcon, DiamondIcon,
  DependabotIcon, CpuIcon, ShieldCheckIcon,
  CommentIcon, PlugIcon, TerminalIcon, SyncIcon,
  ArrowLeftIcon,
} from '@primer/octicons-react';
import type { Icon } from '@primer/octicons-react';
import { SidePanel } from '@/layout/SidePanel';
import { Loading } from '@/components/Loading';
import { activates } from '@/lib/activation';
import { useChatActions, useChatSession } from '@/features/chat/ChatSessionContext';
import { ReplayDialog } from '@/features/chat/ReplayDialog';
import { PayloadItem, payloadEntry, payloadItems, prettyMaybeJSON, toolOutputEntry, type PayloadRecord } from '@/features/chat/TracePayload';
import { fmtDuration } from '@/lib/background';
import type { AttachmentMeta } from '@/lib/attachments';
import type { TraceMarker } from '@/lib/queuedInputs';

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
  // The payload fields (input, output, …) left out of data by the summary
  // listing or the live cap; they load on open from the stored row
  // (ChatActions.loadSpan).
  payloadOmitted?: boolean;
  // The image attachments the span's input items reference, resolved by the
  // server (the items themselves keep the stored reference).
  attachments?: AttachmentMeta[];
}

// lastGenerationSpan is a run's newest model call: what a replay reopens and
// what the trace lands on when opened from the run's turn.
export function lastGenerationSpan(events?: TraceEventData[]): TraceEventData | undefined {
  if (!events) return undefined;
  for (let i = events.length - 1; i >= 0; i--) {
    if (events[i].kind === 'span' && events[i].type === 'generation') return events[i];
  }
  return undefined;
}

// Span type → icon + color, mirroring the SDK's typed span constructors.
const SPAN_META: Record<string, { color: string; icon: Icon }> = {
  agent:      { color: 'var(--fgColor-open)',      icon: DependabotIcon },
  generation: { color: 'var(--fgColor-accent)',    icon: CpuIcon },
  function:   { color: 'var(--fgColor-done)',      icon: ToolsIcon },
  handoff:    { color: 'var(--fgColor-severe)',    icon: ArrowSwitchIcon },
  guardrail:  { color: 'var(--fgColor-attention)', icon: ShieldCheckIcon },
  compaction: { color: 'var(--fgColor-attention)', icon: DiamondIcon },
  mcp:        { color: 'var(--fgColor-muted)',     icon: PlugIcon },
  sandbox:    { color: 'var(--fgColor-muted)',     icon: TerminalIcon },
  model_retry: { color: 'var(--fgColor-attention)', icon: SyncIcon },
};
const FALLBACK_META = { color: 'var(--fgColor-muted)', icon: DiamondIcon };

// spanColor is a span's color on the waterfall: its type's, or danger once it failed.
function spanColor(s: TraceEventData): string {
  return s.error ? 'var(--fgColor-danger)' : (SPAN_META[s.type || ''] || FALLBACK_META).color;
}

// Structured view of a function span's data: the tool call's arguments and
// its stringified result — a multimodal result as its text and pictures.
function FunctionPayload({ data, indent }: { data: PayloadRecord; indent: number }) {
  const out = typeof data.output === 'string' ? toolOutputEntry(data.output) : null;
  return (
    <div className="trace-payload" style={{ marginLeft: indent }}>
      <div className="trace-payload-list">
        {data.input !== undefined && (
          <PayloadItem tag="input" text={typeof data.input === 'string' ? data.input : JSON.stringify(data.input)} full={prettyMaybeJSON(data.input)} />
        )}
        {data.output !== undefined && (out
          ? <PayloadItem tag="output" text={out.text} full={out.images.length > 0 ? out.text : prettyMaybeJSON(data.output)} images={out.images} />
          : <PayloadItem tag="output" text={JSON.stringify(data.output)} full={prettyMaybeJSON(data.output)} />)}
      </div>
    </div>
  );
}

// Structured view of a generation span's data: the exact request body the
// model received (instructions, tools, settings, items) and the items it returned.
function GenerationPayload({ data, attachments, indent }: { data: PayloadRecord; attachments?: AttachmentMeta[]; indent: number }) {
  const [replayOpen, setReplayOpen] = useState(false);
  const input = payloadItems(data.input);
  const output = payloadItems(data.output);
  const tools = payloadItems(data.tools);
  const handoffs = payloadItems(data.handoffs);
  const settingsRaw = data.model_settings && typeof data.model_settings === 'object' ? data.model_settings as PayloadRecord : null;
  const settings = settingsRaw && Object.keys(settingsRaw).length > 0 ? settingsRaw : null;
  const outputSchema = data.output_schema && typeof data.output_schema === 'object' ? data.output_schema as PayloadRecord : null;
  const instructions = typeof data.system_instructions === 'string' ? data.system_instructions : '';
  const prompt = data.prompt && typeof data.prompt === 'object' ? data.prompt as PayloadRecord : null;
  const promptID = prompt ? String(prompt.id ?? prompt.ID ?? '') : '';
  const promptVersion = prompt ? String(prompt.version ?? prompt.Version ?? '') : '';
  const partial = typeof data.partial_text === 'string' ? data.partial_text : '';
  const str = (k: string) => (typeof data[k] === 'string' ? data[k] as string : '');
  const num = (k: string) => (typeof data[k] === 'number' ? data[k] as number : null);
  // The model that answered when it is not the one asked for: an alias
  // resolved, or a fallback taken.
  const model = str('model'), used = str('model_used'), status = str('status');
  const meta = [
    used && used !== model ? (model ? model + ' → ' + used : used) : model || null,
    num('time_to_first_token_ms') !== null ? 'ttft ' + num('time_to_first_token_ms') + 'ms' : null,
    num('cached_tokens') !== null ? 'cached ' + num('cached_tokens') : null,
    num('cache_write_tokens') !== null ? 'cache write ' + num('cache_write_tokens') : null,
    num('reasoning_tokens') !== null ? 'reasoning ' + num('reasoning_tokens') : null,
    status && status !== 'completed' ? status + (str('incomplete_reason') ? ' (' + str('incomplete_reason') + ')' : '') : null,
    num('fallback_index') !== null ? 'fallback #' + num('fallback_index') : null,
    str('request_id') ? 'req: ' + str('request_id') : null,
    str('previous_response_id') ? 'prev: ' + str('previous_response_id') : null,
    str('conversation_id') ? 'conv: ' + str('conversation_id') : null,
  ].filter(Boolean).join(' · ');

  return (
    <div className="trace-payload" style={{ marginLeft: indent }}>
      <div className="trace-payload-meta">
        <span className="trace-payload-meta-text" title={meta}>{meta}</span>
        <Link as="button" onClick={() => setReplayOpen(true)} style={{ flexShrink: 0, fontSize: 'var(--base-text-size-xs)' }}>
          Replay
        </Link>
      </div>
      {replayOpen && <ReplayDialog data={data} attachments={attachments} onClose={() => setReplayOpen(false)} />}
      {/* One shared grid for both sections so the tag column width (and thus
          the preview start) is identical across Request and Response. */}
      <div className="trace-payload-list">
        <div className="trace-section-label">Request</div>
        {instructions && <PayloadItem tag="system" text={instructions} full={instructions} />}
        {prompt && <PayloadItem tag="prompt" text={promptID + (promptVersion ? '@' + promptVersion : '')} full={JSON.stringify(prompt, null, 2)} />}
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
        {input.map((item, i) => <PayloadItem key={'in-' + i} {...payloadEntry(item, attachments)} />)}
        {typeof data.input === 'string' && <div className="trace-payload-preview">{String(data.input)}</div>}
        <div className="trace-section-label">Response</div>
        {output.map((item, i) => <PayloadItem key={'out-' + i} {...payloadEntry(item)} />)}
        {partial && <PayloadItem tag="partial" text={partial} full={partial} />}
        {typeof data.output === 'string' && <div className="trace-payload-preview">{String(data.output)}</div>}
      </div>
    </div>
  );
}

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

// splitEpisodes cuts a run's roots where it stopped and went on (a root agent
// span no handoff led to); a non-agent root (a before-run compaction) goes with
// the agent after it.
function splitEpisodes(roots: SpanNode[]): SpanNode[][] {
  const episodes: SpanNode[][] = [];
  let pending: SpanNode[] = [];
  let lastAgent: SpanNode | undefined;
  for (const root of roots) {
    if (root.span.type !== 'agent') { pending.push(root); continue; }
    if (!lastAgent?.children.some(c => c.span.type === 'handoff')) episodes.push([]);
    episodes[episodes.length - 1].push(...pending, root);
    pending = [];
    lastAgent = root;
  }
  if (pending.length) {
    if (episodes.length === 0) episodes.push([]);
    episodes[episodes.length - 1].push(...pending);
  }
  return episodes;
}

function episodeSpans(roots: SpanNode[]): TraceEventData[] {
  const out: TraceEventData[] = [];
  const walk = (n: SpanNode) => { out.push(n.span); n.children.forEach(walk); };
  roots.forEach(walk);
  return out;
}

interface TimeRange {
  t0: number;
  total: number;
}

// spanExtent is a span's [start, end] in ms; a span still running ends where it
// started.
function spanExtent(s: TraceEventData): [number, number] | null {
  if (!s.started_at) return null;
  const a = new Date(s.started_at).getTime();
  return [a, s.ended_at ? new Date(s.ended_at).getTime() : a];
}

function spanTimeRange(spans: TraceEventData[]): TimeRange | null {
  let t0 = Infinity, t1 = -Infinity;
  for (const s of spans) {
    const e = spanExtent(s);
    if (!e) continue;
    if (e[0] < t0) t0 = e[0];
    if (e[1] > t1) t1 = e[1];
  }
  if (!isFinite(t0)) return null;
  return { t0, total: Math.max(t1 - t0, 1) };
}

// barGeometry places an extent on the track: a bar in CSS percentages, or a
// tick (CSS-sized) when the extent is under 1% of the range.
function barGeometry(range: TimeRange, [a, b]: [number, number]): { left: string; width?: string; tick: boolean } {
  const left = (((a - range.t0) / range.total) * 100).toFixed(2) + '%';
  const w = ((b - a) / range.total) * 100;
  return w < 1 ? { left, tick: true } : { left, width: w.toFixed(2) + '%', tick: false };
}

// tickStep is the interval of the time column's faint lines: the smallest
// round step that fits the range in six or fewer.
const TICK_STEPS = [100, 200, 500, 1000, 2000, 5000, 10000, 20000, 30000, 60000, 120000, 300000, 600000, 900000, 1800000, 3600000];
function tickStep(total: number): number {
  return TICK_STEPS.find(step => total / step <= 6) ?? TICK_STEPS[TICK_STEPS.length - 1];
}

// spanHasDetails reports whether a span row can expand: any data at all (the
// server strips content-free data), a payload left out of the listing, or an error.
function spanHasDetails(s: TraceEventData): boolean {
  return !!s.error || !!s.payloadOmitted || !!(s.data && Object.keys(s.data).length > 0);
}

function SpanRow({ node, depth, range, loadSpan, focusSpanId, markers }: { node: SpanNode; depth: number; range: TimeRange | null; loadSpan?: (spanId: string) => Promise<void>; focusSpanId?: string; markers?: TraceMarker[] }) {
  // The span the trace was opened on starts open and marked.
  const focused = !!focusSpanId && node.span.span_id === focusSpanId;
  const [open, setOpen] = useState(focused);
  const rowRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!focused) return;
    setOpen(true);
    rowRef.current?.scrollIntoView({ block: 'nearest' });
  }, [focused]);
  // The payload fetch of an opened row: pending, done, or failed (a live span
  // has no stored row yet). Reset on close so reopening asks again; never twice
  // while open.
  const [payload, setPayload] = useState<'idle' | 'loading' | 'loaded' | 'failed'>('idle');
  const s = node.span;
  const failed = !!s.error;
  const running = !s.ended_at;
  const SpanIcon = (SPAN_META[s.type || ''] || FALLBACK_META).icon;
  const iconColor = spanColor(s);
  const displayName = s.name.includes(':') ? s.name.slice(s.name.indexOf(':') + 1) : s.name;
  const extraData = !!s.data && Object.keys(s.data).length > 0;
  // A function's mcp child is its transport, the same call over the wire: it
  // folds under the row until opened, and never overlays the bar.
  const transport = (c: SpanNode) => s.type === 'function' && c.span.type === 'mcp';
  const folded = node.children.filter(transport);
  const shown = open ? node.children : node.children.filter(c => !transport(c));
  const hasData = spanHasDetails(s) || folded.length > 0;

  const spanId = s.span_id;
  const omitted = !!s.payloadOmitted;
  useEffect(() => {
    if (!open || !omitted || !loadSpan || !spanId || payload !== 'idle') return;
    setPayload('loading');
    loadSpan(spanId).then(() => setPayload('loaded'), () => setPayload('failed'));
  }, [open, omitted, spanId, loadSpan, payload]);
  const toggle = () => { setOpen(o => !o); setPayload('idle'); };

  const d = s.data || {};
  const num = (k: string) => (typeof d[k] === 'number' ? d[k] as number : null);
  const str = (k: string) => (typeof d[k] === 'string' ? d[k] as string : '');
  // A guardrail span's verdicts: the names it consulted, and the one that ruled.
  const verdicts = s.type === 'guardrail' ? payloadItems(d.guardrails) : [];
  const ruled = verdicts.find(v => v.action === 'replace' || v.action === 'trip');
  const pendingTools = Array.isArray(d.pending_tools) ? d.pending_tools.map(String).join(', ') : '';

  const own = spanExtent(s);
  const bar = range && own ? barGeometry(range, own) : null;
  // The children's extents overlay the parent's bar in their own colors, so a
  // collapsed row still shows the run's shape; a gap is time no child explains.
  const segments = range
    ? node.children.filter(c => !transport(c)).flatMap((c, i) => {
        const e = spanExtent(c.span);
        return e ? [{ key: c.span.span_id || i, color: spanColor(c.span), ...barGeometry(range, e) }] : [];
      })
    : [];

  return (
    <>
      <div
        ref={rowRef}
        className={'trace-span' + (hasData ? ' trace-span-clickable' : '') + (focused ? ' trace-span-focus' : '')}
        style={{ '--d': depth } as CSSProperties}
        role={hasData ? 'button' : undefined}
        tabIndex={hasData ? 0 : undefined}
        aria-expanded={hasData ? open : undefined}
        onClick={hasData ? toggle : undefined}
        onKeyDown={hasData ? e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(); } } : undefined}
      >
        <span className="trace-span-label">
          <span className="trace-ev-icon" style={{ color: iconColor }}><SpanIcon size={12} /></span>
          <span className={'trace-span-name' + (failed ? ' trace-span-failed' : '')} title={s.name}>{displayName}</span>
          {s.type && (s.type === 'agent' || !SPAN_META[s.type]) && <span className="trace-ev-tag trace-ev-tag-span">{s.type}</span>}
          {failed && <span className="trace-ev-tag trace-ev-tag-error">error</span>}
          {s.type === 'generation' && d.input_tokens !== undefined && (
            <span className="trace-ev-tokens">
              {(num('cached_tokens') ?? 0) > 0
                ? <span className="trace-ev-tokens-cached" title={num('cached_tokens') + ' cached input tokens'}>{'↑' + Number(d.input_tokens || 0)}</span>
                : <span>{'↑' + Number(d.input_tokens || 0)}</span>}
              <span>{'↓' + Number(d.output_tokens || 0)}</span>
            </span>
          )}
          {s.type === 'generation' && str('status') === 'incomplete' && (
            <span className="trace-ev-tag trace-ev-tag-attention" title={str('incomplete_reason')}>incomplete</span>
          )}
          {s.type === 'generation' && num('fallback_index') !== null && <span className="trace-ev-tag trace-ev-tag-attention">fallback</span>}
          {s.type === 'compaction' && d.before_items !== undefined && (
            <span className="trace-ev-detail">{Number(d.before_items) + '→' + Number(d.after_items) + ' items'}</span>
          )}
          {s.type === 'compaction' && d.reset === true && <span className="trace-ev-tag trace-ev-tag-attention">reset</span>}
          {s.type === 'handoff' && str('to_agent') && <span className="trace-ev-detail">{'→ ' + str('to_agent')}</span>}
          {s.type === 'sandbox' && num('exit_code') !== null && (
            <span className={'trace-ev-detail' + (num('exit_code') ? ' trace-ev-detail-danger' : '')}>{'exit ' + num('exit_code')}</span>
          )}
          {s.type === 'model_retry' && num('attempt') !== null && (
            <span className="trace-ev-detail">{'attempt ' + num('attempt') + (num('max_attempts') !== null ? '/' + num('max_attempts') : '')}</span>
          )}
          {verdicts.length > 0 && <span className="trace-ev-detail">{verdicts.map(v => String(v.name || '')).join(', ')}</span>}
          {ruled && <span className="trace-ev-tag trace-ev-tag-attention">{ruled.action === 'trip' ? 'tripped' : 'replaced'}</span>}
          {s.type === 'agent' && str('ended_by') === 'interruption' && (
            <span className="trace-ev-tag trace-ev-tag-attention" title={'awaiting approval' + (pendingTools ? ': ' + pendingTools : '')}>paused</span>
          )}
          {s.type === 'agent' && (str('ended_by') === 'stop' || d.stopped_early === true) && <span className="trace-ev-tag">stopped</span>}
          {folded.length > 0 && <span className="trace-span-hint">mcp</span>}
          {running && <span className="trace-span-live-dot" />}
        </span>
        <span className="trace-span-duration">{s.duration}</span>
        <span className="trace-span-track">
          {bar && <span className={'trace-span-bar' + (running ? ' live' : '') + (segments.length ? ' covered' : '') + (bar.tick ? ' tick' : '')} style={{ left: bar.left, width: bar.width, background: iconColor }} />}
          {bar && segments.map(g => <span key={g.key} className={'trace-span-seg' + (g.tick ? ' tick' : '')} style={{ left: g.left, width: g.width, background: g.color }} />)}
        </span>
      </div>
      {open && failed && (
        <div className="trace-span-error" style={{ marginLeft: 14 + depth * 12 }}>{s.error}</div>
      )}
      {open && omitted && payload === 'loading' && (
        <div className="trace-span-note" style={{ marginLeft: 14 + depth * 12 }}>Loading the payload…</div>
      )}
      {open && omitted && payload === 'failed' && (
        <div className="trace-span-note" style={{ marginLeft: 14 + depth * 12 }}>The payload is not stored yet — a span still running has no row; reopen once it ends.</div>
      )}
      {open && s.data && extraData && !(omitted && payload === 'loading') && (
        s.type === 'generation' && (s.data.input !== undefined || s.data.output !== undefined)
          ? <GenerationPayload data={s.data} attachments={s.attachments} indent={14 + depth * 12} />
          : s.type === 'function' && (s.data.input !== undefined || s.data.output !== undefined)
            ? <FunctionPayload data={s.data} indent={14 + depth * 12} />
            : <pre className="trace-span-data" style={{ marginLeft: 14 + depth * 12 }}>
                {JSON.stringify(s.data, null, 2)}
              </pre>
      )}
      {withMarkers(shown, markers || []).map((row, i) => row.marker
        ? <MarkerRow key={'m' + i} marker={row.marker} range={range} depth={depth + 1} />
        : <SpanRow key={row.node!.span.span_id || i} node={row.node!} depth={depth + 1} range={range} loadSpan={loadSpan} focusSpanId={focusSpanId} />)}
    </>
  );
}

// One run's events inside a trace detail. A detail usually holds a single run,
// but an exchange that spawned background tasks also pulls in the wake-up runs
// their results triggered; each segment keeps its own waterfall (the runs are minutes apart).
export interface TraceRunSegment {
  runId: string;
  events: TraceEventData[];
  // label, when set, renders a small heading above the segment — the task
  // panel names each run of a retried task with it; absent, segments are unlabeled.
  label?: string;
}

// A row of the waterfall: a span node, or a queued-input marker between spans.
interface WaterfallRow { node?: SpanNode; marker?: TraceMarker }

// markersForPart gives each marker to the first stretch that ends after it;
// one past every stretch, or without a time, goes to the last.
function markersForPart(markers: TraceMarker[] | undefined, parts: { range: TimeRange | null }[], index: number): TraceMarker[] {
  if (!markers?.length) return [];
  return markers.filter(m => {
    let home = parts.length - 1;
    if (m.at !== undefined) {
      const hit = parts.findIndex(p => p.range && m.at! <= p.range.t0 + p.range.total);
      if (hit >= 0) home = hit;
    }
    return home === index;
  });
}

// withMarkers interleaves markers with the root spans by start time, so a
// queued input reads where the run picked it up.
function withMarkers(roots: SpanNode[], markers: TraceMarker[]): WaterfallRow[] {
  if (markers.length === 0) return roots.map(node => ({ node }));
  const rows: WaterfallRow[] = [];
  const pending = [...markers].sort((a, b) => (a.at ?? Infinity) - (b.at ?? Infinity));
  for (const node of roots) {
    const start = spanExtent(node.span)?.[0];
    while (pending.length && pending[0].at !== undefined && start !== undefined && pending[0].at <= start) {
      rows.push({ marker: pending.shift()! });
    }
    rows.push({ node });
  }
  for (const marker of pending) rows.push({ marker });
  return rows;
}

// MarkerRow is a queued input on the waterfall, shaped like a span row: the
// text, like a payload, shows once the row is opened.
function MarkerRow({ marker, range, depth = 0 }: { marker: TraceMarker; range: TimeRange | null; depth?: number }) {
  const [open, setOpen] = useState(false);
  const left = range && marker.at !== undefined
    ? Math.min(100, Math.max(0, ((marker.at - range.t0) / range.total) * 100)).toFixed(2) + '%'
    : '100%';
  return (
    <>
      <div
        className="trace-span trace-span-clickable trace-marker"
        style={{ '--d': depth } as CSSProperties}
        role="button"
        tabIndex={0}
        aria-expanded={open}
        onClick={() => setOpen(v => !v)}
        onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); setOpen(v => !v); } }}
      >
        <span className="trace-span-label">
          <span className="trace-ev-icon trace-marker-icon"><CommentIcon size={12} /></span>
          <span className="trace-span-name">Queued input</span>
          <span className="trace-ev-tag trace-ev-tag-span">input</span>
        </span>
        <span className="trace-span-duration" />
        <span className="trace-span-track"><span className="trace-marker-tick" style={{ left }} /></span>
      </div>
      {open && <div className="trace-span-data" style={{ marginLeft: 14 + depth * 12 }}>{marker.text}</div>}
    </>
  );
}

// TraceSummary is what a run's spans add up to: the figures on its list row
// and its detail's meta line.
export interface TraceSummary {
  tokens: { input: number; output: number } | null;
  spanCount: number;
  failed: boolean;
}

export function traceSummary(segments: TraceRunSegment[]): TraceSummary {
  let inp = 0, out = 0, count = 0, failed = false;
  for (const seg of segments) {
    for (const ev of seg.events) {
      if (ev.kind !== 'span') continue;
      count++;
      if (ev.type === 'generation' && ev.data) {
        inp += Number(ev.data.input_tokens) || 0;
        out += Number(ev.data.output_tokens) || 0;
      }
      // The run failed when its loop did: an agent span carries that error;
      // a tool's own failure, recovered, stays on the tool's row.
      if (ev.type === 'agent' && ev.error) failed = true;
    }
  }
  return { tokens: inp > 0 ? { input: inp, output: out } : null, spanCount: count, failed };
}

function tokenText(tokens: { input: number; output: number }): string {
  return (tokens.input + tokens.output).toLocaleString() + ' tok';
}

// TraceTags marks a run: failed, inputs read from its queue, replaced by a
// regeneration — the same set on a list row and a detail's meta line.
function TraceTags({ summary, markers, stale }: { summary: TraceSummary; markers?: TraceMarker[]; stale?: boolean }) {
  return (
    <>
      {summary.failed && <span className="trace-ev-tag trace-ev-tag-error">error</span>}
      {!!markers?.length && <span className="trace-ev-tag" title="Inputs the run read from its queue">{markers.length + ' queued'}</span>}
      {stale && <span className="trace-run-stale" title="This answer was regenerated; the session is on another attempt">replaced</span>}
    </>
  );
}

interface TraceDetailProps {
  segments: TraceRunSegment[];
  // stale marks a run on a branch the session has moved away from.
  stale?: boolean;
  isLive?: boolean;
  // payloadSessionId is the session whose stored rows hold these spans' payload:
  // the chat's own by default, an inspected task's child session in the task lens.
  payloadSessionId?: string;
  // The span to open and mark, when the trace was opened on one.
  focusSpanId?: string;
  // The inputs the run read from its queue, placed on the waterfall by time
  // (lib/queuedInputs.ts); they are the transcript's, not spans.
  markers?: TraceMarker[];
  // lead opens the meta line in place of the token figure — the run lens puts
  // the run id there, having shown the figure in its head.
  lead?: ReactNode;
}

// TraceDetail is one run's trace in full: a meta line (what it cost, its tags)
// over the waterfall of its spans, one timeline per stretch the run ran in.
export function TraceDetail({ segments, stale, isLive, payloadSessionId, focusSpanId, markers, lead }: TraceDetailProps) {
  const { loadSpan } = useChatActions();
  const { sessionId } = useChatSession();
  const payloadSession = payloadSessionId || sessionId;
  const summary = useMemo(() => traceSummary(segments), [segments]);
  const parts = useMemo(() => segments.flatMap(seg => {
    const spanEvents = seg.events.filter(ev => ev.kind === 'span');
    const load = loadSpan && payloadSession ? (spanId: string) => loadSpan(payloadSession, seg.runId, spanId) : undefined;
    let prevEnd: number | undefined;
    let prevPaused = false;
    return splitEpisodes(buildSpanTree(spanEvents)).map((roots, i) => {
      const range = spanTimeRange(episodeSpans(roots));
      // A later stretch is headed by how long the run had been stopped — as
      // a wait for approval when the stretch before it paused for one.
      const gap = range && prevEnd !== undefined ? fmtDuration(range.t0 - prevEnd) : '';
      const label = i === 0 ? seg.label : !gap ? undefined : prevPaused ? 'waited ' + gap + ' for approval' : gap + ' later';
      if (range) prevEnd = range.t0 + range.total;
      prevPaused = roots.some(r => r.span.type === 'agent' && r.span.data?.ended_by === 'interruption');
      return { key: seg.runId + ':' + i, label, spanRoots: roots, range, loadSpan: load };
    });
  }), [segments, loadSpan, payloadSession]);

  if (summary.spanCount === 0) return <div className="trace-empty">{isLive ? 'No trace events yet.' : 'No trace events.'}</div>;
  const tagged = summary.failed || !!markers?.length || stale;
  return (
    <div className="trace-detail">
      {(lead || summary.tokens || tagged) && (
        <div className="trace-meta">
          {lead ?? (summary.tokens && <span className="trace-meta-tokens">{tokenText(summary.tokens)}</span>)}
          <TraceTags summary={summary} markers={markers} stale={stale} />
        </div>
      )}
      {parts.map((p, pi) => {
        // The loop's steps are the agent root's children, so a stretch under
        // one agent span hands its markers down to sit among them.
        const partMarkers = markersForPart(markers, parts, pi);
        const underAgent = p.spanRoots.length === 1 && p.spanRoots[0].span.type === 'agent';
        const rows: WaterfallRow[] = underAgent ? p.spanRoots.map(node => ({ node })) : withMarkers(p.spanRoots, partMarkers);
        return (
          <div key={p.key} className="trace-run-segment" style={p.range ? { '--trace-step': ((tickStep(p.range.total) / p.range.total) * 100).toFixed(2) + '%' } as CSSProperties : undefined}>
            {p.label && <div className="trace-segment-label">{p.label}</div>}
            {rows.map((row, i) => row.marker
              ? <MarkerRow key={'m' + i} marker={row.marker} range={p.range} />
              : <SpanRow key={row.node!.span.span_id || i} node={row.node!} depth={0} range={p.range} loadSpan={p.loadSpan} focusSpanId={focusSpanId} markers={underAgent ? partMarkers : undefined} />)}
          </div>
        );
      })}
    </div>
  );
}

// TraceGroupView is one entry of the Traces lens: a run with the wake-up runs
// its tasks triggered chained under it (lib/traceGroups.ts), each a segment.
export interface TraceGroupView { rootId: string; segments: TraceRunSegment[] }

function runLabel(labels: Record<string, string>, rootId: string): string {
  return labels[rootId] || rootId.slice(0, 8);
}

function groupMarkers(markers: Record<string, TraceMarker[]> | undefined, group: TraceGroupView): TraceMarker[] | undefined {
  return markers ? group.segments.flatMap(s => markers[s.runId] || []) : undefined;
}

// What both lenses read off the session: which run is live, each run's label,
// the branched-away runs, and per run the inputs it read from its queue.
interface TraceLensContext {
  liveRunId: string | null;
  runLabels: Record<string, string>;
  staleRuns?: Set<string>;
  markers?: Record<string, TraceMarker[]>;
}

function TraceRow({ group, label, stale, isLive, markers, onOpen }: { group: TraceGroupView; label: string; stale?: boolean; isLive: boolean; markers?: TraceMarker[]; onOpen: (rootId: string) => void }) {
  const summary = useMemo(() => traceSummary(group.segments), [group.segments]);
  const spanCountText = summary.spanCount + (summary.spanCount === 1 ? ' span' : ' spans');
  return (
    <div className="lens-row trace-row" role="button" tabIndex={0} onClick={() => onOpen(group.rootId)}
      onKeyDown={e => { if (activates(e)) { e.preventDefault(); onOpen(group.rootId); } }}>
      <span className="trace-row-label">{label}</span>
      <TraceTags summary={summary} markers={markers} stale={stale} />
      {isLive && <span className="trace-tab-live" />}
      {summary.tokens && <span className="trace-row-tokens">{tokenText(summary.tokens)}</span>}
      <CounterLabel title={spanCountText} aria-label={spanCountText}>{summary.spanCount}</CounterLabel>
    </div>
  );
}

interface TraceDrawerProps extends TraceLensContext {
  groups: TraceGroupView[];
  onOpen: (rootId: string) => void;
  onClose: () => void;
}

// TraceDrawer is the Inspector's "trace" lens: one row per exchange, oldest
// first, with what it cost and how many spans it left; a row opens the run lens.
export function TraceDrawer({ groups, liveRunId, runLabels, staleRuns, markers, onOpen, onClose }: TraceDrawerProps) {
  return (
    <SidePanel icon={PulseIcon} title="Traces" count={groups.length} onClose={onClose} storageKey="inspectorWidth">
      {groups.length === 0 && <div className="trace-empty">No traces yet.</div>}
      {groups.map(group => (
        <TraceRow
          key={group.rootId}
          group={group}
          label={runLabel(runLabels, group.rootId)}
          stale={staleRuns?.has(group.rootId)}
          isLive={group.segments.some(s => s.runId === liveRunId)}
          markers={groupMarkers(markers, group)}
          onOpen={onOpen}
        />
      ))}
    </SidePanel>
  );
}

interface TraceRunPanelProps extends TraceLensContext {
  group: TraceGroupView;
  // The span to open and mark: the run's last model call when opened from its turn.
  focusSpanId?: string;
  // onJump scrolls the chat to the run's user message; absent when the
  // conversation has no message for the run.
  onJump?: () => void;
  onBack: () => void;
  onClose: () => void;
}

// TraceRunPanel is the Inspector's "run" lens: one exchange's trace under a
// head — kept in view while the trace scrolls — that leads back to the list,
// shows what the run cost and jumps to its message; the run id sits on the
// meta line as plain text.
export function TraceRunPanel({ group, liveRunId, runLabels, staleRuns, markers, focusSpanId, onJump, onBack, onClose }: TraceRunPanelProps) {
  const isLive = group.segments.some(s => s.runId === liveRunId);
  const summary = useMemo(() => traceSummary(group.segments), [group.segments]);
  return (
    <SidePanel icon={PulseIcon} title={runLabel(runLabels, group.rootId)} count={summary.spanCount || undefined} onClose={onClose} storageKey="inspectorWidth">
      <div className="lens-chrome">
        <div className="lens-head">
          <IconButton icon={ArrowLeftIcon} variant="invisible" size="small" aria-label="Back to traces" tooltipDirection="se" onClick={onBack} />
          {isLive && <span className="trace-tab-live" />}
          <div className="lens-head-spacer" />
          {summary.tokens && <span className="trace-head-tokens">{tokenText(summary.tokens)}</span>}
          {onJump && <IconButton icon={CommentIcon} variant="invisible" size="small" aria-label="Jump to message" tooltipDirection="sw" onClick={onJump} />}
        </div>
      </div>
      <TraceDetail
        segments={group.segments}
        stale={staleRuns?.has(group.rootId)}
        isLive={isLive}
        focusSpanId={focusSpanId}
        markers={groupMarkers(markers, group)}
        lead={<span className="trace-meta-id" title={group.rootId}>{group.rootId}</span>}
      />
    </SidePanel>
  );
}

// TraceRunMissingPanel stands in for the run lens while a deep-linked run has
// no trace in the session (still loading, pruned, or another session's): it
// says so and leads back to the list.
export function TraceRunMissingPanel({ runId, loading, onBack, onClose }: { runId: string; loading: boolean; onBack: () => void; onClose: () => void }) {
  return (
    <SidePanel icon={PulseIcon} title={loading ? 'Trace' : 'Run not found'} onClose={onClose} storageKey="inspectorWidth">
      <div className="lens-chrome">
        <div className="lens-head">
          <IconButton icon={ArrowLeftIcon} variant="invisible" size="small" aria-label="Back to traces" tooltipDirection="se" onClick={onBack} />
          <div className="lens-head-spacer" />
          <span className="lens-head-id" title={runId}><span className="lens-head-id-text">{runId}</span></span>
        </div>
      </div>
      {loading
        ? <Loading kind="inline" />
        : <div className="trace-empty">This run has no trace in this session — it may belong to another session, or its trace was removed.</div>}
    </SidePanel>
  );
}
