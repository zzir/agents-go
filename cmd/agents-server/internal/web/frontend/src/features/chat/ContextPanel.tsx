import { useMemo, useState } from 'react';
import { ProgressBar, Spinner } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { MeterIcon, PulseIcon } from '@primer/octicons-react';
import { SidePanel } from '@/layout/SidePanel';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import './context.css';

// One entry's estimated share of the context, as the server ranks it.
interface ContextItem {
  kind: string;
  label: string;
  tokens: number;
  anchor?: string;
  run_id?: string;
  // The model call that produced it — how a non-tool item finds its generation
  // span. Tool items join on anchor, which is their call id.
  response_id?: string;
}

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
  skills_index_chars?: number;
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
  items?: ContextItem[];
  prompt?: PromptProfile;
}

// Characters → tokens, the estimator's ratio (compaction.CharEstimator). The
// server reports the prompt profile in characters because that is what it can
// measure; the panel shows tokens because that is what the reader is thinking in.
const CHARS_PER_TOKEN = 4;
const est = (chars: number) => Math.round(chars / CHARS_PER_TOKEN);

// A prompt profile flattened into the rows the Breakdown draws: the
// instruction layers, then one row per tool origin.
function breakdownRows(p: PromptProfile): Array<{ label: string; tokens: number; unavailable?: boolean }> {
  const rows: Array<{ label: string; tokens: number; unavailable?: boolean }> = [
    { label: 'Instructions', tokens: est(p.instructions_chars || 0) },
    { label: 'System prompt', tokens: est(p.global_prompt_chars || 0) },
    { label: 'Memory', tokens: est(p.memory_chars || 0) },
    { label: 'Skills index', tokens: est(p.skills_index_chars || 0) },
  ].filter(r => r.tokens > 0);
  for (const b of p.tools || []) {
    rows.push({
      label: b.source.startsWith('mcp:') ? b.source : `tools · ${b.source}`,
      tokens: est(b.chars),
      unavailable: b.unavailable,
    });
  }
  return rows;
}

interface ContextPanelProps {
  sessionId: string;
  // running refetches when a run ends: the report is computed from stored
  // entries, so it only moves at turn boundaries.
  running: boolean;
  // reloadKey refetches when its identity changes — the caller passes what a
  // server re-read of the timeline replaces, so a branch switch (the report
  // describes the ACTIVE branch, and <2/2> moves it without any run ending)
  // updates the report too.
  reloadKey?: unknown;
  onClose: () => void;
  // onJump takes the reader to the item in the CHAT — what to do about a fat
  // item is decided on its content. onOpenTrace is the secondary move: how it
  // got there (arguments, timing, nested sandbox work). Absent for an item
  // whose run left no trace.
  onJump: (item: ContextItem) => void;
  onOpenTrace?: (item: ContextItem) => void;
  hasTrace?: (item: ContextItem) => boolean;
  // onCompact forces one compaction pass now; it owns the API call, the
  // toasts and the timeline reload — the panel only shows the pending state.
  onCompact?: () => Promise<void>;
}

const fmt = (n: number) => n.toLocaleString();

// Estimated values are rendered to the precision they actually have: two
// significant figures behind a ~. A badge saying "estimated" is skipped by the
// eye that reads the digits; four exact-looking digits claim a measurement the
// character estimator never made. Provider figures keep every digit, and the
// contrast between the two is the whole labelling scheme.
function approx(n: number): string {
  if (n <= 0) return '0';
  const step = Math.pow(10, Math.max(0, Math.floor(Math.log10(n)) - 1));
  return '~' + (Math.round(n / step) * step).toLocaleString();
}

// Sparkline over the per-call input tokens. Absolute values are already on the
// gauge above; this shape answers "how fast is it filling".
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
      <circle className="ctx-spark-dot" cx={last[0]} cy={last[1]} r="2.6" />
    </svg>
  );
}

export function ContextPanel({ sessionId, running, reloadKey, onClose, onJump, onOpenTrace, hasTrace, onCompact }: ContextPanelProps) {
  const { data, loading } = useApi<ContextReport>(() => api.sessions.context(sessionId), [sessionId, running, reloadKey]);
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
  // Share of THIS call's input the cache served — the same denominator the
  // legend's "fresh" is the remainder of, so the two cannot disagree.
  const cached = data?.cached_tokens || 0;
  const hitPct = used > 0 ? (cached / used) * 100 : 0;
  const compactionPct = data?.compaction_threshold
    ? Math.min(100, (data.compaction_tokens / data.compaction_threshold) * 100)
    : 0;
  const heaviest = data?.items?.[0]?.tokens || 1;

  return (
    <SidePanel
      icon={MeterIcon}
      title="Context"
      count={windowSize > 0 ? `${Math.round(pct)}%` : undefined}
      onClose={onClose}
      storageKey="inspectorWidth"
    >
      {loading && !data ? (
        <div className="ctx-loading"><Spinner size="small" /></div>
      ) : !data || (used === 0 && !data.compaction_tokens) ? (
        <Blankslate>
          <Blankslate.Description>Nothing in context yet — the first model call reports what it sent.</Blankslate.Description>
        </Blankslate>
      ) : (
        <div className="ctx-body">
          <section className="ctx-sec">
            <div className="ctx-sec-head">
              <span>Window</span>
              {data.model && <span className="ctx-mono ctx-muted">{data.model}</span>}
            </div>
            <div className="ctx-big">
              <span className="ctx-mono ctx-num">{fmt(used)}</span>
              {windowSize > 0 && <span className="ctx-mono ctx-slash">/ {fmt(windowSize)}</span>}
            </div>
            {windowSize > 0 ? (
              <>
                <ProgressBar progress={pct} bg="accent.emphasis" aria-label={`${Math.round(pct)}% of the context window used`} />
                <div className="ctx-legend">
                  <span>used {Math.round(pct)}%</span>
                  <span className="ctx-muted">{fmt(Math.max(0, windowSize - used))} free</span>
                </div>
              </>
            ) : (
              <div className="ctx-legend">
                <span className="ctx-muted">No context window set for this agent — add one to see how full it is.</span>
              </div>
            )}
            <div className="ctx-note">
              Session totals <span className="ctx-mono">{fmt(data.session_input_tokens)} in</span> ·{' '}
              <span className="ctx-mono">{fmt(data.session_output_tokens)} out</span>
            </div>

            {data.compaction_enabled && (
              <div className="ctx-sub">
                <div className="ctx-sub-head">
                  <span className="ctx-sub-label">
                    Compaction
                    {onCompact && (
                      <button
                        type="button"
                        className="ctx-compact-btn"
                        disabled={running || compacting}
                        title={running
                          ? 'The run compacts at its own boundaries — wait for it to finish'
                          : 'Fold older history into a summary now, keeping the recent window'}
                        onClick={compact}
                      >
                        {compacting ? 'Compacting…' : 'Compact now'}
                      </button>
                    )}
                  </span>
                  <span className="ctx-mono ctx-sub-val">
                    {fmt(data.compaction_tokens)}
                    {data.compaction_threshold ? <span className="ctx-muted"> / {fmt(data.compaction_threshold)}</span> : null}
                  </span>
                </div>
                {data.compaction_threshold ? (
                  <ProgressBar progress={compactionPct} bg="attention.emphasis" barSize="small"
                    aria-label={`${Math.round(compactionPct)}% of the way to a compaction pass`} />
                ) : null}
                <div className="ctx-note ctx-sub-note">
                  What the pass actually compares: the last call's total — input <em>and</em> output — plus an estimate
                  for the turns since. Right after a fold it is fully estimated, until the next call re-prices.
                </div>
              </div>
            )}
          </section>

          {(cached > 0 || data.cache_write_tokens > 0) && (
            <section className="ctx-sec">
              <div className="ctx-sec-head">
                <span>Cache</span>
                <span className="ctx-mono ctx-ok">{Math.round(hitPct)}% hit</span>
              </div>
              <ProgressBar progress={hitPct} bg="success.emphasis" barSize="small"
                aria-label={`${Math.round(hitPct)}% of the last call's input was served from cache`} />
              <div className="ctx-legend">
                <span>hit <span className="ctx-mono">{fmt(cached)}</span></span>
                <span>fresh <span className="ctx-mono">{fmt(Math.max(0, used - cached))}</span></span>
                {data.cache_write_tokens > 0 && (
                  <span>written <span className="ctx-mono">{fmt(data.cache_write_tokens)}</span></span>
                )}
              </div>
            </section>
          )}

          {(data.growth?.length || 0) > 1 && (
            <section className="ctx-sec">
              <div className="ctx-sec-head">
                <span>Growth</span>
                <span className="ctx-mono ctx-muted">
                  {(() => {
                    const g = data.growth!;
                    const delta = g[g.length - 1] - g[g.length - 2];
                    return `${delta >= 0 ? '+' : ''}${fmt(delta)} last call`;
                  })()}
                </span>
              </div>
              <Growth points={data.growth!} />
              <div className="ctx-legend">
                <span className="ctx-muted">call 1</span>
                <span className="ctx-muted ctx-legend-end">call {data.growth!.length}</span>
              </div>
            </section>
          )}

          {data.prompt && breakdownRows(data.prompt).length > 0 && (() => {
            const rows = breakdownRows(data.prompt!);
            const total = rows.reduce((n, r) => n + r.tokens, 0);
            const widest = Math.max(...rows.map(r => r.tokens), 1);
            return (
              <section className="ctx-sec">
                <div className="ctx-sec-head">
                  <span>Before the conversation</span>
                </div>
                <div className="ctx-big ctx-big-small">
                  <span className="ctx-mono ctx-num ctx-num-small">{approx(total)}</span>
                  <span className="ctx-mono ctx-slash">every turn</span>
                </div>
                <ul className="ctx-rows ctx-rows-plain">
                  {/* Keyed by position too: two MCP servers may share a display
                      name, and colliding keys would drop a row. */}
                  {rows.map((r, i) => (
                    <li key={`${i}-${r.label}`}>
                      <div className="ctx-row-top">
                        <span className="ctx-row-name">{r.label}</span>
                        <span className="ctx-mono ctx-row-tok">{r.unavailable ? 'unknown' : approx(r.tokens)}</span>
                      </div>
                      <span className="ctx-row-track">
                        <span className="ctx-row-fill" style={{ width: r.unavailable ? '0%' : `${(r.tokens / widest) * 100}%` }} />
                      </span>
                    </li>
                  ))}
                </ul>
                <div className="ctx-note">
                  Prompt layers and tool schemas from the last run's build — the part of the window the conversation
                  never shrinks.
                </div>
              </section>
            );
          })()}

          {(data.items?.length || 0) > 0 && (
            <section className="ctx-sec">
              <div className="ctx-sec-head">
                <span>Heaviest items</span>
              </div>
              <ul className="ctx-rows">
                {data.items!.map((it, i) => {
                  // The trace button swaps in WHERE the token figure sits
                  // (hover hides the figure, shows the button — the VS Code
                  // tree-item pattern). No reserved column, so the figures
                  // stay flush with the panel edge like every other section;
                  // no overlay, so nothing is ever covered half-way.
                  const traceable = !!onOpenTrace && !!hasTrace?.(it);
                  return (
                    <li key={it.anchor || i} className={'ctx-row-wrap' + (traceable ? ' ctx-row-has-trace' : '')}>
                      <button className="ctx-row" onClick={() => onJump(it)} title="Jump to this item">
                        <span className="ctx-row-top">
                          <span className="ctx-kind">{it.kind}</span>
                          <span className="ctx-row-name">{it.label || it.kind}</span>
                          <span className="ctx-mono ctx-row-tok">{approx(it.tokens)}</span>
                        </span>
                        <span className="ctx-row-track">
                          <span className="ctx-row-fill" style={{ width: `${(it.tokens / heaviest) * 100}%` }} />
                        </span>
                      </button>
                      {traceable && (
                        // A bare glyph, not an IconButton: the 28px control
                        // chrome spanned both row lines and sat on the bar.
                        // This lives inside the text line, where the figure it
                        // replaces sits, and never touches the bar below.
                        <button
                          type="button"
                          className="ctx-row-trace"
                          aria-label="Show in trace"
                          title="Show in trace"
                          onClick={() => onOpenTrace!(it)}
                        >
                          <PulseIcon size={14} />
                        </button>
                      )}
                    </li>
                  );
                })}
              </ul>
            </section>
          )}
        </div>
      )}
    </SidePanel>
  );
}

export type { ContextItem };
