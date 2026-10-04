import { useCallback, useMemo, memo } from 'react';
import { Button, IconButton } from '@primer/react';
import { useCopy } from '@/lib/hooks';
import { ChevronRightIcon, ChevronLeftIcon, RepoForkedIcon, CopyIcon, CheckIcon, SyncIcon, AlertIcon, StopIcon, ShieldIcon, PlayIcon } from '@primer/octicons-react';
import { Disclosure } from '@/components/Disclosure';
import { type TurnPart, type ErrorPart, type CancelledPart, type Branches } from '@/lib/timeline';
import { ERR, PER_CALL_APPROVALS } from '@/lib/protocol';
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

// SHARED_FILES_COPY is what the turn controls add on a session bound to a
// project: a fork, a regenerate and a switch of attempts all act on the one
// working tree the project's sessions share (decisions §5.28).
export const SHARED_FILES_COPY = {
  fork: "the new session shares the project's files with this one",
  regenerate: "the project's files keep what the first attempt changed",
  attempts: "attempts share the project's files",
};

// endpointTrouble recognizes the pre-flight failures a Providers edit fixes:
// no endpoint on the agent, or an endpoint it cannot reach any more. The
// messages are the runner's (bridge/runner.go, provider_resolve.go).
export function endpointTrouble(message: string): boolean {
  return /no API key configured|names provider|provider \S+: not found|provider \S+ is out of the agent's scope/i.test(message);
}

// ERROR_TITLES is the card's first line per run.error code (protocol.md,
// "Run error codes"): what failed, in words; the raw text waits in Details.
export const ERROR_TITLES: Record<string, string> = {
  [ERR.configError]: 'The agent could not be built from its configuration',
  [ERR.maxTurns]: 'The run hit its turn limit',
  [ERR.modelRefusal]: 'The model refused to answer',
  [ERR.toolLoop]: 'A tool was called in a loop',
  [ERR.toolTimeout]: 'A tool ran out of time',
  [ERR.sandboxExec]: 'The sandbox could not run the command',
  [ERR.mcp]: 'An MCP server call failed',
  [ERR.sessionBusy]: 'The session already had a run going',
  [ERR.providerError]: 'The model provider answered with an error',
  [ERR.contextOverflow]: 'The conversation no longer fits the model\'s context',
};

export function errorTitle(code?: string): string {
  return (code && ERROR_TITLES[code]) || 'The run failed';
}

interface ErrorCardProps {
  message: string;
  code?: string;
  guardrail?: string;
  stage?: string;
  onOpenProviders?: () => void;
  // Runs the turn again (the regenerate), when the turn can be.
  onRetry?: () => void;
  // Opens the trace on this run's failing span.
  onOpenSpan?: () => void;
  // Folds the session, offered when the context overflowed.
  onCompact?: () => void;
}

export function ErrorCard({ message, code, guardrail, stage, onOpenProviders, onRetry, onOpenSpan, onCompact }: ErrorCardProps) {
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
  const actions = [
    onRetry && <Button key="retry" size="small" onClick={onRetry}>Retry</Button>,
    code === ERR.contextOverflow && onCompact && <Button key="compact" size="small" onClick={onCompact}>Compact</Button>,
    onOpenProviders && endpointTrouble(message) && <Button key="providers" size="small" onClick={onOpenProviders}>Open Providers</Button>,
    onOpenSpan && <Button key="span" size="small" variant="invisible" onClick={onOpenSpan}>Open failing span</Button>,
  ].filter(Boolean);
  return (
    <Disclosure icon={AlertIcon} label={errorTitle(code)} variant="danger" className="error-card">
      <div className="error-card-details">Details</div>
      <pre className="error-card-body">{message}</pre>
      {actions.length > 0 && <div className="error-card-actions">{actions}</div>}
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
  // The run that produced the turn, when known: the trace and the replay
  // open on it.
  runId?: string;
}

export const TurnBlock = memo(function TurnBlock({ parts, streaming, reasoning, isLive, prompt, duration, messageId, branches, runId }: TurnBlockProps) {
  // Live-run state applies to the live turn only — every read below is gated
  // on isLive.
  const { running, compacting, projectBound } = useChatSession();
  const { regenerate, fork, switchBranch, openSettings, approveAll, openTrace, replayRun, compact } = useChatActions();
  // On a bound session the attempts and forks share the project's files,
  // and the controls say so (decisions §5.28).
  const shared = projectBound ? SHARED_FILES_COPY : null;
  const isEmpty = parts.length === 0 && !streaming && !reasoning;
  const { copied, copy } = useCopy();
  // A turn paused on a decision offers no fork, regenerate or attempt switch:
  // the decision is what it waits for, and a branch here would abandon it.
  const pendingCalls = parts.flatMap(p => p.type === 'tools' ? p.toolCalls.filter(tc => tc.needs_approval && !tc.status) : []);
  const awaitingDecision = pendingCalls.length > 0;
  // Two or more calls a person need not confirm one by one: one Approve all.
  const batchIds = pendingCalls.filter(tc => !PER_CALL_APPROVALS.has(tc.tool_name)).map(tc => tc.tool_call_id);

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
          : <ErrorCard key={'notice-' + i} message={part.content || 'Unknown error'} code={part.code} guardrail={part.guardrail} stage={part.stage}
              onOpenProviders={openSettings ? () => openSettings('providers') : undefined}
              onRetry={!running && canRegen ? () => regenerate!(regenEntryId!, regenContent!) : undefined}
              onOpenSpan={runId ? () => openTrace(runId) : undefined}
              onCompact={compact} />
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
      {!isLive && batchIds.length >= 2 && approveAll && (
        <div className="turn-approve-all">
          <Button size="small" variant="primary" onClick={() => approveAll(batchIds)}>Approve all ({batchIds.length})</Button>
        </div>
      )}
      {!isLive && (turnText || canRegen || (messageId && fork) || (branches && branches.tips.length > 1)) && (
        <div className="turn-actions">
          {!awaitingDecision && branches && branches.tips.length > 1 && switchBranch && (
            <span className="branch-switcher">
              <IconButton
                icon={ChevronLeftIcon}
                variant="invisible"
                size="small"
                aria-label={shared ? 'Previous attempt — ' + shared.attempts : 'Previous attempt'}
                disabled={running || branches.active === 0}
                onClick={() => switchBranch(branches.tips[branches.active - 1])}
              />
              <span className="branch-count">{branches.active + 1} / {branches.tips.length}</span>
              <IconButton
                icon={ChevronRightIcon}
                variant="invisible"
                size="small"
                aria-label={shared ? 'Next attempt — ' + shared.attempts : 'Next attempt'}
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
          {!awaitingDecision && messageId && fork && (
            <IconButton
              icon={RepoForkedIcon}
              variant="invisible"
              size="small"
              aria-label={shared ? 'Fork — ' + shared.fork : 'Fork'}
              onClick={() => fork(String(messageId))}
            />
          )}
          {!running && !awaitingDecision && canRegen && (
            <IconButton
              icon={SyncIcon}
              variant="invisible"
              size="small"
              aria-label={shared ? 'Regenerate — ' + shared.regenerate : 'Regenerate'}
              onClick={() => regenerate!(regenEntryId!, regenContent!)}
            />
          )}
          {runId && replayRun && (
            <IconButton
              icon={PlayIcon}
              variant="invisible"
              size="small"
              aria-label="Replay…"
              onClick={() => replayRun(runId)}
            />
          )}
          {duration && <span className="turn-duration">{duration}</span>}
        </div>
      )}
    </div>
  );
});
