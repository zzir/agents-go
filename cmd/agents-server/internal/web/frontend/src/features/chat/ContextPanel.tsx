import { useMemo, useState } from 'react';
import { ProgressBar, Spinner } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { MeterIcon } from '@primer/octicons-react';
import { SidePanel } from '@/layout/SidePanel';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
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
  conversation_tokens?: number;
  prompt?: PromptProfile;
}

// Characters → tokens, the estimator's ratio (compaction.CharEstimator). The
// server reports the prompt profile in characters because that is what it can
// measure; the panel shows tokens because that is what the reader is thinking in.
const CHARS_PER_TOKEN = 4;
const est = (chars: number) => Math.round(chars / CHARS_PER_TOKEN);

// The window's composition: the prompt layers and tool surface the build sends
// every turn, plus the conversation itself — one ruler (estimates), so the
// shares can be honest percentages of their own total.
function compositionRows(data: ContextReport): Array<{ label: string; tokens: number; unavailable?: boolean }> {
  const p = data.prompt || {};
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
  if ((data.conversation_tokens || 0) > 0) {
    rows.push({ label: 'Conversation', tokens: data.conversation_tokens! });
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

// A share of a same-ruler total. Whole percents; a sliver reads "<1%" rather
// than rounding to an untruthful 0%.
function pctOf(n: number, total: number): string {
  if (total <= 0 || n <= 0) return '0%';
  const p = (n / total) * 100;
  return p < 1 ? '<1%' : `${Math.round(p)}%`;
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

export function ContextPanel({ sessionId, running, reloadKey, onClose, onCompact }: ContextPanelProps) {
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
  // Share of THIS call's input the cache served — the same denominator the
  // legend's "fresh" is the remainder of, so the two cannot disagree.
  const cached = data?.cached_tokens || 0;
  const hitPct = used > 0 ? (cached / used) * 100 : 0;
  const threshold = (data?.compaction_enabled && data.compaction_threshold) || 0;
  const compactionPct = threshold > 0 ? Math.min(100, ((data?.compaction_tokens || 0) / threshold) * 100) : 0;
  // The fold point on the window's own scale — ONE bar carries both stories.
  // The threshold compares against the compaction figure (last call's total
  // plus estimates), so the tick is where the fold roughly lands, not a second
  // meter; the numbers line below keeps the exact comparison.
  const thresholdPct = windowSize > 0 && threshold > 0 ? Math.min(100, (threshold / windowSize) * 100) : 0;
  // The estimated size of the NEXT request (all composition rows summed). The
  // big number above is the LAST MEASURED call, which does not move until a
  // real request follows — so right after "Compact now" it still shows the
  // pre-fold total. This estimate reflects the fold immediately; surfaced only
  // when it diverges from the measured figure, so steady state stays quiet.
  const composition = data ? compositionRows(data) : [];
  const estNext = composition.reduce((n, r) => n + r.tokens, 0);
  const showNext = windowSize > 0 && estNext > 0 && Math.abs(estNext - used) >= Math.max(500, used * 0.1);

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
              {data.compaction_enabled && onCompact && (
                <button
                  type="button"
                  className="ctx-compact-btn ctx-compact-btn--end"
                  disabled={running || compacting}
                  title={running
                    ? 'The run compacts at its own boundaries — wait for it to finish'
                    : 'Fold older history into a summary now, keeping the recent window'}
                  onClick={compact}
                >
                  {compacting ? 'Compacting…' : 'Compact now'}
                </button>
              )}
            </div>
            {windowSize > 0 ? (
              <>
                <div className="ctx-track">
                  <ProgressBar progress={pct} bg="accent.emphasis" aria-label={`${Math.round(pct)}% of the context window used`} />
                  {thresholdPct > 0 && (
                    <span
                      className="ctx-tick"
                      style={{ left: `${thresholdPct}%` }}
                      title={`Auto-compaction fires around ${fmt(threshold)} tokens (${Math.round(thresholdPct)}% of the window)`}
                    />
                  )}
                </div>
                <div className="ctx-legend">
                  <span>used {Math.round(pct)}%</span>
                  {thresholdPct > 0 && <span className="ctx-muted">compacts at ~{Math.round(thresholdPct)}%</span>}
                  <span className="ctx-muted">{fmt(Math.max(0, windowSize - used))} free</span>
                </div>
                {showNext && (
                  <div className="ctx-legend">
                    <span className="ctx-muted" title="Estimated tokens the NEXT request will send. The figure above is the last measured call and only updates when one follows — so after Compact now this is what the folded conversation now costs.">
                      next call {approx(estNext)}
                    </span>
                  </div>
                )}
              </>
            ) : threshold > 0 ? (
              <>
                {/* No window declared: the compaction meter is the only scale
                    there is, so the single bar draws it directly. */}
                <ProgressBar progress={compactionPct} bg="attention.emphasis" aria-label={`${Math.round(compactionPct)}% of the way to a compaction pass`} />
                <div className="ctx-legend">
                  <span>to next fold {Math.round(compactionPct)}%</span>
                  <span className="ctx-muted">no context window set for this agent</span>
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
          </section>

          {(() => {
            const rows = composition;
            if (rows.length === 0) return null;
            const total = rows.reduce((n, r) => n + r.tokens, 0);
            return (
              <section className="ctx-sec">
                <div className="ctx-sec-head">
                  <span>In the window</span>
                </div>
                <ul className="ctx-rows ctx-rows-plain">
                  {/* Keyed by position too: two MCP servers may share a display
                      name, and colliding keys would drop a row. */}
                  {rows.map((r, i) => (
                    <li key={`${i}-${r.label}`}>
                      <div className="ctx-row-top">
                        <span className="ctx-row-name">{r.label}</span>
                        {r.unavailable ? (
                          <span className="ctx-mono ctx-row-tok">unknown</span>
                        ) : (
                          // The share is the row's story; the absolute estimate
                          // moves to hover, where a reader who wants it looks.
                          <span className="ctx-mono ctx-row-tok" title={`${approx(r.tokens)} tokens (estimated)`}>
                            {pctOf(r.tokens, total)}
                          </span>
                        )}
                      </div>
                      <span className="ctx-row-track">
                        <span className="ctx-row-fill" style={{ width: r.unavailable ? '0%' : `${total > 0 ? (r.tokens / total) * 100 : 0}%` }} />
                      </span>
                    </li>
                  ))}
                </ul>
                <div className="ctx-note">
                  The window's composition, by the character estimator: the prompt layers and tool schemas the build
                  sends every turn, and the conversation so far. Shares of their own total — not provider counts.
                </div>
              </section>
            );
          })()}

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
        </div>
      )}
    </SidePanel>
  );
}
