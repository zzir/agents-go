import { memo } from 'react';
import { Label } from '@primer/react';
import { WorkflowIcon, ZapIcon } from '@primer/octicons-react';
import type { WorkflowStartedNote } from '@/lib/timeline';
import { useChatActions, useChatSession } from '@/features/chat/ChatSessionContext';
import { AgentAvatar } from '@/components/AgentAvatar';

// originText says who started the execution, the way the trace card and the
// chip both phrase it.
export function originText(origin: WorkflowStartedNote['origin']): string {
  if (origin.kind !== 'trigger') return 'you';
  const kind = origin.trigger_kind || 'trigger';
  return origin.schedule ? `${kind} ${origin.schedule}` : kind;
}

// WorkflowStartedChip is the row a person's or a trigger's workflow start
// leaves in the conversation: the exchange's question, when no run asked. It
// says what started and who asked, and opens the execution — the task's
// detail is where the brief, the steps and the transcript live. It is
// anchored by the run id of the wake-up run that later delivered the result,
// so the trace panel's jump from that run's card lands here. A trigger's
// agent turn leaves the same row before the message it sends — the reader
// sees the next question was an automation's; that message IS the brief,
// so the row is the label alone.
export const WorkflowStartedChip = memo(function WorkflowStartedChip({ note, content, traceRunId, msgIdx, entryId }:
  { note: WorkflowStartedNote; content: string; traceRunId?: string | null; msgIdx: number; entryId?: string }) {
  const { inspectTask } = useChatActions();
  const { agentAvatars } = useChatSession();
  // The note's data names the workflow or the agent; a row without either
  // (the extra missing) shows the line of text the server wrote instead of
  // an empty name.
  const name = note.workflowName || note.workflowId.slice(0, 8);
  const agentTurn = !!note.agentName && !name;
  const label = name ? `Workflow "${name}" started by ${originText(note.origin)}`
    : note.agentName ? `Agent "${note.agentName}" prompted by ${originText(note.origin)}`
    : (content.trim() || 'Workflow started');
  const Icon = agentTurn ? ZapIcon : WorkflowIcon;
  const chip = (
    // One string: the Label is a flex row, where whitespace between nodes
    // renders as nothing.
    <Label variant="secondary" className="wf-started-label">
      <Icon size={12} />
      {agentTurn && <AgentAvatar name={note.agentName} avatar={note.agentConfigId ? agentAvatars[note.agentConfigId] : undefined} size={14} />}
      <span>{label}</span>
    </Label>
  );
  return (
    <div className="message message-system wf-started" data-run-id={traceRunId || undefined} data-msg-idx={msgIdx} data-anchor-id={entryId || undefined}>
      {note.taskId && !agentTurn ? (
        <button type="button" className="wf-started-open" title="Open the execution" onClick={() => inspectTask(note.taskId)}>
          {chip}
        </button>
      ) : chip}
    </div>
  );
});
