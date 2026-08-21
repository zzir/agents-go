import { useState, useMemo, memo } from 'react';
import { Button, IconButton, useConfirm } from '@primer/react';
import { ArrowLeftIcon, StackIcon, CopyIcon, CheckIcon, WorkflowIcon } from '@primer/octicons-react';
import { SidePanel } from '@/layout/SidePanel';
import { ToolCallCard } from '@/features/chat/ToolCallCard';
import { StreamingMarkdown } from '@/features/chat/StreamingMarkdown';
import { TraceRun, type TraceEventData } from '@/features/chat/TracePanel';
import { useAsyncMarkdown } from '@/lib/markdown';
import { fmtDuration, itemDuration, stepRows, type BackgroundItem } from '@/lib/background';
import type { TaskViewState } from '@/lib/useAgentSocket';
import type { TurnPart } from '@/lib/timeline';
import { useChatActions, useChatSession, useChatBackground } from '@/features/chat/ChatSessionContext';
import { useDecisionHold } from '@/features/chat/useDecisionHold';
import { api } from '@/lib/api';
import { toast } from '@/lib/toast';
import { isLive, statusDot } from '@/lib/status';
import { useCopy, useNowTicker } from '@/lib/hooks';

// This is the panel behind the top bar's Tasks button. It holds both kinds of
// background work — spawned tasks and workflow executions — because to a
// person they are one thing: work happening in a session they are not in. The
// UI keeps saying "Tasks"; a second word would be a second concept.

// The list's group order: live work first, then terminal states by kind.
const GROUPS: Array<{ title: string; match: (s: BackgroundItem['status']) => boolean }> = [
  { title: 'Active', match: s => s === 'working' || s === 'input_required' },
  { title: 'Completed', match: s => s === 'completed' },
  { title: 'Failed', match: s => s === 'failed' },
  { title: 'Cancelled', match: s => s === 'cancelled' },
];

// BackgroundListPanel is the Inspector's "tasks" lens: background work grouped
// by state (Active first, then terminal kinds), newest first inside each group.
// State is the row's leading dot + its group header; the right-hand label is
// the duration (ticking while live). Rows open the detail lens.
export function BackgroundListPanel({ onClose }: { onClose: () => void }) {
  const items = useChatBackground();
  const { approve: onApprove, reject: onReject, inspectTask: onOpen, stopTask, retryTask } = useChatActions();
  const { held, decide } = useDecisionHold();
  const hasActive = items.some(it => isLive(it.status));
  // Live durations tick once a second while anything is active.
  const now = useNowTicker(hasActive);
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
                  {it.retryable && <Button size="small" className="task-row-stop" onClick={() => retryTask(it.id)}>Retry</Button>}
                </div>
              )}
              {isLive(it.status) && (
                <div className="task-row-actions" onClick={e => e.stopPropagation()}>
                  {it.status === 'input_required' && it.pendingCallId && onApprove && onReject && (
                    <>
                      <Button size="small" variant="primary" disabled={held(it.pendingCallId)} onClick={() => decide(it.pendingCallId!, () => onApprove(it.pendingCallId!))}>Approve</Button>
                      <Button size="small" variant="danger" disabled={held(it.pendingCallId)} onClick={() => decide(it.pendingCallId!, () => onReject(it.pendingCallId!))}>Reject</Button>
                    </>
                  )}
                  {it.activity && <span className="task-row-activity">{it.activity}</span>}
                  <Button size="small" className="task-row-stop" onClick={() => stopTask(it.id)}>Stop</Button>
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
}

// BackgroundDetailPanel is the Inspector's "task" lens: the child session's
// transcript (read-only, live-tailing while the work runs) and its trace. For a
// workflow that transcript is every step's turns in order — the sequence shares
// one session, which is the point of it.
// BackgroundMissingPanel stands in for the detail lens while the task named
// by a deep link is not in the conversation's list: still loading, or gone —
// a row removed with its conversation, or one a fork's copy never carried.
// Either way the panel opens, says so, and leads back to the list.
export function BackgroundMissingPanel({ taskId, loading, onBack, onClose }: { taskId: string; loading: boolean; onBack: () => void; onClose: () => void }) {
  return (
    <SidePanel icon={StackIcon} title={loading ? 'Task' : 'Task not found'} onClose={onClose} storageKey="inspectorWidth">
      <div className="task-detail-head">
        <IconButton icon={ArrowLeftIcon} variant="invisible" size="small" aria-label="Back to tasks" onClick={onBack} />
        <div className="task-detail-spacer" />
        <span className="task-detail-id"><span className="task-detail-id-text">{taskId}</span></span>
      </div>
      <div className="trace-empty">
        {loading ? 'Loading the task…' : 'This task is not in the conversation\'s list — it may have been removed, belong to another conversation, or the list could not be loaded (reopen the conversation to try again).'}
      </div>
    </SidePanel>
  );
}

export function BackgroundDetailPanel({ item, view, onBack, onClose }: BackgroundDetailPanelProps) {
  const { approve: onApprove, reject: onReject, stopTask, retryTask } = useChatActions();
  const { sessionId } = useChatSession();
  const confirm = useConfirm();
  // A finished workflow can be run again with the same brief — a NEW execution
  // (its side effects happen again, hence the confirmation), unlike a retry,
  // which resumes this one from where it stopped.
  const rerunnable = item.kind === 'workflow' && !isLive(item.status) && !!item.state?.workflow_id && !!sessionId;
  const runAgain = async () => {
    if (!item.state?.workflow_id || !sessionId) return;
    if (!await confirm({ title: 'Run again?', content: 'A new execution starts from the first step, with the same brief. Whatever the steps do — files, commands, sends — happens again.', confirmButtonContent: 'Run again' })) return;
    try {
      await api.workflows.run(item.state.workflow_id, { session_id: sessionId, input: item.state.input || '' });
      toast.success(`Started "${item.label}" again`);
    } catch (e) {
      toast.error((e as Error).message || 'Could not start the workflow');
    }
  };
  // A workflow opens on its steps: how far it got and what each cost is the
  // question a sequence gets asked; a task's only shape is its transcript.
  const [tab, setTab] = useState<'steps' | 'transcript' | 'trace'>(item.kind === 'workflow' ? 'steps' : 'transcript');
  const [traceExpanded, setTraceExpanded] = useState(true);
  const { copied, copy } = useCopy();
  const { held, decide } = useDecisionHold();
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
  // The launch log joined with each run's trace: which step ran, how it
  // ended, what it cost. Empty for a task.
  const steps = useMemo(
    () => (item.kind === 'workflow' ? stepRows(item.state, item.status, view?.traceRuns as Record<string, TraceEventData[]> | undefined) : []),
    [item.kind, item.state, item.status, view?.traceRuns],
  );

  return (
    <SidePanel icon={item.kind === 'workflow' ? WorkflowIcon : StackIcon} title={item.label} onClose={onClose} storageKey="inspectorWidth">
      <div className="task-detail-head">
        <IconButton icon={ArrowLeftIcon} variant="invisible" size="small" aria-label="Back to tasks" onClick={onBack} />
        {statusDot(item.status)}
        {item.activity && <span className="task-row-activity">{item.activity}</span>}
        <div className="task-detail-spacer" />
        {/* Full id, right-aligned; only the action buttons (live work) may
            compress it — then the text ellipsizes, never the buttons. */}
        <button className="task-detail-id" title="Copy id" onClick={() => copy(item.id)}>
          <span className="task-detail-id-text">{item.id}</span> {copied ? <CheckIcon size={12} /> : <CopyIcon size={12} />}
        </button>
        {item.status === 'input_required' && item.pendingCallId && onApprove && onReject && (
          <>
            <Button size="small" variant="primary" disabled={held(item.pendingCallId)} onClick={() => decide(item.pendingCallId!, () => onApprove(item.pendingCallId!))}>Approve</Button>
            <Button size="small" variant="danger" disabled={held(item.pendingCallId)} onClick={() => decide(item.pendingCallId!, () => onReject(item.pendingCallId!))}>Reject</Button>
          </>
        )}
        {live && <Button size="small" onClick={() => stopTask(item.id)}>Stop</Button>}
        {/* The transcript below is what a retry resumes from, so the action
            belongs beside it. */}
        {item.retryable && <Button size="small" onClick={() => retryTask(item.id)}>Retry</Button>}
        {rerunnable && <Button size="small" onClick={runAgain}>Run again</Button>}
      </div>
      <div className="task-detail-tabs">
        {item.kind === 'workflow' && (
          <button className={tab === 'steps' ? 'active' : ''} onClick={() => setTab('steps')}>
            Steps{steps.length > 0 ? ` (${steps.length})` : ''}
          </button>
        )}
        <button className={tab === 'transcript' ? 'active' : ''} onClick={() => setTab('transcript')}>Transcript</button>
        <button className={tab === 'trace' ? 'active' : ''} onClick={() => setTab('trace')}>
          Trace{spanTotal > 0 ? ` (${spanTotal})` : ''}
        </button>
      </div>

      {tab === 'steps' ? (
        <div className="task-view">
          {steps.length === 0 ? (
            <div className="trace-empty">No step has started yet.</div>
          ) : (
            <table className="wf-steps">
              <tbody>
                {steps.map((row, i) => (
                  <tr key={row.runId} className={i === steps.length - 1 && isLive(item.status) ? 'wf-steps-live' : undefined}>
                    <td className="wf-steps-index">{row.index}</td>
                    <td className="wf-steps-name">{row.name}{row.retry && <span className="wf-steps-retry"> · retry</span>}</td>
                    <td className="wf-steps-outcome"><span className={'wf-outcome wf-outcome-' + row.outcome}>{row.outcome}</span></td>
                    <td className="wf-steps-num">{row.durationMs !== undefined ? fmtDuration(row.durationMs) : ''}</td>
                    <td className="wf-steps-num">{row.tokens ? `↑${row.tokens.input} ↓${row.tokens.output}` : ''}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {item.state?.input && <div className="wf-steps-brief"><span className="wf-steps-brief-label">Brief</span>{item.state.input}</div>}
        </div>
      ) : !view || !view.loaded ? (
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
                      // A foreign transcript: approvals still route by call id, but the
                      // task offers (inspect/retry) belong to the parent's cards only.
                      return part.toolCalls.map(tc => <ToolCallCard key={tc.tool_call_id} toolCall={tc} live={live} />);
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
              payloadSessionId={view?.childSessionId}
            />
          )}
        </div>
      )}
    </SidePanel>
  );
}
