import { memo, useState } from 'react';
import { IconButton, Label } from '@primer/react';
import { PulseIcon, WorkflowIcon, ZapIcon } from '@primer/octicons-react';
import type { WorkflowStartedNote } from '@/lib/timeline';
import { useChatActions } from '@/features/chat/ChatSessionContext';

// originText says who started the execution, the way the trace card and the
// chip both phrase it.
export function originText(origin: WorkflowStartedNote['origin']): string {
  if (origin.kind !== 'trigger') return 'you';
  const kind = origin.trigger_kind || 'trigger';
  return origin.schedule ? `${kind} ${origin.schedule}` : kind;
}

// WorkflowStartedChip is the row a person's or a trigger's workflow start
// leaves in the conversation: the exchange's question, when no run asked.
// It carries the run id of the wake-up run that later delivered the result,
// so the trace panel's jump lands here and the chip can open that trace. A
// trigger's agent turn leaves the same row before the message it sends —
// the reader sees the next question was an automation's.
export const WorkflowStartedChip = memo(function WorkflowStartedChip({ note, content, traceRunId, msgIdx, entryId }:
  { note: WorkflowStartedNote; content: string; traceRunId?: string | null; msgIdx: number; entryId?: string }) {
  const { openTrace } = useChatActions();
  const [open, setOpen] = useState(false);
  const brief = note.brief.replace(/\s+/g, ' ').trim();
  const short = brief.length > 100 ? brief.slice(0, 100) + '…' : brief;
  // The note's data names the workflow or the agent; a row without either
  // (the extra missing) shows the line of text the server wrote instead of
  // an empty name.
  const name = note.workflowName || note.workflowId.slice(0, 8);
  const label = name ? `Workflow "${name}" started by ${originText(note.origin)}`
    : note.agentName ? `Agent "${note.agentName}" prompted by ${originText(note.origin)}`
    : (content.trim() || 'Workflow started');
  const Icon = note.agentName && !name ? ZapIcon : WorkflowIcon;
  return (
    <div className="message message-system wf-started" data-run-id={traceRunId || undefined} data-msg-idx={msgIdx} data-anchor-id={entryId || undefined}>
      {/* One string: the Label is a flex row, where whitespace between nodes
          renders as nothing. */}
      <Label variant="secondary" className="wf-started-label">
        <Icon size={12} />
        <span>{label}</span>
      </Label>
      {brief && (
        <button type="button" className="wf-started-brief" title={open ? 'Show less' : 'Show the whole brief'} onClick={() => setOpen(o => !o)}>
          {open ? note.brief : short}
        </button>
      )}
      {traceRunId && (
        <IconButton icon={PulseIcon} variant="invisible" size="small" aria-label="Trace of the result" onClick={() => openTrace(traceRunId)} />
      )}
    </div>
  );
});
