import { memo } from 'react';
import { Label } from '@primer/react';
import { WorkflowIcon, ZapIcon } from '@primer/octicons-react';
import { originText, type WorkflowStartedNote } from '@/lib/timeline';
import { useChatActions, useChatSession } from '@/features/chat/ChatSessionContext';
import { AgentAvatar } from '@/components/AgentAvatar';

// WorkflowStartedChip is the row a workflow start leaves in the conversation,
// opening the execution; anchored by the wake-up run's id, so the trace panel's
// jump lands here. A trigger's agent turn leaves it as a bare label before its message.
export const WorkflowStartedChip = memo(function WorkflowStartedChip({ note, content, traceRunId, msgIdx }:
  { note: WorkflowStartedNote; content: string; traceRunId?: string | null; msgIdx: number }) {
  const { inspectTask } = useChatActions();
  const { agentAvatars } = useChatSession();
  // The note names the workflow or the agent; a row with neither shows the
  // line of text the server wrote instead of an empty name.
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
      {agentTurn && <AgentAvatar name={note.agentName} avatar={note.agentConfigId ? agentAvatars[note.agentConfigId] : undefined} size={16} />}
      <span>{label}</span>
    </Label>
  );
  return (
    <div className="message message-system wf-started" data-run-id={traceRunId || undefined} data-msg-idx={msgIdx}>
      {note.taskId && !agentTurn ? (
        <button type="button" className="wf-started-open" title="Open the execution" onClick={() => inspectTask(note.taskId)}>
          {chip}
        </button>
      ) : chip}
    </div>
  );
});
