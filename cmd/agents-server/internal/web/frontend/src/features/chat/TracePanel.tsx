import './trace.css';
import { useState, useEffect, useMemo, useRef, type CSSProperties } from 'react';
import { CounterLabel, Link } from '@primer/react';
import {
  PulseIcon, ToolsIcon, ArrowSwitchIcon, DiamondIcon,
  DependabotIcon, CpuIcon, ShieldCheckIcon, ChevronRightIcon,
  CommentIcon, PlugIcon, TerminalIcon, SyncIcon,
} from '@primer/octicons-react';
import type { Icon } from '@primer/octicons-react';
import { SidePanel } from '@/layout/SidePanel';
import { Disclosure } from '@/components/Disclosure';
import { useChatActions, useChatSession } from '@/features/chat/ChatSessionContext';
import { ReplayDialog } from '@/features/chat/ReplayDialog';
import { PayloadItem, payloadEntry, payloadItems, prettyMaybeJSON, toolOutputEntry, type PayloadRecord } from '@/features/chat/TracePayload';
import { fmtDuration } from '@/lib/background';
import type { AttachmentMeta } from '@/lib/attachments';

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
  // The image attachments the span's input items reference, resolved by the
  // server (the items themselves keep the stored reference).
  attachments?: AttachmentMeta[];
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

/* ---------- span payloads ---------- */

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
// model received (instructions, tool definitions, settings, items) and the
// items it returned.
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

// splitEpisodes cuts a run's roots where it stopped and later went on: a root
// agent span no handoff led to (the loop a resume restarts). Each stretch then
// gets its own timeline instead of sharing one with the pause between them. A
// root that is not an agent (a before-run compaction) goes with the agent
// span after it.
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

// spanExtent is a span's [start, end] in ms; a span still running ends where it started.
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
  const childExpandable = shown.some(c => spanHasDetails(c.span));

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
        className={'trace-span' + (hasData ? ' trace-span-clickable' : '')}
        style={{ '--d': depth } as CSSProperties}
        role={hasData ? 'button' : undefined}
        tabIndex={hasData ? 0 : undefined}
        aria-expanded={hasData ? open : undefined}
        onClick={hasData ? toggle : undefined}
        onKeyDown={hasData ? e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(); } } : undefined}
      >
        <span className="trace-span-label">
          {(hasData || alignChevron) && (
            <span className={'trace-span-chevron' + (open ? ' open' : '')}>
              {hasData && <ChevronRightIcon size={10} />}
            </span>
          )}
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
      {shown.map((c, i) => <SpanRow key={c.span.span_id || i} node={c} depth={depth + 1} range={range} alignChevron={childExpandable} loadSpan={loadSpan} />)}
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

  const { parts, tokens, spanCount, failed } = useMemo(() => {
    let inp = 0, out = 0, count = 0, failed = false;
    const parts = segments.flatMap(seg => {
      const spanEvents = seg.events.filter(ev => ev.kind === 'span');
      count += spanEvents.length;
      for (const ev of spanEvents) {
        if (ev.type === 'generation' && ev.data) {
          inp += Number(ev.data.input_tokens) || 0;
          out += Number(ev.data.output_tokens) || 0;
        }
        // The run failed when its loop did: an agent span carries that error;
        // a tool's own failure, recovered, stays on the tool's row.
        if (ev.type === 'agent' && ev.error) failed = true;
      }
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
    });
    return { parts, tokens: inp > 0 ? { input: inp, output: out } : null, spanCount: count, failed };
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
      {failed && <span className="trace-ev-tag trace-ev-tag-error">error</span>}
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
        <div key={p.key} className="trace-run-segment" style={p.range ? { '--trace-step': ((tickStep(p.range.total) / p.range.total) * 100).toFixed(2) + '%' } as CSSProperties : undefined}>
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
