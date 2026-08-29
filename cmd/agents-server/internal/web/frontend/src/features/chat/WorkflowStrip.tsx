import { useEffect, useState } from 'react';
import { Button, Label } from '@primer/react';
import { WorkflowIcon } from '@primer/octicons-react';
import { useChatActions, useChatBackground } from '@/features/chat/ChatSessionContext';
import { useDecisionHold } from '@/features/chat/useDecisionHold';
import { STEP_APPROVAL_TOOL } from '@/lib/protocol';
import './workflow.css';

// A running step can sit for minutes on a slow model; a live elapsed clock says
// "still working" so the thin bar doesn't read as stuck.
function elapsed(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`;
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
export function WorkflowStrip() {
  const items = useChatBackground();
  const { approve, reject, inspectTask, stopTask, retryTask, dismissTask } = useChatActions();
  // One flag per execution with a request in flight: two bars worked at once
  // must not free each other's buttons.
  const [busy, setBusy] = useState<Set<string>>(() => new Set());
  // Every hook before the early return: the strip renders empty most of the
  // time, and a hook that only ran once a sequence appeared would change the
  // hook order between those two renders (React #310).
  const { held, decide } = useDecisionHold();
  const live = items.filter(it => it.kind === 'workflow' && it.status !== 'completed' && it.status !== 'cancelled' && !it.dismissed);
  // A once-a-second tick drives the elapsed clock — only while a step is
  // actually running, so a paused or empty strip keeps no timer alive.
  const [nowMs, setNowMs] = useState(() => Date.now());
  const anyRunning = live.some(it => it.status === 'working');
  useEffect(() => {
    if (!anyRunning) return;
    const h = setInterval(() => setNowMs(Date.now()), 1000);
    return () => clearInterval(h);
  }, [anyRunning]);
  if (live.length === 0) return null;

  const act = async (id: string, fn: () => Promise<void>) => {
    setBusy(prev => new Set(prev).add(id));
    try {
      await fn();
    } finally {
      setBusy(prev => { const next = new Set(prev); next.delete(id); return next; });
    }
  };

  return (
    <>
      {live.map(it => (
        <div key={it.id} className="wf-bar" role="button" tabIndex={0}
          onClick={() => inspectTask(it.id)}
          onKeyDown={e => { if (e.key === 'Enter') inspectTask(it.id); }}>
          <WorkflowIcon size={14} />
          <span className="wf-bar-name">{it.label}</span>
          {it.status === 'failed' ? (
            <>
              <Label variant="danger">failed</Label>
              <span className="wf-bar-step" title={it.error}>
                {it.activity}{it.error ? ' — ' + it.error : ''}
              </span>
              <span className="wf-bar-actions" onClick={e => e.stopPropagation()}>
                {/* Only when the server would take it: attempts left, and no
                    bound (budget, step ceiling) that refuses a retry outright. */}
                {it.retryable && (
                  <Button size="small" variant="invisible" disabled={busy.has(it.id)} onClick={() => act(it.id, () => retryTask(it.id))}>
                    Retry from here
                  </Button>
                )}
                <Button size="small" variant="invisible" disabled={busy.has(it.id)} onClick={() => act(it.id, () => dismissTask(it.id))}>
                  Dismiss
                </Button>
              </span>
            </>
          ) : it.status === 'input_required' && it.pendingCallId ? (
            <>
              <span className="wf-bar-step">
                {it.pendingToolName === STEP_APPROVAL_TOOL
                  ? <>{it.activity} is waiting to start — run it?</>
                  : <>{it.activity} needs your decision: <code>{it.pendingToolName}</code></>}
              </span>
              <span className="wf-bar-actions" onClick={e => e.stopPropagation()}>
                <Button size="small" variant="primary" disabled={busy.has(it.id) || held(it.pendingCallId)}
                  onClick={() => decide(it.pendingCallId!, () => approve?.(it.pendingCallId!))}>Approve</Button>
                <Button size="small" variant="danger" disabled={busy.has(it.id) || held(it.pendingCallId)}
                  onClick={() => decide(it.pendingCallId!, () => reject?.(it.pendingCallId!))}>Reject</Button>
              </span>
            </>
          ) : (
            <>
              <span className="wf-bar-step">{it.activity}</span>
              <span className="wf-bar-track">
                <span className="wf-bar-fill" style={{ width: `${(it.progress || 0) * 100}%` }} />
              </span>
              {it.status === 'working' && it.createdAt && (
                <span className="wf-bar-elapsed" title="Elapsed since this sequence started">{elapsed(nowMs - it.createdAt)}</span>
              )}
              <span className="wf-bar-actions" onClick={e => e.stopPropagation()}>
                <Button size="small" variant="invisible" disabled={busy.has(it.id)} onClick={() => act(it.id, () => stopTask(it.id))}>
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
