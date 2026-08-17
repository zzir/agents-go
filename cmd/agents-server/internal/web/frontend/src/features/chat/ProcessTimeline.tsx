import { useState } from 'react';
import { Label } from '@primer/react';
import { ArrowSwitchIcon, ChevronRightIcon } from '@primer/octicons-react';
import { DIAGNOSTIC_LABELS, type RunDiagnostic } from '@/lib/protocol';
import type { TurnPart } from '@/lib/timeline';
import { ToolCallCard } from '@/features/chat/ToolCallCard';
import { useChatSession, useChatActions } from '@/features/chat/ChatSessionContext';

function TimelineThinking({ content }: { content: string }) {
  return (
    <div className="pt-entry">
      <div className="pt-thinking">{content}</div>
    </div>
  );
}

function TimelineHandoff({ content }: { content: string }) {
  return (
    <div className="pt-entry">
      <div className="pt-handoff">
        <ArrowSwitchIcon size={14} />
        <span>{content}</span>
      </div>
    </div>
  );
}

// DiagnosticBadge reports trouble the run survived.
//
// It sits on the process group rather than in the transcript because that is
// what it describes: not something the agent said, but how the turn went. A run
// that answered after three retries or on a fallback model looks identical to
// one that answered first time, and this is the difference — the thing that
// explains why the answer took forty seconds, or why it is worse than usual.
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

// Space-separated tool call ids across a turn's parts, for the group's
// data-anchor-ids attribute.
function toolCallIds(parts: TurnPart[]): string {
  return parts.flatMap(p => (p.type === 'tools' ? p.toolCalls.map(tc => tc.tool_call_id) : [])).join(' ');
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
  const { compacting, diagnostics } = useChatSession();
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

  // Once the turn's answer text starts streaming, this group's thinking/tool
  // phase is finished even though the run is still live: settle the label and
  // let it collapse like a done group, instead of pinning "Thinking…" over an
  // already-visible answer. The run.reasoning_item / run.message events that
  // freeze the live preview into parts only land after the whole model call, so
  // the live `reasoning` state otherwise lingers through the entire answer.
  const active = live && !textStreaming;

  const shouldShow = pendingCount > 0 || (expanded ?? active);

  // A pending approval is the group's status whether or not the run is still
  // "active": in the steady paused state running is false, so gate it above
  // `active` — otherwise it falls through to the settled step count and the
  // "Waiting for approval" wording only flashes in the run.tool_call→interrupted
  // window.
  const label = pendingCount > 0
    ? 'Waiting for approval'
    : active
      ? (compacting ? 'Compacting context…'
        : runningTool ? (runningToolCount > 1 ? 'Running ' + runningToolCount + ' tools…' : 'Running ' + runningTool + '…')
        : reasoning ? 'Thinking…' : 'Working…')
      : stepCount + ' step' + (stepCount > 1 ? 's' : '');

  return (
    // The call ids this group holds, so a jump aimed at a tool card inside a
    // COLLAPSED group can find the group (the cards themselves are not in the
    // DOM until it opens) and expand it.
    <div className="process-group" data-anchor-ids={toolCallIds(parts) || undefined}>
      <div
        className={'process-group-toggle' + (shouldShow ? ' expanded' : '')}
        onClick={() => setExpanded(!shouldShow)}
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
            if (p.type === 'handoff') return <TimelineHandoff key={'pt-' + i} content={p.content || ''} />;
            if (p.type === 'tools') {
              return p.toolCalls.map(tc => (
                <ToolCallCard key={tc.tool_call_id} toolCall={tc} live={live} onInspectTask={inspectTask} onRetryTask={retryTask} />
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
