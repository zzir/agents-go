import { useState, useEffect, useMemo, memo } from 'react';
import { Button, IconButton } from '@primer/react';
import { ArrowLeftIcon, StackIcon, CopyIcon, CheckIcon, WorkflowIcon } from '@primer/octicons-react';
import { SidePanel } from '@/layout/SidePanel';
import { ToolCallCard } from '@/features/chat/ToolCallCard';
import { StreamingMarkdown } from '@/features/chat/StreamingMarkdown';
import { TraceRun, type TraceEventData } from '@/features/chat/TracePanel';
import { useAsyncMarkdown } from '@/lib/markdown';
import type { BackgroundItem } from '@/lib/background';
import type { TaskViewState } from '@/lib/useAgentSocket';
import type { TurnPart } from '@/lib/timeline';

// This is the panel behind the top bar's Tasks button. It holds both kinds of
// background work — spawned tasks and workflow executions — because to a
// person they are one thing: work happening in a session they are not in. The
// UI keeps saying "Tasks"; a second word would be a second concept.

// statusDot carries the state as color (with the words in the
// tooltip/aria-label): in the list the group headers name the state, in the
// detail header the action buttons spell it out. Live states pulse, same
// rhythm as the trace live dot.
function statusDot(status: string) {
  const text = status.replace('_', ' ');
  return <span className={'task-status-dot task-status-dot-' + status} title={text} aria-label={text} role="img" />;
}

// fmtDuration renders a millisecond span as a compact duration (12s, 4m32s,
// 1h03m) for the list's right-hand label.
function fmtDuration(ms: number): string {
  if (!isFinite(ms) || ms < 0) return '';
  const s = Math.floor(ms / 1000);
  if (s < 60) return s + 's';
  const m = Math.floor(s / 60);
  if (m < 60) return m + 'm' + String(s % 60).padStart(2, '0') + 's';
  return Math.floor(m / 60) + 'h' + String(m % 60).padStart(2, '0') + 'm';
}

function isLive(status: string): boolean {
  return status === 'working' || status === 'input_required';
}

// itemDuration: live work ticks against now; finished work is fixed at its
// finish time (updatedAt).
function itemDuration(it: BackgroundItem, now: number): string {
  if (!it.createdAt) return '';
  const end = isLive(it.status) ? now : (it.updatedAt || 0);
  return end > it.createdAt ? fmtDuration(end - it.createdAt) : '';
}

// The list's group order: live work first, then terminal states by kind.
const GROUPS: Array<{ title: string; match: (s: BackgroundItem['status']) => boolean }> = [
  { title: 'Active', match: s => s === 'working' || s === 'input_required' },
  { title: 'Completed', match: s => s === 'completed' },
  { title: 'Failed', match: s => s === 'failed' },
  { title: 'Cancelled', match: s => s === 'cancelled' },
];

interface BackgroundListPanelProps {
  items: BackgroundItem[];
  onOpen: (id: string) => void;
  onClose: () => void;
  onApprove?: (id: string, scope?: string) => void;
  onReject?: (id: string) => void;
  onStop: (item: BackgroundItem) => void;
  onRetry: (item: BackgroundItem) => void;
}

// BackgroundListPanel is the Inspector's "tasks" lens: background work grouped
// by state (Active first, then terminal kinds), newest first inside each group.
// State is the row's leading dot + its group header; the right-hand label is
// the duration (ticking while live). Rows open the detail lens.
export function BackgroundListPanel({ items, onOpen, onClose, onApprove, onReject, onStop, onRetry }: BackgroundListPanelProps) {
  const hasActive = items.some(it => isLive(it.status));
  // Live durations tick once a second while anything is active.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!hasActive) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [hasActive]);
  const groups = useMemo(() => GROUPS
    .map(g => ({ title: g.title, items: items.filter(it => g.match(it.status)).sort((a, b) => (b.createdAt || 0) - (a.createdAt || 0)) }))
    .filter(g => g.items.length > 0), [items]);

  return (
    <SidePanel icon={StackIcon} title="Tasks" count={items.length} onClose={onClose} storageKey="inspectorWidth">
      {items.length === 0 && <div className="trace-empty">No background work in this session.</div>}
      {/* One line per row by default (the full result lives in the detail
          lens). Live rows add one action line — activity or Approve/Reject
          on the left, Stop isolated on the right. failed is the only terminal
          state that keeps a second line: its error excerpt explains itself
          without a click. */}
      {groups.map(g => (
        <div key={g.title} className="task-group">
          <div className="task-group-title">{g.title}</div>
          {g.items.map(it => (
            <div key={it.id} className="task-row" onClick={() => onOpen(it.id)} role="button" tabIndex={0}
              onKeyDown={e => { if (e.key === 'Enter') onOpen(it.id); }}>
              <div className="task-row-head">
                {statusDot(it.status)}
                {/* A sequence is marked; a task is the unmarked default. */}
                {it.kind === 'workflow' && <WorkflowIcon size={12} className="task-row-kind" />}
                <span className="task-row-label">{it.label}</span>
                {itemDuration(it, now) && <span className="task-row-duration">{itemDuration(it, now)}</span>}
              </div>
              {it.status === 'failed' && it.error && <div className="task-row-error">{it.error}</div>}
              {it.status === 'failed' && ((it.attempt || 1) > 1 || it.retryable) && (
                <div className="task-row-actions" onClick={e => e.stopPropagation()}>
                  {(it.attempt || 1) > 1 && <span className="task-row-activity">attempt {it.attempt}</span>}
                  {/* Derived from the ceiling the server sends, so the offer
                      follows the status: an exhausted task shows nothing. */}
                  {it.retryable && <Button size="small" className="task-row-stop" onClick={() => onRetry(it)}>Retry</Button>}
                </div>
              )}
              {isLive(it.status) && (
                <div className="task-row-actions" onClick={e => e.stopPropagation()}>
                  {it.status === 'input_required' && it.pendingCallId && onApprove && onReject && (
                    <>
                      <Button size="small" variant="primary" onClick={() => onApprove(it.pendingCallId!)}>Approve</Button>
                      <Button size="small" variant="danger" onClick={() => onReject(it.pendingCallId!)}>Reject</Button>
                    </>
                  )}
                  {it.activity && <span className="task-row-activity">{it.activity}</span>}
                  <Button size="small" className="task-row-stop" onClick={() => onStop(it)}>Stop</Button>
                </div>
              )}
            </div>
          ))}
        </div>
      ))}
    </SidePanel>
  );
}

// MdBlock renders one settled markdown text part (worker pipeline, same as chat).
const MdBlock = memo(function MdBlock({ text }: { text: string }) {
  const html = useAsyncMarkdown(text);
  return <div className="markdown-body task-view-text" dangerouslySetInnerHTML={{ __html: html }} />;
});

interface BackgroundDetailPanelProps {
  item: BackgroundItem;
  view: TaskViewState | null;
  onBack: () => void;
  onClose: () => void;
  onApprove?: (id: string, scope?: string) => void;
  onReject?: (id: string) => void;
  onStop: (item: BackgroundItem) => void;
  onRetry: (item: BackgroundItem) => void;
}

// BackgroundDetailPanel is the Inspector's "task" lens: the child session's
// transcript (read-only, live-tailing while the work runs) and its trace. For a
// workflow that transcript is every step's turns in order — the sequence shares
// one session, which is the point of it.
export function BackgroundDetailPanel({ item, view, onBack, onClose, onApprove, onReject, onStop, onRetry }: BackgroundDetailPanelProps) {
  const [tab, setTab] = useState<'transcript' | 'trace'>('transcript');
  const [traceExpanded, setTraceExpanded] = useState(true);
  const [copied, setCopied] = useState(false);
  const live = isLive(item.status);
  // One segment per RUN — a retry starts a new run on the same child session,
  // and a workflow's steps are runs in their own right, so their spans must not
  // interleave on one waterfall. Group order is insertion order (load is row
  // order, live runs append), so segments read oldest first; the labels only
  // appear once there is more than one. "run N", NOT "attempt N": the index
  // counts runs that left spans, and an attempt that died before its first span
  // (a preflight failure) leaves no group — numbering the survivors as attempts
  // would misname them.
  const { traceSegments, spanTotal } = useMemo(() => {
    const entries = Object.entries(view?.traceRuns || {});
    const segments = entries.map(([runId, events], i) => ({
      runId,
      events: events as TraceEventData[],
      label: entries.length > 1 ? `run ${i + 1}` : undefined,
    }));
    return { traceSegments: segments, spanTotal: segments.reduce((n, s) => n + s.events.length, 0) };
  }, [view?.traceRuns]);

  return (
    <SidePanel icon={item.kind === 'workflow' ? WorkflowIcon : StackIcon} title={item.label} onClose={onClose} storageKey="inspectorWidth">
      <div className="task-detail-head">
        <IconButton icon={ArrowLeftIcon} variant="invisible" size="small" aria-label="Back to tasks" onClick={onBack} />
        {statusDot(item.status)}
        {item.activity && <span className="task-row-activity">{item.activity}</span>}
        <div className="task-detail-spacer" />
        {/* Full id, right-aligned; only the action buttons (live work) may
            compress it — then the text ellipsizes, never the buttons. */}
        <button className="task-detail-id" title="Copy id" onClick={() => {
          navigator.clipboard.writeText(item.id).then(() => { setCopied(true); setTimeout(() => setCopied(false), 1500); });
        }}>
          <span className="task-detail-id-text">{item.id}</span> {copied ? <CheckIcon size={12} /> : <CopyIcon size={12} />}
        </button>
        {item.status === 'input_required' && item.pendingCallId && onApprove && onReject && (
          <>
            <Button size="small" variant="primary" onClick={() => onApprove(item.pendingCallId!)}>Approve</Button>
            <Button size="small" variant="danger" onClick={() => onReject(item.pendingCallId!)}>Reject</Button>
          </>
        )}
        {live && <Button size="small" onClick={() => onStop(item)}>Stop</Button>}
        {/* The transcript below is what a retry resumes from, so the action
            belongs beside it. */}
        {item.retryable && <Button size="small" onClick={() => onRetry(item)}>Retry</Button>}
      </div>
      <div className="task-detail-tabs">
        <button className={tab === 'transcript' ? 'active' : ''} onClick={() => setTab('transcript')}>Transcript</button>
        <button className={tab === 'trace' ? 'active' : ''} onClick={() => setTab('trace')}>
          Trace{spanTotal > 0 ? ` (${spanTotal})` : ''}
        </button>
      </div>

      {!view || !view.loaded ? (
        <div className="trace-empty">Loading…</div>
      ) : tab === 'transcript' ? (
        <div className="task-view">
          {view.messages.map((m, i) => {
            if (m.role === 'user') {
              return <div key={i} className="task-view-user">{m.content}</div>;
            }
            if (m.role !== 'turn') return null;
            return (
              <div key={i} className="task-view-turn">
                {(m.parts as TurnPart[] | undefined)?.map((part, j) => {
                  switch (part.type) {
                    case 'text':
                      return <MdBlock key={j} text={part.content} />;
                    case 'thinking':
                      return (
                        <details key={j} className="task-view-thinking">
                          <summary>Thinking</summary>
                          <MdBlock text={part.content} />
                        </details>
                      );
                    case 'tools':
                      return part.toolCalls.map(tc => <ToolCallCard key={tc.tool_call_id} toolCall={tc} live={live} onApprove={onApprove} onReject={onReject} />);
                    case 'error':
                      return <div key={j} className="task-view-error">{part.content}</div>;
                    case 'cancelled':
                      return <div key={j} className="task-view-error">Cancelled</div>;
                    case 'handoff':
                      return <div key={j} className="task-view-handoff">{part.content}</div>;
                    default:
                      return null;
                  }
                })}
              </div>
            );
          })}
          {view.reasoning && (
            <details className="task-view-thinking" open>
              <summary>Thinking…</summary>
              <StreamingMarkdown text={view.reasoning} />
            </details>
          )}
          {view.streaming && <div className="task-view-turn"><StreamingMarkdown text={view.streaming} /></div>}
          {view.messages.length === 0 && !view.streaming && !view.reasoning && (
            <div className="trace-empty">No transcript yet.</div>
          )}
        </div>
      ) : (
        <div className="task-view">
          {spanTotal === 0 ? (
            <div className="trace-empty">No trace events yet.</div>
          ) : (
            <TraceRun
              runId={item.id}
              segments={traceSegments}
              label={item.label}
              isLive={live}
              isExpanded={traceExpanded}
              onToggle={() => setTraceExpanded(v => !v)}
            />
          )}
        </div>
      )}
    </SidePanel>
  );
}
