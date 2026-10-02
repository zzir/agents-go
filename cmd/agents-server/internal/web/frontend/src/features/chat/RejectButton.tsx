import { useEffect, useRef, useState, type ReactElement } from 'react';
import { ActionList, ActionMenu, Button, ButtonGroup, IconButton, Textarea } from '@primer/react';
import { TriangleDownIcon } from '@primer/octicons-react';

interface RejectButtonProps {
  // The rejection, with the reason when one was typed.
  onReject: (reason?: string) => void;
  disabled?: boolean;
  // What is being rejected, when it is not an ordinary tool call. A workflow
  // step waiting to start: its rejection ends the run, so the reason is kept
  // on the run's summary and no model reads it. A submitted plan: the reason
  // is the feedback the model revises the plan from.
  kind?: 'step' | 'plan';
}

// The words around the reason, per kind: the menu item that asks for one and
// what the box says it is for.
const REASON_COPY = {
  call: { ask: 'Reject with reason…', placeholder: 'Why? The model reads this — Enter rejects, Esc cancels' },
  step: { ask: 'Reject with reason…', placeholder: 'Why? Kept on the run — Enter rejects, Esc cancels' },
  plan: { ask: 'Keep planning…', placeholder: 'What should change? The model revises the plan — Enter sends, Esc cancels' },
};

// RejectButton is an approval's reject control: the plain rejection one click
// away, and behind the menu a reason — what the model reads as the rejected
// call's output. The reason box is a sibling of the buttons, so a wrapping
// row gives it a line of its own.
export function RejectButton({ onReject, disabled, kind }: RejectButtonProps): ReactElement {
  const copy = REASON_COPY[kind || 'call'];
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
            if (e.key === 'Escape') { e.preventDefault(); close(); return; }
            // An IME's Enter confirms the composition, never the rejection.
            if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) { e.preventDefault(); send(); }
          }}
        />
      )}
    </>
  );
}
