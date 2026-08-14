import { useState } from 'react';
import { Button, Label } from '@primer/react';
import { WorkflowIcon } from '@primer/octicons-react';
import type { BackgroundItem } from '@/lib/background';
import './workflow.css';

interface WorkflowStripProps {
  items: BackgroundItem[];
  onOpen: (id: string) => void;
  onApprove?: (toolCallId: string) => void;
  onReject?: (toolCallId: string) => void;
  onStop: (item: BackgroundItem) => Promise<void>;
  onRetry: (item: BackgroundItem) => Promise<void>;
  onDismiss: (item: BackgroundItem) => Promise<void>;
}

// WorkflowStrip is what a background sequence looks like from the conversation
// that asked for it: how far it has got, and — the part that cannot be left out
// — the decision it is waiting on. A step pauses for approval inside a session
// nobody can open, so without this the sequence waits forever on a question
// nobody can see.
//
// A finished sequence shows nothing here — its result arrives as a turn, and
// the Tasks panel keeps the record. A dismissed failure is likewise the
// panel's alone.
export function WorkflowStrip({ items, onOpen, onApprove, onReject, onStop, onRetry, onDismiss }: WorkflowStripProps) {
  const [busy, setBusy] = useState(false);
  const live = items.filter(it => it.kind === 'workflow' && it.status !== 'completed' && it.status !== 'cancelled' && !it.dismissed);
  if (live.length === 0) return null;

  const act = async (fn: () => Promise<void>) => {
    setBusy(true);
    try {
      await fn();
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      {live.map(it => (
        <div key={it.id} className="wf-bar" role="button" tabIndex={0}
          onClick={() => onOpen(it.id)}
          onKeyDown={e => { if (e.key === 'Enter') onOpen(it.id); }}>
          <WorkflowIcon size={14} />
          <span className="wf-bar-name">{it.label}</span>
          {it.status === 'failed' ? (
            <>
              <Label variant="danger">failed</Label>
              <span className="wf-bar-step" title={it.error}>
                {it.activity}{it.error ? ' — ' + it.error : ''}
              </span>
              <span className="wf-bar-actions" onClick={e => e.stopPropagation()}>
                <Button size="small" variant="invisible" disabled={busy} onClick={() => act(() => onRetry(it))}>
                  Retry from here
                </Button>
                <Button size="small" variant="invisible" disabled={busy} onClick={() => act(() => onDismiss(it))}>
                  Dismiss
                </Button>
              </span>
            </>
          ) : it.status === 'input_required' && it.pendingCallId ? (
            <>
              <span className="wf-bar-step">
                {it.activity} needs your decision: <code>{it.pendingToolName}</code>
              </span>
              <span className="wf-bar-actions" onClick={e => e.stopPropagation()}>
                <Button size="small" variant="primary" disabled={busy}
                  onClick={() => onApprove?.(it.pendingCallId!)}>Approve</Button>
                <Button size="small" variant="danger" disabled={busy}
                  onClick={() => onReject?.(it.pendingCallId!)}>Reject</Button>
              </span>
            </>
          ) : (
            <>
              <span className="wf-bar-step">{it.activity}</span>
              <span className="wf-bar-track">
                <span className="wf-bar-fill" style={{ width: `${(it.progress || 0) * 100}%` }} />
              </span>
              <span className="wf-bar-actions" onClick={e => e.stopPropagation()}>
                <Button size="small" variant="invisible" disabled={busy} onClick={() => act(() => onStop(it))}>
                  Stop
                </Button>
              </span>
            </>
          )}
        </div>
      ))}
    </>
  );
}
