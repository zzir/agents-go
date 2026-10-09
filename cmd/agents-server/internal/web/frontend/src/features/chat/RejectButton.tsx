import { forwardRef, useEffect, useImperativeHandle, useRef, useState } from 'react';
import { ActionList, ActionMenu, Button, ButtonGroup, IconButton, Textarea } from '@primer/react';
import { TriangleDownIcon } from '@primer/octicons-react';

interface RejectButtonProps {
  // The rejection, with the reason when one was typed.
  onReject: (reason?: string) => void;
  disabled?: boolean;
  // What is rejected when not an ordinary call: a step (the reason is kept on
  // the run's summary, no model reads it) or a plan (the reason is the model's
  // feedback).
  kind?: 'step' | 'plan';
}

// The words around the reason, per kind: the menu item that asks for one and
// what the box says it is for.
const REASON_COPY = {
  call: { ask: 'Reject with reason…', placeholder: 'Why? The model reads this — Enter rejects, Esc cancels' },
  step: { ask: 'Reject with reason…', placeholder: 'Why? Kept on the run — Enter rejects, Esc cancels' },
  plan: { ask: 'Keep planning…', placeholder: 'What should change? The model revises the plan — Enter sends, Esc cancels' },
};

// RejectHandle opens the reason box from outside — the card's `n` key.
export interface RejectHandle { ask: () => void }

// RejectButton is an approval's reject control: the plain rejection one click
// away, and behind the menu a reason — what the model reads as the rejected
// call's output. The reason box is a sibling of the buttons, so a wrapping row
// gives it its own line.
export const RejectButton = forwardRef<RejectHandle, RejectButtonProps>(function RejectButton({ onReject, disabled, kind }, ref) {
  const copy = REASON_COPY[kind || 'call'];
  const [asking, setAsking] = useState(false);
  useImperativeHandle(ref, () => ({ ask: () => { if (!disabled) setAsking(true); } }), [disabled]);
  const [reason, setReason] = useState('');
  const boxRef = useRef<HTMLTextAreaElement>(null);
  const menuRef = useRef<HTMLButtonElement>(null);
  // After the menu closes it hands focus back to its anchor; the box takes it
  // a tick later.
  useEffect(() => {
    if (!asking) return;
    const t = window.setTimeout(() => boxRef.current?.focus(), 0);
    return () => clearTimeout(t);
  }, [asking]);

  const close = () => { setAsking(false); setReason(''); };
  // Cancelling hands focus back to the control that opened the box.
  const cancel = () => { close(); menuRef.current?.focus(); };
  const send = () => {
    const typed = reason.trim();
    close();
    onReject(typed || undefined);
  };

  return (
    <>
      <ButtonGroup>
        {/* With the box open, Reject sends what was typed in it. */}
        <Button size="small" variant="danger" disabled={disabled} onClick={send}>Reject</Button>
        <ActionMenu anchorRef={menuRef}>
          <ActionMenu.Anchor>
            <IconButton icon={TriangleDownIcon} size="small" variant="danger" aria-label="More ways to reject" disabled={disabled} />
          </ActionMenu.Anchor>
          <ActionMenu.Overlay>
            <ActionList>
              <ActionList.Item onSelect={() => setAsking(true)}>{copy.ask}</ActionList.Item>
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
          placeholder={copy.placeholder}
          value={reason}
          onChange={e => setReason(e.target.value)}
          onKeyDown={e => {
            // The row around it may open on Enter or Space; typing is not that.
            e.stopPropagation();
            // An IME's Enter confirms the composition and its Escape cancels it;
            // keyCode 229 is how Safari marks the Enter after compositionend.
            if (e.nativeEvent.isComposing || e.keyCode === 229) return;
            if (e.key === 'Escape') { e.preventDefault(); cancel(); return; }
            if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send(); }
          }}
        />
      )}
    </>
  );
});
