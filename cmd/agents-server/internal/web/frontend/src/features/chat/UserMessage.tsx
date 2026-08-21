import { useCallback, memo } from 'react';
import { IconButton } from '@primer/react';
import { useCopy } from '@/lib/hooks';
import { PulseIcon, CopyIcon, CheckIcon } from '@primer/octicons-react';
import { parseTaskNotification } from '@/lib/protocol';
import { useChatActions } from '@/features/chat/ChatSessionContext';

interface UserMessageProps {
  content: string;
  traceRunId?: string | null;
  msgIdx: number;
  // The durable entry id, so the Context panel can scroll to this bubble.
  entryId?: string;
}

export const UserMessage = memo(function UserMessage({ content, traceRunId, msgIdx, entryId }: UserMessageProps) {
  const { openTrace } = useChatActions();
  const { copied, copy } = useCopy();

  const handleCopy = useCallback(() => {
    if (content) copy(content);
  }, [content, copy]);

  // A server-injected notification (a finished task or workflow) never renders
  // in the timeline: the model reads it verbatim, but for the person the
  // composer's indicators and the Tasks panel are the surfaces — an in-flow
  // card duplicated them mid-conversation.
  if (parseTaskNotification(content)) return null;

  return (
    <div className="message message-user message-forkable" data-run-id={traceRunId || undefined} data-msg-idx={msgIdx} data-anchor-id={entryId || undefined}>
      <div className="message-body">{content}</div>
      <div className="message-user-actions">
        {traceRunId && (
          <IconButton
            icon={PulseIcon}
            variant="invisible"
            size="small"
            aria-label="Trace"
            onClick={() => openTrace(traceRunId)}
          />
        )}
        <IconButton
          icon={copied ? CheckIcon : CopyIcon}
          variant="invisible"
          size="small"
          aria-label={copied ? 'Copied!' : 'Copy'}
          onClick={handleCopy}
          style={copied ? { color: 'var(--fgColor-success)' } : undefined}
        />
      </div>
    </div>
  );
});
