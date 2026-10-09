import { useEffect, useState, useMemo, memo } from 'react';
import { Button, IconButton, useConfirm } from '@primer/react';
import { ArrowLeftIcon, StackIcon, CopyIcon, CheckIcon, WorkflowIcon } from '@primer/octicons-react';
import { SidePanel } from '@/layout/SidePanel';
import { Loading } from '@/components/Loading';
import { LoadError } from '@/components/LoadError';
import { ToolCallCard } from '@/features/chat/ToolCallCard';
import { CHECKLIST_KEY, CHECKLIST_TOOL, latestChecklist } from '@/lib/checklist';
import { queuedInputMarkers } from '@/lib/queuedInputs';
import { StreamingMarkdown } from '@/features/chat/StreamingMarkdown';
import { TraceRun, type TraceEventData } from '@/features/chat/TracePanel';
import { useAsyncMarkdown } from '@/lib/markdown';
import { fmtDuration, itemDuration, stepRows, type BackgroundItem } from '@/lib/background';
import type { TaskViewState } from '@/lib/useAgentSocket';
import type { TurnPart } from '@/lib/timeline';
import { useChatActions, useChatSession, useChatBackground } from '@/features/chat/ChatSessionContext';
import { RejectButton } from '@/features/chat/RejectButton';
import { useDecisionHold } from '@/features/chat/useDecisionHold';
import { AgentAvatar } from '@/components/AgentAvatar';
import { api } from '@/lib/api';
import { toast } from '@/lib/toast';
import { STEP_APPROVAL_TOOL } from '@/lib/protocol';
import { isLive, statusDot } from '@/lib/status';
import { requestedBy } from '@/lib/background';
import { activates } from '@/lib/activation';
import { useCopy, useNowTicker } from '@/lib/hooks';

// The panel behind the top bar's Tasks button: spawned tasks and workflow
// executions in one list, under the one word "Tasks" — invariant 36.

// The list's group order: live work first, then terminal states by kind.
const GROUPS: Array<{ title: string; match: (s: BackgroundItem['status']) => boolean }> = [
  { title: 'Active', match: s => s === 'working' || s === 'input_required' },
  { title: 'Completed', match: s => s === 'completed' },
  { title: 'Failed', match: s => s === 'failed' },
  { title: 'Cancelled', match: s => s === 'cancelled' },
];

// BackgroundListPanel is the Inspector's "tasks" lens: background work grouped
// by state (Active first, then terminal kinds), newest first in each group; the
// right-hand label is the duration, ticking while live. Rows open the detail lens.
export function BackgroundListPanel({ onClose }: { onClose: () => void }) {
  const items = useChatBackground();
  const { tasksError, agentNames } = useChatSession();
  const { approve: onApprove, reject: onReject, inspectTask: onOpen, stopTask, retryTask, retryTasks } = useChatActions();
  const { held, decide } = useDecisionHold();
  const hasActive = items.some(it => isLive(it.status));
  // Live durations tick once a second while anything is active.
  const now = useNowTicker(hasActive);
  const groups = useMemo(() => GROUPS
    .map(g => ({ title: g.title, items: items.filter(it => g.match(it.status)).sort((a, b) => (b.createdAt || 0) - (a.createdAt || 0)) }))
    .filter(g => g.items.length > 0), [items]);

  return (
    <SidePanel icon={StackIcon} title="Tasks" count={items.length} onClose={onClose} storageKey="inspectorWidth">
      {/* A failed read says so above whatever rows live events brought in;
          the empty state is for a list that loaded empty (invariant 79). */}
      {tasksError && <LoadError what="background tasks" error={tasksError} onRetry={retryTasks} />}
      {items.length === 0 && !tasksError && <div className="trace-empty">No background work in this session.</div>}
      {/* One line per row by default; live rows add an action line (activity or
          Approve/Reject left, Stop right), and failed alone keeps a second
          line for its error excerpt. */}
      {groups.map(g => (
        <div key={g.title} className="task-group">
          <div className="task-group-title">{g.title}</div>
          {g.items.map(it => (
            <div key={it.id} className="task-row" onClick={() => onOpen(it.id)} role="button" tabIndex={0}
              onKeyDown={e => { if (activates(e)) { e.preventDefault(); onOpen(it.id); } }}>
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
                      <RejectButton kind={it.pendingToolName === STEP_APPROVAL_TOOL ? 'step' : undefined} disabled={held(it.pendingCallId)} onReject={reason => decide(it.pendingCallId!, () => onReject(it.pendingCallId!, reason))} />
                      <span className="task-row-activity">{requestedBy(it, agentNames)}</span>
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

// TaskViewHandoff is the agent switch inside a task's transcript — the
// avatars when the event carried the config ids, the plain line otherwise.
function TaskViewHandoff({ part }: { part: Extract<TurnPart, { type: 'handoff' }> }) {
  const { agentAvatars } = useChatSession();
  if (!part.from || !part.to) return <div className="task-view-handoff">{part.content}</div>;
  return (
    <div className="task-view-handoff pt-handoff-agents">
      <AgentAvatar name={part.from} avatar={part.fromId ? agentAvatars[part.fromId] : undefined} size={20} />
      {part.from}
      <span className="pt-handoff-arrow">→</span>
      <AgentAvatar name={part.to} avatar={part.toId ? agentAvatars[part.toId] : undefined} size={20} />
      {part.to}
    </div>
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

// BackgroundMissingPanel stands in for the detail lens while a deep-linked task
// is not in the session's list (still loading, removed with its session, or
// never carried by a fork's copy): it says so and leads back to the list.
export function BackgroundMissingPanel({ taskId, loading, onBack, onClose }: { taskId: string; loading: boolean; onBack: () => void; onClose: () => void }) {
  return (
    <SidePanel icon={StackIcon} title={loading ? 'Task' : 'Task not found'} onClose={onClose} storageKey="inspectorWidth">
      <div className="task-detail-head">
        <IconButton icon={ArrowLeftIcon} variant="invisible" size="small" aria-label="Back to tasks" tooltipDirection="se" onClick={onBack} />
        <div className="task-detail-spacer" />
        <span className="task-detail-id"><span className="task-detail-id-text">{taskId}</span></span>
      </div>
      {loading
        ? <Loading kind="inline" />
        : <div className="trace-empty">This task is not in the session's list — it may have been removed, belong to another session, or the list could not be loaded (open Tasks to retry).</div>}
    </SidePanel>
  );
}

// BackgroundDetailPanel is the Inspector's "task" lens: the child session's
// transcript (read-only, live-tailing while the work runs) and its trace; a
// workflow's steps share that session, so its transcript is every step in order.
export function BackgroundDetailPanel({ item, view, onBack, onClose }: BackgroundDetailPanelProps) {
  const { approve: onApprove, reject: onReject, stopTask, retryTask } = useChatActions();
  const { sessionId } = useChatSession();
  const confirm = useConfirm();
  // Run again starts a NEW execution with the same brief (side effects happen
  // again, hence the confirmation); a retry resumes this one where it stopped.
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
  // One trace segment per run (a retry, a workflow step), oldest first, labelled
  // only when several; "run N" not "attempt N" — a span-less attempt leaves no segment.
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
  // The task's checklist as its run last wrote it (checklist.md, invariant 91),
  // re-read as the transcript grows; absent when the run keeps none.
  const taskChecklist = useMemo(() => latestChecklist(view?.messages || []), [view?.messages]);
  const taskMarkers = useMemo(() => Object.values(queuedInputMarkers(view?.messages || [])).flat(), [view?.messages]);
  const [checklistMd, setChecklistMd] = useState<string | null>(null);
  const childSessionId = view?.childSessionId;
  const transcriptLen = view?.messages.length || 0;
  useEffect(() => {
    if (!childSessionId) return;
    let alive = true;
    api.sessions.memoryKey(childSessionId, CHECKLIST_KEY)
      .then(m => { if (alive) setChecklistMd(m.content || null); })
      .catch(() => { if (alive) setChecklistMd(null); });
    return () => { alive = false; };
  }, [childSessionId, transcriptLen, item.status]);

  return (
    <SidePanel icon={item.kind === 'workflow' ? WorkflowIcon : StackIcon} title={item.label} onClose={onClose} storageKey="inspectorWidth">
      <div className="task-detail-head">
        <IconButton icon={ArrowLeftIcon} variant="invisible" size="small" aria-label="Back to tasks" tooltipDirection="se" onClick={onBack} />
        {statusDot(item.status)}
        {item.activity && <span className="task-row-activity">{item.activity}</span>}
        <div className="task-detail-spacer" />
        {/* Full id, right-aligned; only the action buttons (live work) may
            compress it — then the text ellipsizes, never the buttons. */}
        <button className="task-detail-id" title="Copy id" aria-label="Copy id" onClick={() => copy(item.id)}>
          <span className="task-detail-id-text">{item.id}</span> {copied ? <CheckIcon size={12} /> : <CopyIcon size={12} />}
        </button>
        {item.status === 'input_required' && item.pendingCallId && onApprove && onReject && (
          <>
            <Button size="small" variant="primary" disabled={held(item.pendingCallId)} onClick={() => decide(item.pendingCallId!, () => onApprove(item.pendingCallId!))}>Approve</Button>
            <RejectButton kind={item.pendingToolName === STEP_APPROVAL_TOOL ? 'step' : undefined} disabled={held(item.pendingCallId)} onReject={reason => decide(item.pendingCallId!, () => onReject(item.pendingCallId!, reason))} />
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
              <thead>
                <tr>
                  <th scope="col" className="wf-steps-index" aria-label="Step number">#</th>
                  <th scope="col">Step</th>
                  <th scope="col">Status</th>
                  <th scope="col">Verdict</th>
                  <th scope="col" className="wf-steps-num">Duration</th>
                  <th scope="col" className="wf-steps-num">Tokens</th>
                </tr>
              </thead>
              <tbody>
                {steps.map((row, i) => (
                  <tr key={row.runId} className={i === steps.length - 1 && isLive(item.status) ? 'wf-steps-live' : undefined}>
                    <td className="wf-steps-index">{row.index}</td>
                    <td className="wf-steps-name">{row.name}{row.retry && <span className="wf-steps-retry"> · retry</span>}</td>
                    <td className="wf-steps-outcome">{row.outcome && <span className={'wf-outcome wf-outcome-' + row.outcome}>{row.outcome}</span>}</td>
                    <td className="wf-steps-outcome">{row.verdict && <span className={'wf-outcome wf-outcome-' + row.verdict}>{row.verdict}</span>}</td>
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
        <Loading kind="panel" />
      ) : tab === 'transcript' ? (
        <div className="task-view">
          {checklistMd && (
            <div className="task-view-checklist">
              <div className="task-view-checklist-title">Checklist</div>
              <MdBlock text={checklistMd} />
            </div>
          )}
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
                      return part.toolCalls.map(tc => (
                        <ToolCallCard key={tc.tool_call_id} toolCall={tc} live={live}
                          stale={tc.tool_name === CHECKLIST_TOOL && !!taskChecklist && tc.tool_call_id !== taskChecklist.callId} />
                      ));
                    case 'error':
                      return <div key={j} className="task-view-error">{part.content}</div>;
                    case 'cancelled':
                      return <div key={j} className="task-view-error">Cancelled</div>;
                    case 'handoff':
                      return <TaskViewHandoff key={j} part={part} />;
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
              markers={taskMarkers}
            />
          )}
        </div>
      )}
    </SidePanel>
  );
}
