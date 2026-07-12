import { useState, useMemo, memo } from 'react';
import { Button, IconButton, Label } from '@primer/react';
import { ArrowLeftIcon, StackIcon, CopyIcon, CheckIcon } from '@primer/octicons-react';
import { SidePanel } from '@/layout/SidePanel';
import { ToolCallCard } from '@/features/chat/ToolCallCard';
import { StreamingMarkdown } from '@/features/chat/StreamingMarkdown';
import { TraceRun, type TraceEventData } from '@/features/chat/TracePanel';
import { useAsyncMarkdown } from '@/lib/markdown';
import type { TaskState, TaskViewState } from '@/lib/useAgentSocket';
import type { TurnPart } from '@/lib/timeline';

// Status → Primer Label variant, one place for both lenses.
const STATUS_VARIANT: Record<string, 'accent' | 'attention' | 'success' | 'danger' | 'secondary'> = {
  working: 'accent',
  input_required: 'attention',
  completed: 'success',
  failed: 'danger',
  cancelled: 'secondary',
};

function statusLabel(status: string) {
  return <Label variant={STATUS_VARIANT[status] || 'accent'}>{status.replace('_', ' ')}</Label>;
}

// statusDot is the detail header's compact form of statusLabel: there the
// action buttons (Approve/Reject/Stop) already spell out the state, so color
// suffices — the words move into the tooltip/aria-label for hover and
// assistive tech. Live states pulse, same rhythm as the trace live dot.
function statusDot(status: string) {
  const text = status.replace('_', ' ');
  return <span className={'task-status-dot task-status-dot-' + status} title={text} aria-label={text} role="img" />;
}

interface TaskListPanelProps {
  tasks: Record<string, TaskState>;
  onOpenTask: (taskId: string) => void;
  onClose: () => void;
  onApprove?: (id: string, scope?: string) => void;
  onReject?: (id: string) => void;
  onStop: (taskId: string) => void;
}

// TaskListPanel is the Inspector's "tasks" lens: every background task of the
// session, live ones first, with in-row stop/approve controls. Rows open the
// task detail lens.
export function TaskListPanel({ tasks, onOpenTask, onClose, onApprove, onReject, onStop }: TaskListPanelProps) {
  const list = Object.values(tasks);
  const live = list.filter(t => t.status === 'working' || t.status === 'input_required');
  const done = list.filter(t => t.status !== 'working' && t.status !== 'input_required');
  return (
    <SidePanel icon={StackIcon} title="Tasks" count={list.length} onClose={onClose} storageKey="taskPanelWidth">
      {list.length === 0 && <div className="trace-empty">No background tasks in this session.</div>}
      {/* One line per row by default (the full result lives in the detail
          lens). Live tasks add one action line — activity or Approve/Reject
          on the left, Stop isolated on the right. failed is the only terminal
          state that keeps a second line: its error excerpt explains itself
          without a click. */}
      {[...live, ...done].map(t => (
        <div key={t.taskId} className="task-row" onClick={() => onOpenTask(t.taskId)} role="button" tabIndex={0}
          onKeyDown={e => { if (e.key === 'Enter') onOpenTask(t.taskId); }}>
          <div className="task-row-head">
            <span className="task-row-label">{t.label || t.taskId.slice(0, 8)}</span>
            {statusLabel(t.status)}
          </div>
          {t.status === 'failed' && t.summary && <div className="task-row-error">{t.summary}</div>}
          {(t.status === 'working' || t.status === 'input_required') && (
            <div className="task-row-actions" onClick={e => e.stopPropagation()}>
              {t.status === 'input_required' && t.pendingCallId && onApprove && onReject && (
                <>
                  <Button size="small" variant="primary" onClick={() => onApprove(t.pendingCallId!)}>Approve</Button>
                  <Button size="small" variant="danger" onClick={() => onReject(t.pendingCallId!)}>Reject</Button>
                </>
              )}
              {t.status === 'working' && t.lastTool && <span className="task-row-activity">{t.lastTool}</span>}
              <Button size="small" className="task-row-stop" onClick={() => onStop(t.taskId)}>Stop</Button>
            </div>
          )}
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

interface TaskDetailPanelProps {
  task: TaskState;
  view: TaskViewState | null;
  onBack: () => void;
  onClose: () => void;
  onApprove?: (id: string, scope?: string) => void;
  onReject?: (id: string) => void;
  onStop: (taskId: string) => void;
}

// TaskDetailPanel is the Inspector's "task" lens: the child session's
// transcript (read-only, live-tailing while the task runs) and its trace.
export function TaskDetailPanel({ task, view, onBack, onClose, onApprove, onReject, onStop }: TaskDetailPanelProps) {
  const [tab, setTab] = useState<'transcript' | 'trace'>('transcript');
  const [traceExpanded, setTraceExpanded] = useState(true);
  const [copied, setCopied] = useState(false);
  const live = task.status === 'working' || task.status === 'input_required';
  const traceSegments = useMemo(
    () => [{ runId: task.taskId, events: (view?.traces || []) as TraceEventData[] }],
    [task.taskId, view?.traces],
  );

  return (
    <SidePanel icon={StackIcon} title={task.label || task.taskId.slice(0, 8)} onClose={onClose} storageKey="taskPanelWidth">
      <div className="task-detail-head">
        <IconButton icon={ArrowLeftIcon} variant="invisible" size="small" aria-label="Back to tasks" onClick={onBack} />
        {statusDot(task.status)}
        <button className="task-detail-id" title="Copy task id" onClick={() => {
          navigator.clipboard.writeText(task.taskId).then(() => { setCopied(true); setTimeout(() => setCopied(false), 1500); });
        }}>
          {task.taskId.slice(0, 12)}… {copied ? <CheckIcon size={12} /> : <CopyIcon size={12} />}
        </button>
        <div className="task-detail-spacer" />
        {task.status === 'input_required' && task.pendingCallId && onApprove && onReject && (
          <>
            <Button size="small" variant="primary" onClick={() => onApprove(task.pendingCallId!)}>Approve</Button>
            <Button size="small" variant="danger" onClick={() => onReject(task.pendingCallId!)}>Reject</Button>
          </>
        )}
        {live && <Button size="small" onClick={() => onStop(task.taskId)}>Stop</Button>}
      </div>
      <div className="task-detail-tabs">
        <button className={tab === 'transcript' ? 'active' : ''} onClick={() => setTab('transcript')}>Transcript</button>
        <button className={tab === 'trace' ? 'active' : ''} onClick={() => setTab('trace')}>
          Trace{view && view.traces.length > 0 ? ` (${view.traces.length})` : ''}
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
          {view.traces.length === 0 ? (
            <div className="trace-empty">No trace events yet.</div>
          ) : (
            <TraceRun
              runId={task.taskId}
              segments={traceSegments}
              label={task.label || task.taskId.slice(0, 8)}
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
