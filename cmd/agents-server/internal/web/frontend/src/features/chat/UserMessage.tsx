import { useCallback, useEffect, useRef, useState, memo } from 'react';
import { Button, IconButton, Textarea } from '@primer/react';
import { useCopy } from '@/lib/hooks';
import { PulseIcon, CopyIcon, CheckIcon, PencilIcon, ChevronLeftIcon, ChevronRightIcon } from '@primer/octicons-react';
import { parseTaskNotification } from '@/lib/protocol';
import { canEditMessage } from '@/lib/editResend';
import type { Branches } from '@/lib/timeline';
import { useChatActions, useChatSession } from '@/features/chat/ChatSessionContext';
import { ZoomOverlay } from '@/features/chat/ZoomOverlay';
import type { AttachmentMeta } from '@/lib/attachments';

interface UserMessageProps {
  content: string;
  attachments?: AttachmentMeta[];
  traceRunId?: string | null;
  msgIdx: number;
  // The durable entry and its parent: what an edit branches at.
  entryId?: string;
  parentId?: string;
  // Sibling attempts here — the message as edited and resent.
  branches?: Branches;
}

export const UserMessage = memo(function UserMessage({ content, attachments, traceRunId, msgIdx, entryId, parentId, branches }: UserMessageProps) {
  const { openTrace, editResend, switchBranch } = useChatActions();
  const { running, pendingDecision } = useChatSession();
  const { copied, copy } = useCopy();
  const [zoomed, setZoomed] = useState<AttachmentMeta | null>(null);
  // Editing happens in place; nothing reaches the server until Send.
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(content);
  const boxRef = useRef<HTMLTextAreaElement>(null);
  useEffect(() => { if (editing) boxRef.current?.focus(); }, [editing]);

  const handleCopy = useCallback(() => {
    if (content) copy(content);
  }, [content, copy]);
  const beginEdit = () => { setDraft(content); setEditing(true); };
  const cancelEdit = () => setEditing(false);
  const sendEdit = () => {
    const text = draft.trim();
    if (!text || !editResend || !entryId || !parentId) return;
    setEditing(false);
    editResend(entryId, parentId, text, attachments?.map(a => a.id));
  };

  // A server-injected notification (a finished task or workflow) never renders
  // in the timeline: the model reads it verbatim, but for the person the
  // composer's indicators and the Tasks panel are the surfaces — an in-flow
  // card duplicated them mid-conversation.
  if (parseTaskNotification(content)) return null;

  const editable = !!editResend && canEditMessage({ entryId, parentId, content }, { running, pendingDecision: !!pendingDecision });

  return (
    <div className="message message-user message-forkable" data-run-id={traceRunId || undefined} data-msg-idx={msgIdx}>
      {attachments && attachments.length > 0 && (
        <div className="message-attachments">
          {attachments.map(a => (
            <img key={a.id} src={a.url} alt="" loading="lazy" onClick={() => setZoomed(a)} />
          ))}
        </div>
      )}
      {zoomed && (
        <ZoomOverlay onClose={() => setZoomed(null)}>
          <img src={zoomed.url} alt="" style={{ maxWidth: '90vw', maxHeight: '90vh' }} />
        </ZoomOverlay>
      )}
      {editing ? (
        <div className="message-edit">
          <Textarea
            ref={boxRef}
            block
            rows={3}
            resize="vertical"
            aria-label="Edit message"
            value={draft}
            onChange={e => setDraft(e.target.value)}
            onKeyDown={e => {
              if (e.nativeEvent.isComposing || e.keyCode === 229) return;
              if (e.key === 'Escape') { e.preventDefault(); cancelEdit(); return; }
              if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); sendEdit(); }
            }}
          />
          <div className="message-edit-actions">
            <Button size="small" variant="primary" onClick={sendEdit} disabled={!draft.trim()}>Send</Button>
            <Button size="small" onClick={cancelEdit}>Cancel</Button>
            <span className="message-edit-hint">Sends as a new branch; the first attempt stays in the switcher.</span>
          </div>
        </div>
      ) : (
        content && <div className="message-body">{content}</div>
      )}
      <div className="message-user-actions">
        {branches && branches.tips.length > 1 && switchBranch && (
          <span className="branch-switcher">
            <IconButton
              icon={ChevronLeftIcon}
              variant="invisible"
              size="small"
              aria-label="Previous attempt"
              disabled={running || branches.active === 0}
              onClick={() => switchBranch(branches.tips[branches.active - 1])}
            />
            <span className="branch-count">{branches.active + 1} / {branches.tips.length}</span>
            <IconButton
              icon={ChevronRightIcon}
              variant="invisible"
              size="small"
              aria-label="Next attempt"
              disabled={running || branches.active >= branches.tips.length - 1}
              onClick={() => switchBranch(branches.tips[branches.active + 1])}
            />
          </span>
        )}
        {editable && !editing && (
          <IconButton
            icon={PencilIcon}
            variant="invisible"
            size="small"
            aria-label="Edit — resend from here as a new branch"
            onClick={beginEdit}
          />
        )}
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
