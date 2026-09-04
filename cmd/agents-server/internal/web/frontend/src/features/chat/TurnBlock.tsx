import { useCallback, useMemo, memo } from 'react';
import { IconButton } from '@primer/react';
import { useCopy } from '@/lib/hooks';
import { ChevronRightIcon, ChevronLeftIcon, RepoForkedIcon, CopyIcon, CheckIcon, SyncIcon, AlertIcon, StopIcon, ShieldIcon } from '@primer/octicons-react';
import { Disclosure } from '@/components/Disclosure';
import { type TurnPart, type ErrorPart, type CancelledPart, type Branches } from '@/lib/timeline';
import { StreamingMarkdown } from '@/features/chat/StreamingMarkdown';
import { TextContent } from '@/features/chat/TextContent';
import { ProcessTimeline } from '@/features/chat/ProcessTimeline';
import { useChatSession, useChatActions } from '@/features/chat/ChatSessionContext';

// STAGE_NOTES says what a trip at each stage actually stopped. A guardrail runs
// at four of them, and telling someone "the request was blocked before the
// model ran" when a tool result tripped it describes the wrong event.
const STAGE_NOTES: Record<string, string> = {
  input: 'The request was blocked before the model ran.',
  output: 'The response above was blocked before delivery.',
  tool_input: 'A tool call was blocked before the tool ran.',
  tool_output: "A tool's result was blocked before the model could read it.",
};

function ErrorCard({ message, guardrail, stage }: { message: string; guardrail?: string; stage?: string }) {
  // A guardrail block is not a system failure — render it as a distinct
  // "blocked" state.
  if (guardrail) {
    const label = `Blocked by guardrail “${guardrail}”`;
    const note = STAGE_NOTES[stage || ''] || 'The run was blocked by a guardrail.';
    return (
      <Disclosure icon={ShieldIcon} label={label} variant="attention" className="error-card">
        <pre className="error-card-body">{note + '\n\n' + message}</pre>
      </Disclosure>
    );
  }
  return (
    <Disclosure icon={AlertIcon} label="Error" variant="danger" className="error-card">
      <pre className="error-card-body">{message}</pre>
    </Disclosure>
  );
}

function CancelledCard() {
  return (
    <div className="cancelled-card">
      <StopIcon size={16} className="cancelled-card-icon" />
      <span>Run cancelled</span>
    </div>
  );
}

// Group a turn's parts into render segments: every text part is assistant
// prose said to the user — interim narration and final answer alike — and
// renders flat in chronological order; each unbroken run of thinking/tools
// parts between texts collapses into one process group. Notices (errors,
// cancellation) render separately at the end. Empty texts are dropped without
// splitting the group around them.
type TurnSegment =
  | { kind: 'text'; content: string }
  | { kind: 'process'; parts: TurnPart[] };

function buildSegments(parts: TurnPart[]): { segments: TurnSegment[]; notices: (ErrorPart | CancelledPart)[] } {
  const segments: TurnSegment[] = [];
  const notices: (ErrorPart | CancelledPart)[] = [];
  for (const p of parts) {
    if (p.type === 'error' || p.type === 'cancelled') { notices.push(p); continue; }
    if (p.type === 'text') {
      if (p.content) segments.push({ kind: 'text', content: p.content });
      continue;
    }
    const last = segments[segments.length - 1];
    if (last?.kind === 'process') last.parts.push(p);
    else segments.push({ kind: 'process', parts: [p] });
  }
  return { segments, notices };
}

interface TurnBlockProps {
  parts: TurnPart[];
  // Per-delta live text, set on the ONE live turn only (null elsewhere), so a
  // delta re-renders that turn and no other — the memo boundary below.
  streaming: string | null;
  reasoning: string | null;
  isLive: boolean;
  // The user message this turn answers, or null. Regenerating branches back to
  // its ENTRY id (not a row id) and runs again.
  prompt: { entryId?: string; content?: string } | null;
  duration?: string;
  messageId?: string | number;
  // Sibling attempts at this point.
  branches?: Branches;
}

export const TurnBlock = memo(function TurnBlock({ parts, streaming, reasoning, isLive, prompt, duration, messageId, branches }: TurnBlockProps) {
  // Live-run state applies to the live turn only — every read below is gated
  // on isLive.
  const { running, compacting } = useChatSession();
  const { regenerate, fork, switchBranch } = useChatActions();
  const isEmpty = parts.length === 0 && !streaming && !reasoning;
  const { copied, copy } = useCopy();

  const { segments, notices } = useMemo(() => buildSegments(parts), [parts]);

  // While live, the trailing process group is the one still executing — live
  // reasoning and the status label attach there; earlier groups have settled.
  // When the trailing segment is text (or the turn is empty) but reasoning is
  // already streaming, a tail group holds it until the next part arrives.
  const lastSeg = segments[segments.length - 1];
  const activeIdx = isLive && lastSeg?.kind === 'process' ? segments.length - 1 : -1;
  const liveTail = isLive && activeIdx === -1 && !!reasoning;

  const turnText = useMemo(
    () => parts.flatMap(p => p.type === 'text' ? [p.content] : []).join('\n\n'),
    [parts],
  );

  const handleCopy = useCallback(() => {
    if (turnText) copy(turnText);
  }, [turnText, copy]);

  const regenEntryId = prompt?.entryId;
  const regenContent = prompt?.content;
  const canRegen = !!(regenEntryId && regenContent && regenerate);

  return (
    <div className="message message-turn">
      {segments.map((seg, i) =>
        seg.kind === 'text'
          ? <TextContent key={'seg-' + i} content={seg.content} />
          : <ProcessTimeline
              key={'seg-' + i}
              parts={seg.parts}
              live={i === activeIdx}
              reasoning={i === activeIdx ? reasoning : null}
              textStreaming={i === activeIdx && !!streaming}
            />
      )}
      {liveTail && (
        <ProcessTimeline parts={[]} live reasoning={reasoning} textStreaming={!!streaming} />
      )}
      {streaming && <StreamingMarkdown text={streaming} />}
      {notices.map((part, i) => (
        part.type === 'cancelled'
          ? <CancelledCard key={'notice-' + i} />
          : <ErrorCard key={'notice-' + i} message={part.content || 'Unknown error'} guardrail={part.guardrail} stage={part.stage} />
      ))}
      {isLive && isEmpty && !compacting && (
        <div className="thinking-indicator">
          <div className="thinking-dots">
            <span /><span /><span />
          </div>
        </div>
      )}
      {isLive && compacting && activeIdx === -1 && !liveTail && (
        <div className="thinking-indicator">
          <div className="thinking-dots">
            <span /><span /><span />
          </div>
          <span className="thinking-agent">Compacting context…</span>
        </div>
      )}
      {/* The bar shows for anything a person can act on: a failed or
          cancelled turn with no assistant text still regenerates, forks and
          switches attempts — only Copy needs text. */}
      {!isLive && (turnText || canRegen || (messageId && fork) || (branches && branches.tips.length > 1)) && (
        <div className="turn-actions">
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
          {turnText && (
            <IconButton
              icon={copied ? CheckIcon : CopyIcon}
              variant="invisible"
              size="small"
              aria-label={copied ? 'Copied!' : 'Copy'}
              onClick={handleCopy}
              style={copied ? { color: 'var(--fgColor-success)' } : undefined}
            />
          )}
          {messageId && fork && (
            <IconButton
              icon={RepoForkedIcon}
              variant="invisible"
              size="small"
              aria-label="Fork"
              onClick={() => fork(String(messageId))}
            />
          )}
          {!running && canRegen && (
            <IconButton
              icon={SyncIcon}
              variant="invisible"
              size="small"
              aria-label="Regenerate"
              onClick={() => regenerate!(regenEntryId!, regenContent!)}
            />
          )}
          {duration && <span className="turn-duration">{duration}</span>}
        </div>
      )}
    </div>
  );
});
