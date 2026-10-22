import { Fragment, useEffect, useMemo, useState } from 'react';
import { Link, useConfirm } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { ChevronRightIcon, MeterIcon, XIcon } from '@primer/octicons-react';
import { SidePanel } from '@/layout/SidePanel';
import { Loading } from '@/components/Loading';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { activates } from '@/lib/activation';
import './context.css';

// One origin's share of the tool surface. unavailable marks a server that
// could not be asked — reported as unknown, never as zero.
interface ToolBucket {
  source: string;
  count: number;
  chars: number;
  unavailable?: boolean;
}

// What the last build put in front of the conversation, in characters.
interface PromptProfile {
  instructions_chars?: number;
  global_prompt_chars?: number;
  memory_chars?: number;
  sandbox_prompt_chars?: number;
  context_guidance_chars?: number;
  skills_index_chars?: number;
  plan_preamble_chars?: number;
  background_chars?: number;
  tools?: ToolBucket[];
}

interface ContextReport {
  model?: string;
  context_window?: number;
  input_tokens: number;
  output_tokens: number;
  cached_tokens: number;
  cache_write_tokens: number;
  session_input_tokens: number;
  session_output_tokens: number;
  growth?: number[];
  compaction_enabled: boolean;
  compaction_threshold?: number;
  compaction_tokens: number;
  compaction_mode?: string;
  conversation_tokens?: number;
  prompt?: PromptProfile;
}

// Characters → tokens at the estimator's ratio (compaction.CharEstimator): the
// server reports the prompt profile in characters, the panel shows tokens.
const CHARS_PER_TOKEN = 4;
const est = (chars: number) => Math.round(chars / CHARS_PER_TOKEN);

// One layer of the window, on the estimate ruler.
interface Layer { label: string; tokens: number; unavailable?: boolean }
// The window's composition in four groups — the prompt layers, the skills
// index, the tool surface, the messages — on the one estimate ruler so the
// shares add up (invariant 28). A group of one layer is shown as that layer.
interface Group { label: string; tokens: number; unavailable?: boolean; layers: Layer[] }

function compositionGroups(data: ContextReport): Group[] {
  const p = data.prompt || {};
  const layer = (label: string, chars?: number): Layer[] => est(chars || 0) > 0 ? [{ label, tokens: est(chars || 0) }] : [];
  const prompt = [
    ...layer('Instructions', p.instructions_chars),
    ...layer('System prompt', p.global_prompt_chars),
    ...layer('Memory', p.memory_chars),
    ...layer('Sandbox prompt', p.sandbox_prompt_chars),
    ...layer('Context guidance', p.context_guidance_chars),
    ...layer('Plan mode preamble', p.plan_preamble_chars),
    ...layer('Background suffix', p.background_chars),
  ];
  const tools = (p.tools || []).map(b => ({
    label: b.source.startsWith('mcp:') ? b.source : `tools · ${b.source}`,
    tokens: est(b.chars),
    unavailable: b.unavailable,
  }));
  const messages: Layer[] = (data.conversation_tokens || 0) > 0 ? [{ label: 'Messages', tokens: data.conversation_tokens! }] : [];
  const all: { label: string; layers: Layer[] }[] = [
    { label: 'Prompt', layers: prompt },
    { label: 'Skills', layers: layer('Skills index', p.skills_index_chars) },
    { label: 'Tools', layers: tools },
    { label: 'Messages', layers: messages },
  ];
  return all.filter(g => g.layers.length > 0).map(g => ({
    ...g,
    tokens: g.layers.reduce((n, l) => n + (l.unavailable ? 0 : l.tokens), 0),
    unavailable: g.layers.some(l => l.unavailable),
  }));
}

interface ContextPanelProps {
  sessionId: string;
  // running refetches when a run ends: the report is computed from stored
  // entries, so it only moves at turn boundaries.
  running: boolean;
  // reloadKey refetches when its identity changes: the caller passes what a
  // timeline re-read replaces, so a branch switch (no run ends) updates the report too.
  reloadKey?: unknown;
  onClose: () => void;
  // onCompact forces one compaction pass now; it owns the API call, the
  // toasts and the timeline reload — the panel only shows the pending state.
  onCompact?: () => Promise<void>;
  // Opens the Settings dialog on a tab — the hint to set a context window
  // links to Agents through it.
  onSettingsOpen?: (tab?: string) => void;
}

const fmt = (n: number) => n.toLocaleString();

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

// approx renders an estimate at the precision it has: two significant figures
// behind a ~, while provider figures keep every digit — invariant 28.
function approx(n: number): string {
  if (n <= 0) return '0';
  const step = Math.pow(10, Math.max(0, Math.floor(Math.log10(n)) - 1));
  return '~' + (Math.round(n / step) * step).toLocaleString();
}

// A share of a same-ruler total. Whole percents; a sliver reads "<1%" rather
// than rounding to an untruthful 0%.
function pctOf(n: number, total: number): string {
  if (total <= 0 || n <= 0) return '0%';
  const p = (n / total) * 100;
  return p < 1 ? '<1%' : `${Math.round(p)}%`;
}

// Sparkline over the per-call input tokens. Absolute values are on the meter
// above; this shape answers "how fast is it filling".
function Growth({ points }: { points: number[] }) {
  const path = useMemo(() => {
    const max = Math.max(...points, 1);
    const stepX = points.length > 1 ? 392 / (points.length - 1) : 0;
    return points.map((v, i) => `${(4 + i * stepX).toFixed(1)},${(40 - (v / max) * 32).toFixed(1)}`);
  }, [points]);
  const last = path[path.length - 1].split(',');
  return (
    <svg className="ctx-spark" viewBox="0 0 400 46" preserveAspectRatio="none" role="img"
      aria-label={`Input tokens per model call, ${fmt(points[0])} rising to ${fmt(points[points.length - 1])}`}>
      <polygon className="ctx-spark-area" points={`${path.join(' ')} 396,46 4,46`} />
      <polyline className="ctx-spark-line" points={path.join(' ')} />
      <circle className="ctx-spark-dot" cx={last[0]} cy={last[1]} r="4" />
    </svg>
  );
}

// Composition lists the groups as rows; a group of several layers opens them
// under it. Closed layers stay in the DOM (hidden), so the list reads whole.
function Composition({ groups }: { groups: Group[] }) {
  const [open, setOpen] = useState<Record<string, boolean>>({});
  const total = groups.reduce((n, g) => n + g.tokens, 0);
  const toggle = (label: string) => setOpen(o => ({ ...o, [label]: !o[label] }));
  const cells = (row: Layer, extra?: string) => (
    <>
      <span className="ctx-row-bar"><span className="ctx-row-fill" style={{ width: row.unavailable ? '0%' : `${total > 0 ? (row.tokens / total) * 100 : 0}%` }} /></span>
      <span className="ctx-mono ctx-row-pct" title={row.unavailable ? 'A server that could not be sized' : `${approx(row.tokens)} tokens, estimated${extra || ''}`}>
        {row.unavailable ? 'unknown' : pctOf(row.tokens, total)}
      </span>
    </>
  );
  return (
    <ul className="ctx-rows">
      {groups.map(g => {
        const single = g.layers.length === 1;
        const isOpen = !!open[g.label];
        if (single) {
          return (
            <li key={g.label} className="ctx-row">
              <span className="ctx-row-name">{g.layers[0].label}</span>
              {cells(g.layers[0])}
            </li>
          );
        }
        return (
          <Fragment key={g.label}>
            <li className={'ctx-row ctx-row-group' + (isOpen ? ' ctx-row-open' : '')} role="button" tabIndex={0} aria-expanded={isOpen}
              onClick={() => toggle(g.label)} onKeyDown={e => { if (activates(e)) { e.preventDefault(); toggle(g.label); } }}>
              <span className="ctx-row-name"><span className="ctx-chev"><ChevronRightIcon size={12} /></span>{g.label}</span>
              {cells({ label: g.label, tokens: g.tokens }, g.unavailable ? '; one source could not be sized' : '')}
            </li>
            {g.layers.map((l, i) => (
              // Keyed by position too: two MCP servers may share a display name.
              <li key={`${i}-${l.label}`} className="ctx-row ctx-row-sub" hidden={!isOpen}>
                <span className="ctx-row-name">{l.label}</span>
                {cells(l)}
              </li>
            ))}
          </Fragment>
        );
      })}
    </ul>
  );
}

interface SessionMemoryInfo { id: string; key: string; bytes: number; written_by: string; updated_at: string }

// SessionMemory lists what the model wrote for itself in this conversation:
// the memory that survives compaction and a reset. Read-only here; a key
// opens its text, and the owner can delete one.
function SessionMemory({ sessionId, running, reloadKey }: { sessionId: string; running: boolean; reloadKey?: unknown }) {
  const { data, reload } = useApi<SessionMemoryInfo[]>(() => api.sessions.memory(sessionId) as Promise<SessionMemoryInfo[]>, [sessionId, running, reloadKey]);
  const [open, setOpen] = useState<string | null>(null);
  // Content is cached per key AND version: a memory the model rewrote moves
  // its updated_at, so the next expand (or the open row) fetches it again.
  const [content, setContent] = useState<Record<string, string>>({});
  const rows = data || [];
  const versionOf = (r: SessionMemoryInfo) => `${r.key}@${r.updated_at}`;
  const openRow = rows.find(r => r.key === open);
  const openVersion = openRow ? versionOf(openRow) : null;
  useEffect(() => {
    if (!openRow || !openVersion || content[openVersion] !== undefined) return;
    let cancelled = false;
    api.sessions.memoryKey(sessionId, openRow.key)
      .then(m => { if (!cancelled) setContent(prev => ({ ...prev, [openVersion]: m.content ?? '' })); })
      .catch(e => { if (!cancelled) setContent(prev => ({ ...prev, [openVersion]: `(could not load: ${e instanceof Error ? e.message : String(e)})` })); });
    return () => { cancelled = true; };
  }, [sessionId, openRow, openVersion, content]);
  const show = (key: string) => setOpen(open === key ? null : key);
  const confirm = useConfirm();
  // A delete confirms like every other one (invariant 41).
  const del = async (row: SessionMemoryInfo) => {
    if (!await confirm({
      title: `Delete “${row.key}”?`,
      content: 'The memory is removed from this session. This cannot be undone.',
      confirmButtonContent: 'Delete',
      confirmButtonType: 'danger',
    })) return;
    await api.memories.delete(row.id);
    setContent(prev => Object.fromEntries(Object.entries(prev).filter(([k]) => !k.startsWith(`${row.key}@`))));
    if (open === row.key) setOpen(null);
    reload();
  };
  if (rows.length === 0) return null;
  const total = rows.reduce((n, r) => n + r.bytes, 0);
  return (
    <section className="ctx-sec">
      <div className="ctx-cap">
        <span>Memory</span>
        <span className="ctx-mono">{rows.length} {rows.length === 1 ? 'key' : 'keys'} · {fmtBytes(total)}</span>
      </div>
      {rows.map(r => (
        <div key={r.id}>
          <div className="ctx-mem">
            <button type="button" className="ctx-mono ctx-mem-key" onClick={() => show(r.key)} title={r.written_by === 'model' ? 'Written by the model' : 'Written by you'}>{r.key}</button>
            <span className="ctx-mono ctx-muted">{fmtBytes(r.bytes)}</span>
            <button type="button" className="ctx-mem-del" onClick={() => del(r)} title="Delete this memory" aria-label={`Delete memory ${r.key}`}><XIcon size={12} /></button>
          </div>
          {open === r.key && (
            <pre className="ctx-mem-text">{content[versionOf(r)] === undefined ? 'Loading…' : content[versionOf(r)]}</pre>
          )}
        </div>
      ))}
    </section>
  );
}

export function ContextPanel({ sessionId, running, reloadKey, onClose, onCompact, onSettingsOpen }: ContextPanelProps) {
  const { data, loading } = useApi<ContextReport>(() => api.sessions.context(sessionId) as Promise<ContextReport>, [sessionId, running, reloadKey]);
  const [compacting, setCompacting] = useState(false);
  const compact = async () => {
    if (!onCompact || compacting) return;
    setCompacting(true);
    try {
      await onCompact();
    } finally {
      setCompacting(false);
    }
  };

  const windowSize = data?.context_window || 0;
  const used = data?.input_tokens || 0;
  const pct = windowSize > 0 ? Math.min(100, (used / windowSize) * 100) : 0;
  // The cached share of THIS call's input; fresh is the remainder of the same
  // denominator, so the two cannot disagree.
  const cached = Math.min(data?.cached_tokens || 0, used);
  const fresh = used - cached;
  const threshold = (data?.compaction_enabled && data.compaction_threshold) || 0;
  const resetMode = data?.compaction_mode === 'reset' || data?.compaction_mode === 'hybrid';
  const compactionPct = threshold > 0 ? Math.min(100, ((data?.compaction_tokens || 0) / threshold) * 100) : 0;
  // The fold point as a tick on the window's own bar (invariant 28): the
  // threshold compares against the compaction figure, so the tick is approximate.
  const thresholdPct = windowSize > 0 && threshold > 0 ? Math.min(100, (threshold / windowSize) * 100) : 0;
  // The estimated size of the NEXT request; the big number is the LAST MEASURED
  // call, which stays pre-fold until a request follows. Shown only when the two
  // diverge.
  const groups = data ? compositionGroups(data) : [];
  const estNext = groups.reduce((n, g) => n + g.tokens, 0);
  const showNext = windowSize > 0 && estNext > 0 && Math.abs(estNext - used) >= Math.max(500, used * 0.1);
  const growth = data?.growth && data.growth.length > 1 ? data.growth : null;
  const delta = growth ? growth[growth.length - 1] - growth[growth.length - 2] : 0;

  return (
    <SidePanel
      icon={MeterIcon}
      title="Context"
      count={windowSize > 0 ? `${Math.round(pct)}%` : undefined}
      onClose={onClose}
      storageKey="inspectorWidth"
    >
      {loading && !data ? (
        <Loading kind="panel" />
      ) : !data || (used === 0 && !data.compaction_tokens) ? (
        <Blankslate>
          <Blankslate.Description>Nothing in context yet.</Blankslate.Description>
        </Blankslate>
      ) : (
        <div className="ctx-body">
          <section className="ctx-sec">
            <div className="ctx-big">
              <span className="ctx-mono ctx-num" title="The last measured call; the model is told this figure on every call">{fmt(used)}</span>
              {windowSize > 0 && <span className="ctx-mono ctx-of">/ {fmt(windowSize)}</span>}
              <span className="ctx-big-end">
                {data.model && <span className="ctx-mono ctx-muted">{data.model}</span>}
                {data.compaction_enabled && onCompact && (
                  <button
                    type="button"
                    className="ctx-compact-btn"
                    disabled={running || compacting}
                    title={running
                      ? 'The run compacts at its own boundaries — wait for it to finish'
                      : resetMode
                        ? 'Start the model over with its session memory and the latest message; the history stays searchable'
                        : 'Fold older history into a summary now, keeping the recent window'}
                    onClick={compact}
                  >
                    {compacting ? (resetMode ? 'Resetting…' : 'Compacting…') : resetMode ? 'Reset now' : 'Compact now'}
                  </button>
                )}
              </span>
            </div>
            {windowSize > 0 ? (
              <>
                <div className="ctx-meter" role="img" aria-label={`${Math.round(pct)}% of the context window used`}>
                  <span className="ctx-meter-fill" style={{ width: `${pct}%` }} />
                  {cached > 0 && fresh > 0 && (
                    <span className="ctx-meter-fresh" style={{ left: `${(cached / windowSize) * 100}%`, width: `${(fresh / windowSize) * 100}%` }} />
                  )}
                  {thresholdPct > 0 && (
                    <span
                      className="ctx-tick"
                      style={{ left: `${thresholdPct}%` }}
                      title={`Auto-compaction ${resetMode ? 'resets' : 'compacts'} around ${fmt(threshold)} tokens (${Math.round(thresholdPct)}% of the window)`}
                    />
                  )}
                </div>
                <div className="ctx-line ctx-mono">
                  {cached > 0 ? (
                    <>
                      <span title={`${Math.round((cached / used) * 100)}% of the last call's input was served from cache${data.cache_write_tokens > 0 ? `; ${fmt(data.cache_write_tokens)} written` : ''}`}>
                        <i className="ctx-sw" />{fmt(cached)} cached
                      </span>
                      <span><i className="ctx-sw ctx-sw-fresh" />{fmt(fresh)} fresh</span>
                    </>
                  ) : (
                    <span>{fmt(used)} used</span>
                  )}
                  {showNext && (
                    <span title="Estimated tokens the NEXT request will send; the figure above is the last measured call and only updates when one follows">
                      next {approx(estNext)}
                    </span>
                  )}
                  <span className="ctx-line-end">{fmt(Math.max(0, windowSize - used))} free</span>
                </div>
              </>
            ) : threshold > 0 ? (
              <>
                {/* No window declared: the compaction meter is the only scale
                    there is, so the single bar draws it directly. */}
                <div className="ctx-meter ctx-meter-fold" role="img" aria-label={`${Math.round(compactionPct)}% of the way to a compaction pass`}>
                  <span className="ctx-meter-fill" style={{ width: `${compactionPct}%` }} />
                </div>
                <div className="ctx-line ctx-mono">
                  <span>to next fold {Math.round(compactionPct)}%</span>
                  <span className="ctx-line-end">no context window</span>
                </div>
              </>
            ) : (
              <div className="ctx-line">
                <span>
                  No context window ·{' '}
                  {onSettingsOpen
                    ? <Link as="button" type="button" onClick={() => onSettingsOpen('agents')}>Settings → Agents</Link>
                    : 'Settings → Agents'}
                </span>
              </div>
            )}
            {growth && <Growth points={growth} />}
            <div className="ctx-line ctx-mono">
              {growth && <span>{growth.length} calls · {delta >= 0 ? '+' : ''}{fmt(delta)}</span>}
              <span className="ctx-line-end">session {fmt(data.session_input_tokens)} in · {fmt(data.session_output_tokens)} out</span>
            </div>
          </section>

          {groups.length > 0 && (
            <section className="ctx-sec">
              <div className="ctx-cap">
                <span title="Estimated from characters: the prompt layers and tool schemas the build sends every turn, and the messages so far; shares of their own total, not provider counts">In the window</span>
              </div>
              <Composition groups={groups} />
            </section>
          )}

          <SessionMemory sessionId={sessionId} running={running} reloadKey={reloadKey} />
        </div>
      )}
    </SidePanel>
  );
}
