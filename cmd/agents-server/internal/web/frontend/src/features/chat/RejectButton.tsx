import { useEffect, useRef, useState, type ReactElement } from 'react';
import { ActionList, ActionMenu, Button, ButtonGroup, IconButton, Textarea } from '@primer/react';
import { TriangleDownIcon } from '@primer/octicons-react';

interface RejectButtonProps {
  // The rejection, with the reason when one was typed.
  onReject: (reason?: string) => void;
  disabled?: boolean;
  // A workflow step waiting to start: its rejection ends the run, so the
  // reason is kept on the run's summary and no model reads it.
  step?: boolean;
}

// RejectButton is an approval's reject control: the plain rejection one click
// away, and behind the menu a reason — what the model reads as the rejected
// call's output. The reason box is a sibling of the buttons, so a wrapping
// row gives it a line of its own.
export function RejectButton({ onReject, disabled, step }: RejectButtonProps): ReactElement {
  const [asking, setAsking] = useState(false);
  const [reason, setReason] = useState('');
  const boxRef = useRef<HTMLTextAreaElement>(null);
  // After the menu closes it hands focus back to its anchor; the box takes it
  // a tick later.
  useEffect(() => {
    if (!asking) return;
    const t = window.setTimeout(() => boxRef.current?.focus(), 0);
    return () => clearTimeout(t);
  }, [asking]);

  const close = () => { setAsking(false); setReason(''); };
  const send = () => {
    const typed = reason.trim();
    close();
    onReject(typed || undefined);
  };

  return (
    <>
      <ButtonGroup>
        <Button size="small" variant="danger" disabled={disabled} onClick={() => onReject()}>Reject</Button>
        <ActionMenu>
          <ActionMenu.Anchor>
            <IconButton icon={TriangleDownIcon} size="small" variant="danger" aria-label="More ways to reject" disabled={disabled} />
          </ActionMenu.Anchor>
          <ActionMenu.Overlay>
            <ActionList>
              <ActionList.Item onSelect={() => setAsking(true)}>Reject with reason…</ActionList.Item>
            </ActionList>
          </ActionMenu.Overlay>
        </ActionMenu>
      </ButtonGroup>
      {asking && (
        <Textarea
          ref={boxRef}
          className="reject-reason"
          block
          rows={2}
          resize="vertical"
          aria-label="Reason for rejecting"
          placeholder={(step ? 'Why? Kept on the run' : 'Why? The model reads this') + ' — Enter rejects, Esc cancels'}
          value={reason}
          onChange={e => setReason(e.target.value)}
          onKeyDown={e => {
            // The row around it may open on Enter or Space; typing is not that.
            e.stopPropagation();
            if (e.key === 'Escape') { e.preventDefault(); close(); return; }
            // An IME's Enter confirms the composition, never the rejection.
            if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) { e.preventDefault(); send(); }
          }}
        />
      )}
    </>
  );
}
