import { useState } from 'react';
import { Label } from '@primer/react';
import { ArrowSwitchIcon, ChevronRightIcon } from '@primer/octicons-react';
import { DIAGNOSTIC_LABELS, type RunDiagnostic } from '@/lib/protocol';
import type { TurnPart } from '@/lib/timeline';
import { ToolCallCard } from '@/features/chat/ToolCallCard';
import { CHECKLIST_TOOL } from '@/lib/checklist';
import { useChatSession, useChatActions } from '@/features/chat/ChatSessionContext';
import { AgentAvatar } from '@/components/AgentAvatar';

function TimelineThinking({ content }: { content: string }) {
  return (
    <div className="pt-entry">
      <div className="pt-thinking">{content}</div>
    </div>
  );
}

function TimelineHandoff({ part }: { part: Extract<TurnPart, { type: 'handoff' }> }) {
  const { agentAvatars } = useChatSession();
  return (
    <div className="pt-entry">
      <div className="pt-handoff">
        <ArrowSwitchIcon size={14} />
        {part.from && part.to ? (
          <span className="pt-handoff-agents">
            <AgentAvatar name={part.from} avatar={part.fromId ? agentAvatars[part.fromId] : undefined} size={20} />
            {part.from}
            <span className="pt-handoff-arrow">→</span>
            <AgentAvatar name={part.to} avatar={part.toId ? agentAvatars[part.toId] : undefined} size={20} />
            {part.to}
          </span>
        ) : (
          <span>{part.content}</span>
        )}
      </div>
    </div>
  );
}

// DiagnosticBadge reports trouble the run survived (retries, a fallback model):
// it sits on the process group, not in the transcript, since it describes how
// the turn went, not what the agent said.
function DiagnosticBadge({ diagnostics }: { diagnostics?: RunDiagnostic[] }) {
  if (!diagnostics || diagnostics.length === 0) return null;
  // Counted by kind: three retries is one fact, not three.
  const counts = new Map<string, number>();
  for (const d of diagnostics) counts.set(d.type, (counts.get(d.type) || 0) + 1);
  const summary = [...counts.entries()]
    .map(([type, n]) => (DIAGNOSTIC_LABELS[type] || type) + (n > 1 ? ' ×' + n : ''))
    .join(', ');
  const detail = diagnostics
    .map(d => (DIAGNOSTIC_LABELS[d.type] || d.type) + (d.message ? ': ' + d.message : ''))
    .join('\n');
  return (
    <Label variant="attention" className="process-status" title={detail}>{summary}</Label>
  );
}

interface ProcessTimelineProps {
  parts: TurnPart[];
  live: boolean;
  reasoning: string | null;
  // The turn's answer text has started streaming, so this group's thinking/tool
  // phase is done even while the run is still live.
  textStreaming?: boolean;
}

// One collapsible group of thinking + tool-call parts. `live` marks the group
// still executing (the trailing one while its run is live): it stays open and
// shows a status label; settled groups collapse to "N steps".
export function ProcessTimeline({ parts, live, reasoning, textStreaming }: ProcessTimelineProps) {
  // The live run's state (compaction, diagnostics) belongs to the executing
  // group only; a settled group shows none of it.
  const { compacting, diagnostics, checklist } = useChatSession();
  const { inspectTask, retryTask } = useChatActions();
  // null = auto (open while live, closed once done); true/false = user override.
  const [expanded, setExpanded] = useState<boolean | null>(null);

  let stepCount = 0;
  let pendingCount = 0;
  let runningTool: string | null = null;
  let runningToolCount = 0;
  for (const p of parts) {
    if (p.type === 'tools') {
      stepCount += p.toolCalls.length;
      for (const tc of p.toolCalls) {
        if (tc.needs_approval && !tc.status) pendingCount++;
        else if (!tc.output && tc.status !== 'completed' && tc.status !== 'rejected') { runningToolCount++; if (!runningTool) runningTool = tc.tool_name; }
      }
    } else {
      stepCount++;
    }
  }
  if (live && reasoning) stepCount++;

  if (stepCount === 0) return null;

  // Streaming answer text ends this group's phase even while the run is live:
  // the live `reasoning` state only settles after the whole model call.
  const active = live && !textStreaming;

  const shouldShow = pendingCount > 0 || (expanded ?? active);

  // A pending approval is the status even when the run is not `active` (paused:
  // running is false), so it is gated first; below `active` it would only flash
  // at the interrupt.
  const label = pendingCount > 0
    ? 'Waiting for approval'
    : active
      ? (compacting ? 'Compacting context…'
        : runningTool ? (runningToolCount > 1 ? 'Running ' + runningToolCount + ' tools…' : 'Running ' + runningTool + '…')
        : reasoning ? 'Thinking…' : 'Working…')
      : stepCount + ' step' + (stepCount > 1 ? 's' : '');

  const toggle = () => setExpanded(!shouldShow);
  return (
    <div className="process-group">
      <div
        className={'process-group-toggle' + (shouldShow ? ' expanded' : '')}
        role="button"
        tabIndex={0}
        aria-expanded={shouldShow}
        onClick={toggle}
        onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(); } }}
      >
        <ChevronRightIcon size={16} className="process-icon" />
        <span>{label}</span>
        {pendingCount > 0 && <Label variant="accent" className="process-status">{pendingCount + ' pending'}</Label>}
        <DiagnosticBadge diagnostics={live ? diagnostics : undefined} />
      </div>
      {shouldShow && (
        <div className="process-timeline">
          {parts.map((p, i) => {
            if (p.type === 'thinking') return <TimelineThinking key={'pt-' + i} content={p.content || ''} />;
            if (p.type === 'handoff') return <TimelineHandoff key={'pt-' + i} part={p} />;
            if (p.type === 'tools') {
              return p.toolCalls.map(tc => (
                <ToolCallCard key={tc.tool_call_id} toolCall={tc} live={live} onInspectTask={inspectTask} onRetryTask={retryTask}
                  stale={tc.tool_name === CHECKLIST_TOOL && !!checklist && tc.tool_call_id !== checklist.callId} />
              ));
            }
            return null;
          })}
          {live && reasoning && <TimelineThinking content={reasoning} />}
        </div>
      )}
    </div>
  );
}
